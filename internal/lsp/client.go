package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/isaacphi/mcp-language-server/internal/protocol"
)

type Client struct {
	Cmd   *exec.Cmd
	stdin io.WriteCloser
	// writeMu serializes every message written to stdin
	writeMu sync.Mutex
	stdout  *bufio.Reader
	stderr  io.ReadCloser

	// Request ID counter
	nextID atomic.Int32

	// Response handlers
	handlers   map[string]chan *Message
	handlersMu sync.RWMutex

	// Server request handlers
	serverRequestHandlers map[string]ServerRequestHandler
	serverHandlersMu      sync.RWMutex

	// Notification handlers
	notificationHandlers map[string]NotificationHandler
	notificationMu       sync.RWMutex

	// Diagnostic cache
	diagnostics   map[protocol.DocumentUri][]protocol.Diagnostic
	diagnosticsMu sync.RWMutex

	// Files are currently opened by the LSP
	openFiles   map[string]*OpenFileInfo
	openFilesMu sync.RWMutex

	// Serializes temporary worktree overlays onto canonical workspace URIs
	worktreeOverlayMu sync.Mutex
}

func NewClient(command string, args ...string) (*Client, error) {
	cmd := exec.Command(command, args...)
	// Copy env
	cmd.Env = os.Environ()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	client := &Client{
		Cmd:                   cmd,
		stdin:                 stdin,
		stdout:                bufio.NewReader(stdout),
		stderr:                stderr,
		handlers:              make(map[string]chan *Message),
		notificationHandlers:  make(map[string]NotificationHandler),
		serverRequestHandlers: make(map[string]ServerRequestHandler),
		diagnostics:           make(map[protocol.DocumentUri][]protocol.Diagnostic),
		openFiles:             make(map[string]*OpenFileInfo),
	}

	// Start the LSP server process
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start LSP server: %w", err)
	}

	// Handle stderr in a separate goroutine with proper logging
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			processLogger.Info("%s", line)
		}
		if err := scanner.Err(); err != nil {
			lspLogger.Error("Error reading LSP server stderr: %v", err)
		}
	}()

	// Start message handling loop
	go client.handleMessages()

	return client, nil
}

func (c *Client) RegisterNotificationHandler(method string, handler NotificationHandler) {
	c.notificationMu.Lock()
	defer c.notificationMu.Unlock()
	c.notificationHandlers[method] = handler
}

func (c *Client) RegisterServerRequestHandler(method string, handler ServerRequestHandler) {
	c.serverHandlersMu.Lock()
	defer c.serverHandlersMu.Unlock()
	c.serverRequestHandlers[method] = handler
}

func (c *Client) InitializeLSPClient(ctx context.Context, workspaceDir string) (*protocol.InitializeResult, error) {
	initParams := &protocol.InitializeParams{
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: []protocol.WorkspaceFolder{
				{
					URI:  protocol.URI("file://" + workspaceDir),
					Name: workspaceDir,
				},
			},
		},

		XInitializeParams: protocol.XInitializeParams{
			ProcessID: int32(os.Getpid()),
			ClientInfo: &protocol.ClientInfo{
				Name:    "mcp-language-server",
				Version: "0.1.0",
			},
			RootPath: workspaceDir,
			RootURI:  protocol.DocumentUri("file://" + workspaceDir),
			Capabilities: protocol.ClientCapabilities{
				Workspace: protocol.WorkspaceClientCapabilities{
					Configuration: true,
					DidChangeConfiguration: protocol.DidChangeConfigurationClientCapabilities{
						DynamicRegistration: true,
					},
					DidChangeWatchedFiles: protocol.DidChangeWatchedFilesClientCapabilities{
						DynamicRegistration:    true,
						RelativePatternSupport: true,
					},
				},
				TextDocument: protocol.TextDocumentClientCapabilities{
					Synchronization: &protocol.TextDocumentSyncClientCapabilities{
						DynamicRegistration: true,
						DidSave:             true,
					},
					Completion: protocol.CompletionClientCapabilities{
						CompletionItem: protocol.ClientCompletionItemOptions{},
					},
					CodeLens: &protocol.CodeLensClientCapabilities{
						DynamicRegistration: true,
					},
					DocumentSymbol: protocol.DocumentSymbolClientCapabilities{},
					CodeAction: protocol.CodeActionClientCapabilities{
						CodeActionLiteralSupport: protocol.ClientCodeActionLiteralOptions{
							CodeActionKind: protocol.ClientCodeActionKindOptions{
								ValueSet: []protocol.CodeActionKind{},
							},
						},
					},
					PublishDiagnostics: protocol.PublishDiagnosticsClientCapabilities{
						VersionSupport: true,
					},
					SemanticTokens: protocol.SemanticTokensClientCapabilities{
						Requests: protocol.ClientSemanticTokensRequestOptions{
							Range: &protocol.Or_ClientSemanticTokensRequestOptions_range{},
							Full:  &protocol.Or_ClientSemanticTokensRequestOptions_full{},
						},
						TokenTypes:     []string{},
						TokenModifiers: []string{},
						Formats:        []protocol.TokenFormat{},
					},
				},
				Window: protocol.WindowClientCapabilities{},
			},
			InitializationOptions: defaultInitializationOptions(),
		},
	}

	var result protocol.InitializeResult
	if err := c.Call(ctx, "initialize", initParams, &result); err != nil {
		return nil, fmt.Errorf("initialize failed: %w", err)
	}

	if err := c.Notify(ctx, "initialized", struct{}{}); err != nil {
		return nil, fmt.Errorf("initialized notification failed: %w", err)
	}

	// Register handlers
	c.RegisterServerRequestHandler("workspace/applyEdit", HandleApplyEdit)
	c.RegisterServerRequestHandler("workspace/configuration", HandleWorkspaceConfiguration)
	c.RegisterServerRequestHandler("client/registerCapability", HandleRegisterCapability)
	c.RegisterNotificationHandler("window/showMessage", HandleServerMessage)
	c.RegisterNotificationHandler("textDocument/publishDiagnostics",
		func(params json.RawMessage) { HandleDiagnostics(c, params) })

	// Notify the LSP server
	err := c.Initialized(ctx, protocol.InitializedParams{})
	if err != nil {
		return nil, fmt.Errorf("initialization failed: %w", err)
	}

	// LSP sepecific Initialization
	path := strings.ToLower(c.Cmd.Path)
	switch {
	case strings.Contains(path, "typescript-language-server"):
		err := initializeTypescriptLanguageServer(ctx, c, workspaceDir)
		if err != nil {
			return nil, err
		}
	}

	return &result, nil
}

func (c *Client) Close() error {
	// Try to close all open files first
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Attempt to close files but continue shutdown regardless
	c.CloseAllFiles(ctx)

	// Force kill the LSP process if it doesn't exit within timeout
	forcedKill := make(chan struct{})
	go func() {
		select {
		case <-time.After(2 * time.Second):
			lspLogger.Warn("LSP process did not exit within timeout, forcing kill")
			if c.Cmd.Process != nil {
				if err := c.Cmd.Process.Kill(); err != nil {
					lspLogger.Error("Failed to kill process: %v", err)
				} else {
					lspLogger.Info("Process killed successfully")
				}
			}
			close(forcedKill)
		case <-forcedKill:
			// Channel closed from completion path
			return
		}
	}()

	// Close stdin to signal the server
	if err := c.stdin.Close(); err != nil {
		lspLogger.Error("Failed to close stdin: %v", err)
	}

	// Wait for process to exit
	err := c.Cmd.Wait()
	close(forcedKill) // Stop the force kill goroutine

	return err
}

type ServerState int

const (
	StateStarting ServerState = iota
	StateReady
	StateError
)

func (c *Client) WaitForServerReady(ctx context.Context) error {
	// TODO: wait for specific messages or poll workspace/symbol
	time.Sleep(time.Second * 1)
	return nil
}

type OpenFileInfo struct {
	Version int32
	URI     protocol.DocumentUri
}

func defaultInitializationOptions() any {
	if raw := os.Getenv("MCP_LSP_INIT_OPTIONS"); raw != "" {
		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			return parsed
		}
		lspLogger.Warn("Failed to parse MCP_LSP_INIT_OPTIONS JSON, falling back to defaults")
	}
	return map[string]any{
		"codelenses": map[string]bool{
			"generate":           true,
			"regenerate_cgo":     true,
			"test":               true,
			"tidy":               true,
			"upgrade_dependency": true,
			"vendor":             true,
			"vulncheck":          false,
		},
	}
}

func MapWorktreePath(filePath string) (canonicalPath, wtRoot, repoRoot string, isWorktree bool) {
	const marker = "/.worktrees/"
	idx := strings.Index(filePath, marker)
	if idx < 0 {
		return filePath, "", "", false
	}
	repoRoot = filePath[:idx]
	rest := filePath[idx+len(marker):]
	slashIdx := strings.IndexByte(rest, '/')
	if slashIdx <= 0 {
		return filePath, "", "", false
	}
	wtName := rest[:slashIdx]
	relPath := rest[slashIdx+1:]
	wtRoot = repoRoot + marker + wtName
	canonicalPath = repoRoot + "/" + relPath
	return canonicalPath, wtRoot, repoRoot, true
}

func (c *Client) OpenOrUpdateWithContent(ctx context.Context, targetPath string, content string) error {
	uri := fmt.Sprintf("file://%s", targetPath)

	c.openFilesMu.Lock()
	fileInfo, isOpen := c.openFiles[uri]
	if isOpen {
		fileInfo.Version++
		version := fileInfo.Version
		c.openFilesMu.Unlock()

		params := protocol.DidChangeTextDocumentParams{
			TextDocument: protocol.VersionedTextDocumentIdentifier{
				TextDocumentIdentifier: protocol.TextDocumentIdentifier{
					URI: protocol.DocumentUri(uri),
				},
				Version: version,
			},
			ContentChanges: []protocol.TextDocumentContentChangeEvent{
				{
					Value: protocol.TextDocumentContentChangeWholeDocument{
						Text: content,
					},
				},
			},
		}
		return c.Notify(ctx, "textDocument/didChange", params)
	}
	c.openFilesMu.Unlock()

	params := protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        protocol.DocumentUri(uri),
			LanguageID: DetectLanguageID(uri),
			Version:    1,
			Text:       content,
		},
	}
	if err := c.Notify(ctx, "textDocument/didOpen", params); err != nil {
		return err
	}

	c.openFilesMu.Lock()
	c.openFiles[uri] = &OpenFileInfo{
		Version: 1,
		URI:     protocol.DocumentUri(uri),
	}
	c.openFilesMu.Unlock()
	return nil
}

func (c *Client) OverlayWorktree(ctx context.Context, filePath string) (string, func(), error) {
	canonicalPath, wtRoot, repoRoot, isWorktree := MapWorktreePath(filePath)
	if !isWorktree {
		if err := c.OpenFile(ctx, filePath); err != nil {
			return "", func() {}, err
		}
		return filePath, func() {}, nil
	}

	c.worktreeOverlayMu.Lock()

	type pair struct {
		wtFile    string
		canonFile string
	}
	seen := make(map[string]bool)
	var pairs []pair

	addPair := func(wtFile, canonFile string) {
		if !seen[canonFile] {
			seen[canonFile] = true
			pairs = append(pairs, pair{wtFile: wtFile, canonFile: canonFile})
		}
	}
	addPair(filePath, canonicalPath)

	if out, err := exec.Command("/usr/bin/git", "-C", wtRoot, "status", "--porcelain", "-uall").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if len(line) < 4 {
				continue
			}
			rel := strings.TrimSpace(line[3:])
			if idx := strings.Index(rel, " -> "); idx >= 0 {
				rel = rel[idx+4:]
			}
			if strings.HasSuffix(rel, ".rs") || strings.HasSuffix(rel, ".cs") {
				addPair(wtRoot+"/"+rel, repoRoot+"/"+rel)
			}
		}
	}

	for _, p := range pairs {
		content, err := os.ReadFile(p.wtFile)
		if err != nil {
			if p.wtFile == filePath {
				c.worktreeOverlayMu.Unlock()
				return "", func() {}, fmt.Errorf("error reading worktree file: %w", err)
			}
			continue
		}
		if err := c.OpenOrUpdateWithContent(ctx, p.canonFile, string(content)); err != nil && p.wtFile == filePath {
			c.worktreeOverlayMu.Unlock()
			return "", func() {}, err
		}
	}

	restore := func() {
		defer c.worktreeOverlayMu.Unlock()
		restoreCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for _, p := range pairs {
			if mainBytes, err := os.ReadFile(p.canonFile); err == nil {
				_ = c.OpenOrUpdateWithContent(restoreCtx, p.canonFile, string(mainBytes))
			} else {
				_ = c.CloseFile(restoreCtx, p.canonFile)
			}
		}
	}

	return canonicalPath, restore, nil
}

func (c *Client) OpenFile(ctx context.Context, filepath string) error {
	content, err := os.ReadFile(filepath)
	if err != nil {
		return fmt.Errorf("error reading file: %w", err)
	}
	return c.OpenOrUpdateWithContent(ctx, filepath, string(content))
}

func (c *Client) NotifyChange(ctx context.Context, filepath string) error {
	uri := fmt.Sprintf("file://%s", filepath)

	content, err := os.ReadFile(filepath)
	if err != nil {
		return fmt.Errorf("error reading file: %w", err)
	}

	c.openFilesMu.Lock()
	fileInfo, isOpen := c.openFiles[uri]
	if !isOpen {
		c.openFilesMu.Unlock()
		return fmt.Errorf("cannot notify change for unopened file: %s", filepath)
	}

	// Increment version
	fileInfo.Version++
	version := fileInfo.Version
	c.openFilesMu.Unlock()

	params := protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{
				URI: protocol.DocumentUri(uri),
			},
			Version: version,
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			{
				Value: protocol.TextDocumentContentChangeWholeDocument{
					Text: string(content),
				},
			},
		},
	}

	return c.Notify(ctx, "textDocument/didChange", params)
}

func (c *Client) CloseFile(ctx context.Context, filepath string) error {
	uri := fmt.Sprintf("file://%s", filepath)

	c.openFilesMu.Lock()
	if _, exists := c.openFiles[uri]; !exists {
		c.openFilesMu.Unlock()
		return nil // Already closed
	}
	c.openFilesMu.Unlock()

	params := protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{
			URI: protocol.DocumentUri(uri),
		},
	}
	lspLogger.Debug("Closing file: %s", params.TextDocument.URI.Dir())
	if err := c.Notify(ctx, "textDocument/didClose", params); err != nil {
		return err
	}

	c.openFilesMu.Lock()
	delete(c.openFiles, uri)
	c.openFilesMu.Unlock()

	return nil
}

func (c *Client) IsFileOpen(filepath string) bool {
	uri := fmt.Sprintf("file://%s", filepath)
	c.openFilesMu.RLock()
	defer c.openFilesMu.RUnlock()
	_, exists := c.openFiles[uri]
	return exists
}

// CloseAllFiles closes all currently open files
func (c *Client) CloseAllFiles(ctx context.Context) {
	c.openFilesMu.Lock()
	filesToClose := make([]string, 0, len(c.openFiles))

	// First collect all URIs that need to be closed
	for uri := range c.openFiles {
		// Convert URI back to file path by trimming "file://" prefix
		filePath := strings.TrimPrefix(uri, "file://")
		filesToClose = append(filesToClose, filePath)
	}
	c.openFilesMu.Unlock()

	// Then close them all
	for _, filePath := range filesToClose {
		err := c.CloseFile(ctx, filePath)
		if err != nil {
			lspLogger.Error("Error closing file %s: %v", filePath, err)
		}
	}

	lspLogger.Debug("Closed %d files", len(filesToClose))
}

func (c *Client) GetFileDiagnostics(uri protocol.DocumentUri) []protocol.Diagnostic {
	c.diagnosticsMu.RLock()
	defer c.diagnosticsMu.RUnlock()

	return c.diagnostics[uri]
}

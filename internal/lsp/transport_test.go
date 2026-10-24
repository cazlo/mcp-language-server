package lsp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// chunkedWriter forwards every Write to the pipe in small pieces, the way a
// large write to a real pipe can be split, so unsynchronized writers interleave.
type chunkedWriter struct{ w io.WriteCloser }

func (c chunkedWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		k := min(len(p), 7)
		m, err := c.w.Write(p[:k])
		n += m
		if err != nil {
			return n, err
		}
		p = p[k:]
	}
	return n, nil
}

func (c chunkedWriter) Close() error { return c.w.Close() }

// Concurrent notifications (tool calls racing file-watcher events) must reach
// the server as whole messages; an interleaved header or body makes servers
// such as rust-analyzer exit on a malformed payload.
func TestConcurrentNotifyKeepsMessagesWhole(t *testing.T) {
	r, w := io.Pipe()
	c := &Client{stdin: chunkedWriter{w}}

	const writers, each = 8, 50
	payload := strings.Repeat("x", 300)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range each {
				params := map[string]string{"id": fmt.Sprintf("%d-%d", i, j), "pad": payload}
				if err := c.Notify(context.Background(), "test/notify", params); err != nil {
					t.Errorf("notify: %v", err)
				}
			}
		}()
	}
	go func() { wg.Wait(); w.Close() }()

	reader := bufio.NewReader(r)
	got := 0
	for {
		msg, err := ReadMessage(reader)
		if err == io.EOF || (err != nil && got == writers*each) {
			break
		}
		if err != nil {
			t.Fatalf("message %d: %v", got, err)
		}
		if msg.Method != "test/notify" {
			t.Fatalf("message %d: method %q", got, msg.Method)
		}
		got++
	}
	if got != writers*each {
		t.Fatalf("read %d messages, want %d", got, writers*each)
	}
}

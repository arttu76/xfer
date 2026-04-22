// Package testutil provides the socket-pair loopback + byte capture helpers
// that both the xmodem and zmodem test suites need. Kept out of production
// builds by the _test.go build constraint on every file here — nothing in
// this package is linked into the server binary.
package testutil

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// Pair returns a connected pair of net.Pipe ends, both registered for cleanup.
func Pair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	server, client = net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	return
}

// Capturer accumulates every byte read from a connection.
type Capturer struct {
	mu  sync.Mutex
	buf []byte
}

// Start begins reading from conn in a goroutine until the connection errors.
func Start(conn net.Conn) *Capturer {
	c := &Capturer{}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := conn.Read(b)
			if n > 0 {
				c.mu.Lock()
				c.buf = append(c.buf, b[:n]...)
				c.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return c
}

// Bytes returns a snapshot of everything captured so far.
func (c *Capturer) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, len(c.buf))
	copy(out, c.buf)
	return out
}

// WaitFor polls the capturer until predicate returns true or timeout elapses.
// Fails the test on timeout, printing the captured bytes in hex for debugging.
func (c *Capturer) WaitFor(t *testing.T, predicate func([]byte) bool, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b := c.Bytes(); predicate(b) {
			return b
		}
		time.Sleep(2 * time.Millisecond)
	}
	got := c.Bytes()
	t.Fatalf("WaitFor timed out; got %d bytes:\n%x", len(got), got)
	return nil
}

// FirstDiff returns a human-readable description of where two byte slices
// diverge — used in golden-file cross-language comparisons.
func FirstDiff(want, got []byte) string {
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		if want[i] != got[i] {
			start, end := i-8, i+16
			if start < 0 {
				start = 0
			}
			if end > n {
				end = n
			}
			return fmt.Sprintf("byte %d: want 0x%02x got 0x%02x\n  want window: %s\n  got window:  %s",
				i, want[i], got[i], hex.EncodeToString(want[start:end]), hex.EncodeToString(got[start:end]))
		}
	}
	return fmt.Sprintf("length differs: %d vs %d", len(want), len(got))
}

// Equal is a thin wrapper so tests needn't import bytes just for this.
func Equal(a, b []byte) bool { return bytes.Equal(a, b) }

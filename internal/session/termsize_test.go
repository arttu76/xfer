package session

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solvalou/xfer/internal/constants"
)

// testDetectTimeout is the wall-clock deadline tests give DetectTerminalSize.
// Long enough for split-read replies (the slowest test paces bytes ~5ms
// apart) and short enough that a non-responding test doesn't slow the
// suite. Replaces ad-hoc magic numbers in individual cases.
const testDetectTimeout = 500 * time.Millisecond

// run wires DetectTerminalSize against a net.Pipe so a test goroutine can
// play the role of the client: read everything the server writes and feed
// back any reply bytes the test wants to send. respond is invoked with the
// bytes the server has written so far; it returns reply bytes to send back
// (nil/empty means "stay silent").
func run(t *testing.T, fallbackCols, fallbackRows int, respond func(written []byte) []byte) (cols, rows int, detected bool, wireOut []byte) {
	t.Helper()
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		tmp := make([]byte, 256)
		responded := false
		for {
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, err := client.Read(tmp)
			if n > 0 {
				mu.Lock()
				buf.Write(tmp[:n])
				snapshot := append([]byte(nil), buf.Bytes()...)
				mu.Unlock()
				if !responded {
					if reply := respond(snapshot); len(reply) > 0 {
						_, _ = client.Write(reply)
						responded = true
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	cols, rows, detected = DetectTerminalSize(server, fallbackCols, fallbackRows, testDetectTimeout)
	_ = server.Close()
	<-clientDone
	mu.Lock()
	wireOut = append([]byte(nil), buf.Bytes()...)
	mu.Unlock()
	return
}

func TestDetectTerminalSize_Success(t *testing.T) {
	cols, rows, detected, wire := run(t, constants.TermDefaultWidth, constants.TermDefaultHeight, func(written []byte) []byte {
		// Wait until the probe sequence has been observed before replying,
		// so we know the server already moved on to its read loop.
		if !bytes.Contains(written, []byte("\x1b[6n")) {
			return nil
		}
		// Reply with cursor position 25 rows × 80 cols.
		return []byte("\x1b[25;80R")
	})
	if !detected {
		t.Fatalf("expected detected=true, got false; wire: %q", wire)
	}
	if cols != 80 || rows != 25 {
		t.Fatalf("expected 80x25, got %dx%d", cols, rows)
	}
	if !strings.Contains(string(wire), "Detecting terminal size") {
		t.Fatalf("missing detect banner; wire: %q", wire)
	}
	if !strings.Contains(string(wire), "Terminal size: 80x25") {
		t.Fatalf("missing success line; wire: %q", wire)
	}
	// Probe sequence must be sent in full, in the documented order.
	if !bytes.Contains(wire, []byte("\x1b[s\x1b[999;999H\x1b[6n\x1b[u")) {
		t.Fatalf("probe sequence missing or reordered; wire: %q", wire)
	}
}

func TestDetectTerminalSize_Timeout(t *testing.T) {
	cols, rows, detected, wire := run(t, constants.TermDefaultWidth, constants.TermDefaultHeight, func([]byte) []byte { return nil })
	if detected {
		t.Fatalf("expected detected=false on timeout")
	}
	if cols != constants.TermDefaultWidth || rows != constants.TermDefaultHeight {
		t.Fatalf("expected fallback %dx%d, got %dx%d", constants.TermDefaultWidth, constants.TermDefaultHeight, cols, rows)
	}
	wantLine := fmt.Sprintf("Terminal size not detected, using %dx%d", constants.TermDefaultWidth, constants.TermDefaultHeight)
	if !strings.Contains(string(wire), wantLine) {
		t.Fatalf("missing fallback line %q; wire: %q", wantLine, wire)
	}
}

func TestDetectTerminalSize_OutOfRange(t *testing.T) {
	// 1x1 is well below TermMinWidth/MinHeight — must be rejected.
	cols, rows, detected, wire := run(t, constants.TermDefaultWidth, constants.TermDefaultHeight, func(written []byte) []byte {
		if !bytes.Contains(written, []byte("\x1b[6n")) {
			return nil
		}
		return []byte("\x1b[1;1R")
	})
	if detected {
		t.Fatalf("expected detected=false on out-of-range reply, got 1x1 accepted; wire: %q", wire)
	}
	if cols != constants.TermDefaultWidth || rows != constants.TermDefaultHeight {
		t.Fatalf("expected fallback %dx%d, got %dx%d", constants.TermDefaultWidth, constants.TermDefaultHeight, cols, rows)
	}
}

func TestDetectTerminalSize_GarbageBeforeReply(t *testing.T) {
	cols, rows, detected, wire := run(t, constants.TermDefaultWidth, constants.TermDefaultHeight, func(written []byte) []byte {
		if !bytes.Contains(written, []byte("\x1b[6n")) {
			return nil
		}
		// Stray bytes (echo of a key, noise) followed by a valid reply.
		return []byte("garbage\x00\x01\x1b[40;132R")
	})
	if !detected {
		t.Fatalf("expected detected=true, got false; wire: %q", wire)
	}
	if cols != 132 || rows != 40 {
		t.Fatalf("expected 132x40, got %dx%d", cols, rows)
	}
}

func TestDetectTerminalSize_ReplySplitAcrossReads(t *testing.T) {
	// Send the reply two bytes at a time to make sure the read loop
	// accumulates across multiple Reads instead of bailing on the first
	// non-matching chunk.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		drain := make([]byte, 256)
		// Read until we've seen the probe.
		var seen []byte
		for !bytes.Contains(seen, []byte("\x1b[6n")) {
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			n, err := client.Read(drain)
			if n > 0 {
				seen = append(seen, drain[:n]...)
			}
			if err != nil {
				return
			}
		}
		reply := []byte("\x1b[24;80R")
		for i := 0; i < len(reply); i += 2 {
			end := i + 2
			if end > len(reply) {
				end = len(reply)
			}
			_, _ = client.Write(reply[i:end])
			time.Sleep(5 * time.Millisecond)
		}
		// Drain any trailing writes from the server (the result line)
		// so the server's Write doesn't block on a full pipe.
		for {
			_ = client.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			if _, err := client.Read(drain); err != nil {
				return
			}
		}
	}()

	cols, rows, detected := DetectTerminalSize(server, constants.TermDefaultWidth, constants.TermDefaultHeight, testDetectTimeout)
	_ = server.Close()
	<-clientDone

	if !detected || cols != 80 || rows != 24 {
		t.Fatalf("split-read reply not assembled: detected=%v %dx%d", detected, cols, rows)
	}
}

// Improvement #2: an out-of-range reply must abandon the read loop
// immediately rather than waiting the full timeout for a "better" reply
// that is never going to arrive.
func TestDetectTerminalSize_OutOfRangeBailsFast(t *testing.T) {
	const longTimeout = 5 * time.Second
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		drain := make([]byte, 256)
		var seen []byte
		for !bytes.Contains(seen, []byte("\x1b[6n")) {
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			n, err := client.Read(drain)
			if n > 0 {
				seen = append(seen, drain[:n]...)
			}
			if err != nil {
				return
			}
		}
		_, _ = client.Write([]byte("\x1b[1;1R"))
		// Drain whatever the server writes after deciding so its Write
		// doesn't block on a full pipe.
		for {
			_ = client.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			if _, err := client.Read(drain); err != nil {
				return
			}
		}
	}()

	start := time.Now()
	_, _, detected := DetectTerminalSize(server, constants.TermDefaultWidth, constants.TermDefaultHeight, longTimeout)
	elapsed := time.Since(start)
	_ = server.Close()

	if detected {
		t.Fatalf("out-of-range reply must be rejected, but detected=true")
	}
	// Must finish well under the 5 s timeout — the reply arrives almost
	// instantly so the net wait is just the post-decision drain (~150 ms).
	if elapsed > time.Second {
		t.Fatalf("out-of-range reply caused the loop to wait %v (expected <1s)", elapsed)
	}
}

// Improvement #1: bytes that arrive after the detect decision must be
// drained so they don't pollute the next read (which would otherwise
// receive them as menu input — the failure mode the wirelog showed for
// Term 4.8).
func TestDetectTerminalSize_DrainsLateReply(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		drain := make([]byte, 256)
		var seen []byte
		for !bytes.Contains(seen, []byte("\x1b[6n")) {
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			n, err := client.Read(drain)
			if n > 0 {
				seen = append(seen, drain[:n]...)
			}
			if err != nil {
				return
			}
		}
		// Stay silent until well past the detect timeout, then send a
		// late reply — exactly the Term 4.8 scenario from the wirelog.
		time.Sleep(120 * time.Millisecond)
		_, _ = client.Write([]byte("\x1b[31;80R"))
		// Drain server writes (Detecting…/result line) so the pipe stays
		// open until the test's read window expires.
		for {
			_ = client.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
			if _, err := client.Read(drain); err != nil {
				return
			}
		}
	}()

	// Detect with a tight 50 ms timeout so the late reply arrives well
	// after the decision — but within the post-decision drain window.
	_, _, detected := DetectTerminalSize(server, constants.TermDefaultWidth, constants.TermDefaultHeight, 50*time.Millisecond)
	if detected {
		t.Fatalf("detection should have timed out before the late reply arrived")
	}

	// Now read whatever the server sees as "next user input". With the
	// drain in place this should be empty/timeout — without it, the
	// `\x1b[31;80R` reply would land here.
	_ = server.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	scratch := make([]byte, 64)
	n, _ := server.Read(scratch)
	if n > 0 {
		t.Fatalf("late reply leaked into post-detect read: % x (%q)", scratch[:n], scratch[:n])
	}
	_ = server.Close()
	<-clientDone
}

// Improvement #4: ResolveTerminalSize is the cfg-aware shim main.go uses.
// These two cases pin down the contract: TermDetect=false skips the
// probe entirely, TermDetect=true delegates to DetectTerminalSize.
func TestResolveTerminalSize_DetectDisabled(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	cap := newCaptureWriter()
	go cap.start(client)

	cfg := &Config{
		TermDetect: false,
		TermWidth:  77,
		TermHeight: 33,
	}
	cols, rows, detected := ResolveTerminalSize(server, cfg)
	_ = server.Close()
	cap.wait(t)

	if detected {
		t.Fatalf("disabled detection must report detected=false")
	}
	if cols != 77 || rows != 33 {
		t.Fatalf("disabled detection must echo cfg, got %dx%d", cols, rows)
	}
	if got := cap.bytes(); len(got) > 0 {
		t.Fatalf("disabled detection must not write to the wire; got %q", got)
	}
}

func TestResolveTerminalSize_DetectEnabled(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	doneR := make(chan struct{})
	go func() {
		defer close(doneR)
		drain := make([]byte, 256)
		var seen []byte
		for !bytes.Contains(seen, []byte("\x1b[6n")) {
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			n, err := client.Read(drain)
			if n > 0 {
				seen = append(seen, drain[:n]...)
			}
			if err != nil {
				return
			}
		}
		_, _ = client.Write([]byte("\x1b[24;80R"))
		for {
			_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			if _, err := client.Read(drain); err != nil {
				return
			}
		}
	}()

	cfg := &Config{
		TermDetect:        true,
		TermWidth:         constants.TermDefaultWidth,
		TermHeight:        constants.TermDefaultHeight,
		TermDetectTimeout: testDetectTimeout,
	}
	cols, rows, detected := ResolveTerminalSize(server, cfg)
	_ = server.Close()
	<-doneR

	if !detected || cols != 80 || rows != 24 {
		t.Fatalf("expected detected 80x24, got detected=%v %dx%d", detected, cols, rows)
	}
}

// captureWriter is a tiny goroutine-backed sink for the disabled-detect
// test: it records everything the server writes so the assertion can
// verify nothing went out. done is initialized eagerly so wait() can
// race-freely select on it before the goroutine has scheduled.
type captureWriter struct {
	mu   sync.Mutex
	buf  []byte
	done chan struct{}
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{done: make(chan struct{})}
}

func (c *captureWriter) start(conn net.Conn) {
	defer close(c.done)
	tmp := make([]byte, 256)
	for {
		n, err := conn.Read(tmp)
		if n > 0 {
			c.mu.Lock()
			c.buf = append(c.buf, tmp[:n]...)
			c.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (c *captureWriter) wait(t *testing.T) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("captureWriter goroutine never exited")
	}
}

func (c *captureWriter) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf...)
}

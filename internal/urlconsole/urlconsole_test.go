package urlconsole

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// makeReg returns a registry whose log messages are collected in the
// returned slice (protected by its own mutex).
func makeReg(t *testing.T) (*Registry, func() []string) {
	t.Helper()
	var (
		mu  sync.Mutex
		log []string
	)
	reg := NewRegistry(func(s string) {
		mu.Lock()
		log = append(log, s)
		mu.Unlock()
	})
	return reg, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(log))
		copy(out, log)
		return out
	}
}

func TestRegistry_InjectEmpty(t *testing.T) {
	reg := NewRegistry(nil)
	if reg.Inject([]byte("foo\n")) {
		t.Fatal("inject into empty registry should return false")
	}
}

func TestRegistry_RoutesToOldest(t *testing.T) {
	reg := NewRegistry(nil)
	chA := make(chan []byte, 1)
	chB := make(chan []byte, 1)
	_ = reg.Register(chA)
	_ = reg.Register(chB)

	if !reg.Inject([]byte("url\n")) {
		t.Fatal("inject should succeed")
	}
	select {
	case got := <-chA:
		if !bytes.Equal(got, []byte("url\n")) {
			t.Fatalf("got %q", got)
		}
	case <-chB:
		t.Fatal("inject delivered to newer session instead of oldest")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("oldest channel received nothing")
	}
}

func TestRegistry_HandoffAfterDeregister(t *testing.T) {
	reg := NewRegistry(nil)
	chA := make(chan []byte, 1)
	chB := make(chan []byte, 1)
	idA := reg.Register(chA)
	_ = reg.Register(chB)

	reg.Inject([]byte("first\n"))
	// Drain A to keep its buffer clear.
	<-chA

	reg.Deregister(idA)

	if !reg.Inject([]byte("second\n")) {
		t.Fatal("inject after handoff should succeed")
	}
	select {
	case got := <-chB:
		if !bytes.Equal(got, []byte("second\n")) {
			t.Fatalf("got %q", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("B did not receive after A deregistered")
	}
}

func TestRegistry_BufferFullDoesNotBlock(t *testing.T) {
	reg := NewRegistry(nil)
	ch := make(chan []byte, 1)
	_ = reg.Register(ch)

	if !reg.Inject([]byte("first\n")) {
		t.Fatal("first inject should succeed")
	}
	// Second inject with buffer still full must not block the caller
	// (tested by completing within a short timeout).
	done := make(chan bool, 1)
	go func() {
		ok := reg.Inject([]byte("second\n"))
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("inject onto a full buffer should return false")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("inject blocked on a full buffer")
	}
}

func TestRegistry_DeregisterIsIdempotent(t *testing.T) {
	reg := NewRegistry(nil)
	ch := make(chan []byte, 1)
	id := reg.Register(ch)
	reg.Deregister(id)
	reg.Deregister(id) // must not panic
	if reg.Waiting() != 0 {
		t.Fatal("waiting count should be 0 after deregister")
	}
}

func TestRun_DeliversLinesAndStopsOnEOF(t *testing.T) {
	reg, _ := makeReg(t)
	ch := make(chan []byte, 4)
	_ = reg.Register(ch)

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		Run(reg, pr)
		close(done)
	}()

	go func() {
		defer pw.Close()
		_, _ = pw.Write([]byte("https://example.com/a\n"))
		_, _ = pw.Write([]byte("https://example.com/b\n"))
	}()

	// Two lines expected on ch, in order.
	want := []string{"https://example.com/a\n", "https://example.com/b\n"}
	for i, w := range want {
		select {
		case got := <-ch:
			if string(got) != w {
				t.Fatalf("line %d: got %q want %q", i, got, w)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("line %d: not delivered", i)
		}
	}

	// EOF on the pipe should cause Run to return.
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return on EOF")
	}
}

func TestRun_NoWaitingSessionsSilentlySkips(t *testing.T) {
	reg, capture := makeReg(t)

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		Run(reg, pr)
		close(done)
	}()
	_, _ = pw.Write([]byte("orphan\n"))
	// Give the loop a tick to process.
	time.Sleep(50 * time.Millisecond)
	_ = pw.Close()
	<-done

	log := capture()
	foundSkipMsg := false
	for _, m := range log {
		if strings.Contains(m, "no session") {
			foundSkipMsg = true
			break
		}
	}
	if !foundSkipMsg {
		t.Fatalf("expected a 'no session' log line, got %v", log)
	}
}

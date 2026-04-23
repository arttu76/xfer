// Package urlconsole lets the server's own stdin inject text into an active
// URL-entry prompt. A single operator sitting at the modern computer hosting
// xfer can paste a URL into the server's terminal instead of having to type
// it on the old-computer client.
//
// Sessions that want to receive server-side input Register their inject
// channel. Run reads stdin line by line and forwards each line to the
// oldest-registered session — if the server-operator is already attached
// to session A, a second session B entering URL mode at the same time does
// NOT start receiving operator input until A deregisters. Client-side typing
// is unaffected for every session.
package urlconsole

import (
	"bufio"
	"fmt"
	"io"
	"sync"
)

// Registry tracks sessions currently waiting for URL input and routes
// injected stdin lines to the one that registered first.
type Registry struct {
	log func(string)

	mu      sync.Mutex
	waiting map[int]chan<- []byte
	nextID  int
}

// NewRegistry returns an empty Registry. logFn is called with human-readable
// status lines ("delivered to #3", "no session waiting", ...).
func NewRegistry(logFn func(string)) *Registry {
	if logFn == nil {
		logFn = func(string) {}
	}
	return &Registry{
		log:     logFn,
		waiting: make(map[int]chan<- []byte),
	}
}

// Register adds inject to the waiting set. The returned id is the handle
// callers pass to Deregister. Ids are monotonic so the oldest registration
// always has the smallest id — that's how Inject picks a recipient.
func (r *Registry) Register(inject chan<- []byte) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	id := r.nextID
	r.waiting[id] = inject
	return id
}

// Deregister is idempotent; calling it with a stale id is a no-op.
func (r *Registry) Deregister(id int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.waiting, id)
}

// Waiting returns how many sessions are currently waiting for URL input.
// Exposed for tests and for the Run loop's log message.
func (r *Registry) Waiting() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.waiting)
}

// Inject delivers line to the oldest-registered session. Returns true on
// successful delivery, false when there's nobody to deliver to or when the
// chosen session's inject buffer is full (so stdin reader is never wedged
// by a stuck consumer).
func (r *Registry) Inject(line []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.waiting) == 0 {
		return false
	}
	var (
		oldestID int
		oldestCh chan<- []byte
		first    = true
	)
	for id, ch := range r.waiting {
		if first || id < oldestID {
			oldestID, oldestCh = id, ch
			first = false
		}
	}
	select {
	case oldestCh <- line:
		r.log(fmt.Sprintf("stdin: delivered to session #%d", oldestID))
		return true
	default:
		r.log(fmt.Sprintf("stdin: session #%d inject buffer full; line dropped", oldestID))
		return false
	}
}

// Run reads stdin line by line and injects each line (including a trailing
// \n so the session's prompt handler sees the submit boundary) into the
// registry. Blocks until stdin returns an error — typically EOF — which is
// the normal termination when the server is daemonised or run with
// </dev/null. Safe to launch with `go Run(...)` at startup.
func Run(r *Registry, stdin io.Reader) {
	scanner := bufio.NewScanner(stdin)
	// URLs can be long; set a generous max token size (8 KiB beats the
	// 64 KiB default, but this is comfortable and forbids ridiculous
	// paste bombs).
	scanner.Buffer(make([]byte, 0, 4096), 8192)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		line = append(line, '\n')
		if r.Waiting() == 0 {
			r.log("stdin: no session is in URL entry; line ignored")
			continue
		}
		r.Inject(line)
	}
}

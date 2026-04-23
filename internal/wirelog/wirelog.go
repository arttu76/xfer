// Package wirelog provides a net.Conn wrapper that records every Read and
// Write to a shared log file, one line of hexdump per operation. It exists
// to debug protocol-level hangs (ZMODEM handshakes that wedge for tens of
// seconds, etc.) by giving us a byte-accurate, timestamped trace of what
// flowed each direction — without needing tcpdump or a man-in-the-middle.
package wirelog

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Sink serialises writes from every wrapped connection to a single file.
// Lines are flushed per operation so a crash or SIGKILL still leaves a
// usable trace on disk.
type Sink struct {
	mu sync.Mutex
	w  *bufio.Writer
	f  *os.File
}

// Open creates/truncates path for writing. Use "-" for stderr.
func Open(path string) (*Sink, error) {
	if path == "-" {
		return &Sink{w: bufio.NewWriter(os.Stderr)}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &Sink{w: bufio.NewWriter(f), f: f}, nil
}

func (s *Sink) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.w.Flush()
	if s.f != nil {
		return s.f.Close()
	}
	return nil
}

// emit writes a single hexdump line: timestamp, tag, byte count, hex, ascii.
// Large buffers are split into 32-byte groups so ZDATA bursts stay legible.
func (s *Sink) emit(tag, peer string, buf []byte) {
	if s == nil {
		return
	}
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	s.mu.Lock()
	defer s.mu.Unlock()
	for off := 0; off < len(buf); off += 32 {
		end := off + 32
		if end > len(buf) {
			end = len(buf)
		}
		fmt.Fprintf(s.w, "%s %s %s +%-4d %-5d  %s  |%s|\n",
			ts, peer, tag, off, end-off, hexBytes(buf[off:end]), printable(buf[off:end]))
	}
	_ = s.w.Flush()
}

func hexBytes(b []byte) string {
	out := make([]byte, 0, len(b)*3)
	const hex = "0123456789abcdef"
	for i, c := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, hex[c>>4], hex[c&0xf])
	}
	return string(out)
}

func printable(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 0x20 && c < 0x7f {
			out[i] = c
		} else {
			out[i] = '.'
		}
	}
	return string(out)
}

// Wrap returns a net.Conn that tees every Read (tagged C->S) and Write
// (tagged S->C) to sink. peer is a short label used in the log — typically
// the remote address — so multiple simultaneous connections stay readable.
// Passing a nil sink disables logging and returns conn unchanged.
func Wrap(conn net.Conn, sink *Sink, peer string) net.Conn {
	if sink == nil {
		return conn
	}
	return &loggingConn{Conn: conn, sink: sink, peer: peer}
}

type loggingConn struct {
	net.Conn
	sink *Sink
	peer string
}

func (c *loggingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.sink.emit("C->S", c.peer, p[:n])
	}
	if err != nil && err != io.EOF {
		c.sink.emit("C->S", c.peer, []byte(fmt.Sprintf("<read err: %v>", err)))
	}
	return n, err
}

func (c *loggingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.sink.emit("S->C", c.peer, p[:n])
	}
	if err != nil {
		c.sink.emit("S->C", c.peer, []byte(fmt.Sprintf("<write err: %v>", err)))
	}
	return n, err
}

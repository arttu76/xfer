package zmodem

import (
	"bytes"
	"testing"
	"time"

	"github.com/solvalou/xfer/internal/testutil"
)

// TestSendReceiveLoopback exercises the production ZMODEM Send → Receive
// path end-to-end across a net.Pipe. Different payload shapes catch
// stream-level edge cases (small/large/repeated/empty/binary).
func TestSendReceiveLoopback(t *testing.T) {
	payloads := []struct {
		name string
		data []byte
	}{
		{"small text", []byte("Hello, ZMODEM receiver!\nLine two.\n")},
		{"single subpacket boundary", bytes.Repeat([]byte{0xa5}, subpacketSize)},
		{"two subpackets", bytes.Repeat([]byte{0x5a}, subpacketSize+128)},
		{"escape-heavy", func() []byte {
			// All bytes that ZDLE-escape: the unescape path on the receive
			// side must put them back exactly.
			pat := []byte{0x0d, 0x10, 0x11, 0x13, 0x18, 0x7f, 0x8d, 0x90, 0x91, 0x93, 0xff}
			return bytes.Repeat(pat, 200)
		}()},
		{"medium binary", func() []byte {
			b := make([]byte, 8192)
			for i := range b {
				b[i] = byte(i*37 + (i >> 4))
			}
			return b
		}()},
		{"large binary", func() []byte {
			b := make([]byte, 64*1024)
			for i := range b {
				b[i] = byte(i*13 + (i >> 3))
			}
			return b
		}()},
	}

	for _, pl := range payloads {
		t.Run(pl.name, func(t *testing.T) {
			server, client := testutil.Pair(t)

			sendDone := make(chan error, 1)
			recvDone := make(chan struct {
				ReceiveResult
				err error
			}, 1)

			go func() {
				sendDone <- SendBuffer(server, pl.data, "TEST.BIN")
			}()
			go func() {
				res, err := Receive(client, ReceiveConfig{})
				recvDone <- struct {
					ReceiveResult
					err error
				}{res, err}
			}()

			select {
			case err := <-sendDone:
				if err != nil {
					t.Fatalf("Send: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("Send timed out")
			}

			select {
			case got := <-recvDone:
				if got.err != nil {
					t.Fatalf("Receive: %v", got.err)
				}
				if got.Filename != "TEST.BIN" {
					t.Fatalf("filename: want TEST.BIN got %q", got.Filename)
				}
				if got.Size != len(pl.data) {
					t.Fatalf("size: want %d got %d", len(pl.data), got.Size)
				}
				if !bytes.Equal(got.Data, pl.data) {
					t.Fatalf("data mismatch (want %d bytes, got %d): %s",
						len(pl.data), len(got.Data),
						testutil.FirstDiff(pl.data, got.Data))
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("Receive timed out")
			}
		})
	}
}

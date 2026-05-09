package kermit

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/arttu76/xfer/internal/testutil"
)

func TestSendReceiveProductionLoopback(t *testing.T) {
	payloads := []struct {
		name string
		data []byte
	}{
		{"small text", []byte("Hello, Kermit receiver!\nSecond line.\n")},
		{"empty", []byte{}},
		{"medium binary", func() []byte {
			b := make([]byte, 4096)
			for i := range b {
				b[i] = byte(i*37 + (i >> 4))
			}
			return b
		}()},
		{"runs for RLE", bytes.Repeat([]byte{0x00}, 2000)},
		{"large binary", func() []byte {
			b := make([]byte, 50_000)
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
				sendDone <- Send(connAdapter{server}, pl.data, "TEST.BIN", Config{ReadTimeout: 2 * time.Second})
			}()
			go func() {
				res, err := Receive(connAdapter{client}, ReceiveConfig{ReadTimeout: 5 * time.Second})
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
				if !bytes.Equal(got.Data, pl.data) {
					t.Fatalf("data mismatch (want %d bytes, got %d): %s",
						len(pl.data), len(got.Data),
						testutil.FirstDiff(pl.data, got.Data))
				}
				// Receiver advertises capAttributes, so for non-empty payloads
				// the sender will emit an A packet with the size.
				if len(pl.data) > 0 && got.Size != len(pl.data) {
					t.Fatalf("attr size: want %d got %d", len(pl.data), got.Size)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("Receive timed out")
			}
		})
	}
}

func TestReceiveSurfacesSenderError(t *testing.T) {
	server, client := testutil.Pair(t)

	// Drive a fake sender by hand: complete the S handshake, then emit an E.
	go func() {
		// Read the receiver's nothing — we drive everything from this side.
		// First: send S.
		s := defaultSendParams()
		_, _ = server.Write(buildShortPacket(0, 'S', buildSendInitData(s), 1))
		// Read ACK-S (type-1).
		_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
		ack, err := readPacket(connAdapter{server}, 1)
		if err != nil || ack.typ != 'Y' {
			return
		}
		negotiate(&s, ack.data)
		// Send E with a reason.
		_, _ = server.Write(buildShortPacket(1, 'E', []byte("simulated boom"), s.chkLen))
	}()

	_, err := Receive(connAdapter{client}, ReceiveConfig{ReadTimeout: 2 * time.Second})
	if err == nil {
		t.Fatalf("expected error from E packet")
	}
	if !contains(err.Error(), "simulated boom") {
		t.Fatalf("error should mention reason: %v", err)
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}

// Sanity that decodeKermitData mirrors encodeData for both production paths.
func TestProductionDecoderRoundTrip(t *testing.T) {
	cases := []struct {
		in      []byte
		use8bit bool
		useRept bool
	}{
		{[]byte("Hello, world!"), false, false},
		{[]byte{0x00, 0x01, 0x1f, 0x7f}, false, false},
		{[]byte{0x80, 0xff, 0x81, 0x20, '#', '&', '~'}, true, false},
		{bytes.Repeat([]byte{'A'}, 30), false, true},
		{bytes.Repeat([]byte{0xff}, 20), true, true},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			enc := encodeData(tc.in, '#', '&', '~', tc.use8bit, tc.useRept)
			dec := decodeKermitData(enc, '#', '&', '~', tc.use8bit, tc.useRept)
			if !bytes.Equal(dec, tc.in) {
				t.Fatalf("round-trip mismatch:\n  in:  %x\n  enc: %s\n  dec: %x", tc.in, enc, dec)
			}
		})
	}
}

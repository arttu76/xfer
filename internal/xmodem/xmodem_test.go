package xmodem_test

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/solvalou/xfer/internal/testutil"
	"github.com/solvalou/xfer/internal/xmodem"
)

// runSend starts xmodem.Send in a goroutine and returns a channel for the
// eventual error so tests can assert on completion without deadlocking.
func runSend(server net.Conn, data []byte, cfg xmodem.Config) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- xmodem.Send(server, data, cfg) }()
	return ch
}

// --- Single block, CRC mode -------------------------------------------------

func TestCRCModeSingleBlockFraming(t *testing.T) {
	server, client := testutil.Pair(t)
	payload := make([]byte, 128)
	for i := range payload {
		payload[i] = byte(i + 1)
	}

	cap := testutil.Start(client)
	errCh := runSend(server, payload, xmodem.Config{ReadTimeout: 2 * time.Second})

	// Kick into CRC mode.
	mustWrite(t, client, []byte{xmodem.CRC})
	out := cap.WaitFor(t, func(b []byte) bool { return len(b) >= 133 }, 2*time.Second)

	if out[0] != xmodem.SOH {
		t.Fatalf("byte 0: got %x, want SOH", out[0])
	}
	if out[1] != 0x01 {
		t.Fatalf("block#: got %x, want 01", out[1])
	}
	if out[2] != 0xfe {
		t.Fatalf("~block#: got %x, want fe", out[2])
	}
	if !bytes.Equal(out[3:131], payload) {
		t.Fatalf("data: mismatch")
	}
	wantCrc := xmodem.CRC16Ccitt(payload)
	gotCrc := uint16(out[131])<<8 | uint16(out[132])
	if gotCrc != wantCrc {
		t.Fatalf("CRC: got %04x, want %04x", gotCrc, wantCrc)
	}

	// ACK → EOT → NAK → EOT → ACK.
	mustWrite(t, client, []byte{xmodem.ACK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 133 && b[133] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.NAK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 134 && b[134] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.ACK})

	if err := <-errCh; err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
}

// --- Sub-block SUB padding --------------------------------------------------

func TestSubBlockSUBPadding(t *testing.T) {
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	errCh := runSend(server, []byte("hello"), xmodem.Config{ReadTimeout: 2 * time.Second})
	mustWrite(t, client, []byte{xmodem.CRC})

	out := cap.WaitFor(t, func(b []byte) bool { return len(b) >= 133 }, 2*time.Second)
	if !bytes.Equal(out[3:8], []byte("hello")) {
		t.Fatalf("data prefix mismatch: %x", out[3:8])
	}
	for i := 8; i < 131; i++ {
		if out[i] != xmodem.SUB {
			t.Fatalf("pad byte %d: got %x, want SUB", i, out[i])
		}
	}

	mustWrite(t, client, []byte{xmodem.ACK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 133 && b[133] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.NAK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 134 && b[134] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.ACK})
	if err := <-errCh; err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// --- Multi-block numbering --------------------------------------------------

func TestMultiBlockNumbering(t *testing.T) {
	server, client := testutil.Pair(t)
	payload := make([]byte, 384)
	for i := range payload {
		payload[i] = byte(i)
	}
	cap := testutil.Start(client)
	errCh := runSend(server, payload, xmodem.Config{ReadTimeout: 2 * time.Second})
	mustWrite(t, client, []byte{xmodem.CRC})

	for blk := 1; blk <= 3; blk++ {
		out := cap.WaitFor(t, func(b []byte) bool { return len(b) >= blk*133 }, 2*time.Second)
		off := (blk - 1) * 133
		if out[off] != xmodem.SOH {
			t.Fatalf("blk %d: SOH missing at %d", blk, off)
		}
		if out[off+1] != byte(blk&0xff) || out[off+2] != byte(^blk&0xff) {
			t.Fatalf("blk %d: bad header %02x %02x", blk, out[off+1], out[off+2])
		}
		mustWrite(t, client, []byte{xmodem.ACK})
	}
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 3*133 && b[3*133] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.NAK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 3*133+1 && b[3*133+1] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.ACK})
	if err := <-errCh; err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// --- Retransmit on NAK ------------------------------------------------------

func TestRetransmitOnNAK(t *testing.T) {
	server, client := testutil.Pair(t)
	payload := bytes.Repeat([]byte{0x41}, 128)
	cap := testutil.Start(client)
	errCh := runSend(server, payload, xmodem.Config{ReadTimeout: 2 * time.Second})
	mustWrite(t, client, []byte{xmodem.CRC})

	out := cap.WaitFor(t, func(b []byte) bool { return len(b) >= 133 }, 2*time.Second)
	first := append([]byte(nil), out[:133]...)
	mustWrite(t, client, []byte{xmodem.NAK})

	out = cap.WaitFor(t, func(b []byte) bool { return len(b) >= 266 }, 2*time.Second)
	second := out[133:266]
	if !bytes.Equal(first, second) {
		t.Fatalf("retransmit mismatch: first=%x second=%x", first, second)
	}
	mustWrite(t, client, []byte{xmodem.ACK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 266 && b[266] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.NAK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 267 && b[267] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.ACK})
	if err := <-errCh; err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// --- Block wrap past 32 KB --------------------------------------------------

func TestBlockNumberWrap(t *testing.T) {
	server, client := testutil.Pair(t)
	payload := make([]byte, 257*128)
	for i := range payload {
		payload[i] = byte(i)
	}
	cap := testutil.Start(client)
	errCh := runSend(server, payload, xmodem.Config{ReadTimeout: 3 * time.Second})
	mustWrite(t, client, []byte{xmodem.CRC})

	for blk := 1; blk <= 257; blk++ {
		b := cap.WaitFor(t, func(b []byte) bool { return len(b) >= blk*133 }, 5*time.Second)
		off := (blk - 1) * 133
		if b[off+1] != byte(blk&0xff) || b[off+2] != byte(^blk&0xff) {
			t.Fatalf("blk %d: hdr %02x %02x", blk, b[off+1], b[off+2])
		}
		mustWrite(t, client, []byte{xmodem.ACK})
	}
	out := cap.Bytes()
	if out[255*133+1] != 0x00 || out[255*133+2] != 0xff {
		t.Fatalf("block 256 wrap: %02x %02x", out[255*133+1], out[255*133+2])
	}
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 257*133 && b[257*133] == xmodem.EOT }, 3*time.Second)
	mustWrite(t, client, []byte{xmodem.NAK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 257*133+1 && b[257*133+1] == xmodem.EOT }, 3*time.Second)
	mustWrite(t, client, []byte{xmodem.ACK})
	if err := <-errCh; err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// --- Checksum mode ----------------------------------------------------------

func TestChecksumMode(t *testing.T) {
	server, client := testutil.Pair(t)
	payload := make([]byte, 128)
	for i := range payload {
		payload[i] = byte(i)
	}
	cap := testutil.Start(client)
	errCh := runSend(server, payload, xmodem.Config{ReadTimeout: 2 * time.Second})
	mustWrite(t, client, []byte{xmodem.NAK}) // checksum mode

	out := cap.WaitFor(t, func(b []byte) bool { return len(b) >= 132 }, 2*time.Second)
	if out[0] != xmodem.SOH || out[1] != 0x01 || out[2] != 0xfe {
		t.Fatalf("bad header: %02x %02x %02x", out[0], out[1], out[2])
	}
	if !bytes.Equal(out[3:131], payload) {
		t.Fatalf("data mismatch")
	}
	var sum byte
	for _, b := range payload {
		sum += b
	}
	if out[131] != sum {
		t.Fatalf("checksum: got %02x want %02x", out[131], sum)
	}
	mustWrite(t, client, []byte{xmodem.ACK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 132 && b[132] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.NAK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 133 && b[133] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.ACK})
	if err := <-errCh; err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// --- EOT handshake (explicit) ----------------------------------------------

func TestEOTHandshake(t *testing.T) {
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	errCh := runSend(server, []byte("x"), xmodem.Config{ReadTimeout: 2 * time.Second})
	mustWrite(t, client, []byte{xmodem.CRC})

	cap.WaitFor(t, func(b []byte) bool { return len(b) >= 133 }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.ACK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 133 && b[133] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.NAK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 134 && b[134] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.ACK})
	if err := <-errCh; err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// --- Byte-exact golden dump -------------------------------------------------

func TestCrossLangGolden_EveryByte(t *testing.T) {
	server, client := testutil.Pair(t)
	payload := make([]byte, 256)
	for i := 0; i < 256; i++ {
		payload[i] = byte(i)
	}
	cap := testutil.Start(client)
	errCh := runSend(server, payload, xmodem.Config{ReadTimeout: 2 * time.Second})
	mustWrite(t, client, []byte{xmodem.CRC})

	cap.WaitFor(t, func(b []byte) bool { return len(b) >= 133 }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.ACK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) >= 266 }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.ACK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 266 && b[266] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.NAK})
	cap.WaitFor(t, func(b []byte) bool { return len(b) > 267 && b[267] == xmodem.EOT }, 2*time.Second)
	mustWrite(t, client, []byte{xmodem.ACK})
	if err := <-errCh; err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Small settle before reading the capture.
	time.Sleep(30 * time.Millisecond)

	got := cap.Bytes()
	want, err := os.ReadFile(filepath.Join("..", "..", "test", "golden", "xmodem-every-byte-crc.bin"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("wire bytes differ from committed golden:\n%s", testutil.FirstDiff(want, got))
	}
}

// --- Helpers ---------------------------------------------------------------

func mustWrite(t *testing.T, w io.Writer, b []byte) {
	t.Helper()
	if _, err := w.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
}


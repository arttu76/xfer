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
	want, err := os.ReadFile(filepath.Join("testdata", "xmodem-every-byte-crc.bin"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("wire bytes differ from committed golden:\n%s", testutil.FirstDiff(want, got))
	}
}

// --- Receive: CRC happy path -----------------------------------------------

func TestReceiveCRCSingleBlock(t *testing.T) {
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	resCh := runReceive(server, xmodem.Config{ReadTimeout: 2 * time.Second})

	cap.WaitFor(t, hasByte(xmodem.CRC), 2*time.Second)
	mustWrite(t, client, buildCrcPacket(1, []byte("hello world!")))
	cap.WaitFor(t, countAtLeast(xmodem.ACK, 1), 2*time.Second)

	mustWrite(t, client, []byte{xmodem.EOT})
	cap.WaitFor(t, countAtLeast(xmodem.NAK, 1), 2*time.Second)
	mustWrite(t, client, []byte{xmodem.EOT})
	cap.WaitFor(t, countAtLeast(xmodem.ACK, 2), 2*time.Second)

	res := <-resCh
	if res.err != nil {
		t.Fatalf("Receive: %v", res.err)
	}
	if !bytes.Equal(res.data, []byte("hello world!")) {
		t.Fatalf("payload mismatch: %q", res.data)
	}
}

// --- Receive: SUB padding strip --------------------------------------------

func TestReceiveStripsSUBPadding(t *testing.T) {
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	resCh := runReceive(server, xmodem.Config{ReadTimeout: 2 * time.Second})

	cap.WaitFor(t, hasByte(xmodem.CRC), 2*time.Second)
	// Five real bytes followed by 123 SUB bytes inside the 128-byte block.
	mustWrite(t, client, buildCrcPacket(1, []byte("hello")))
	cap.WaitFor(t, countAtLeast(xmodem.ACK, 1), 2*time.Second)
	mustWrite(t, client, []byte{xmodem.EOT})
	cap.WaitFor(t, countAtLeast(xmodem.NAK, 1), 2*time.Second)
	mustWrite(t, client, []byte{xmodem.EOT})

	res := <-resCh
	if res.err != nil {
		t.Fatalf("Receive: %v", res.err)
	}
	if !bytes.Equal(res.data, []byte("hello")) {
		t.Fatalf("expected SUB-stripped payload, got %x", res.data)
	}
}

// --- Receive: multi-block --------------------------------------------------

func TestReceiveMultiBlock(t *testing.T) {
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	resCh := runReceive(server, xmodem.Config{ReadTimeout: 3 * time.Second})

	cap.WaitFor(t, hasByte(xmodem.CRC), 2*time.Second)

	payload := make([]byte, 384)
	for i := range payload {
		payload[i] = byte(i)
	}
	for blk := 1; blk <= 3; blk++ {
		mustWrite(t, client, buildCrcPacket(byte(blk), payload[(blk-1)*128:blk*128]))
		cap.WaitFor(t, countAtLeast(xmodem.ACK, blk), 2*time.Second)
	}
	mustWrite(t, client, []byte{xmodem.EOT})
	cap.WaitFor(t, countAtLeast(xmodem.NAK, 1), 2*time.Second)
	mustWrite(t, client, []byte{xmodem.EOT})

	res := <-resCh
	if res.err != nil {
		t.Fatalf("Receive: %v", res.err)
	}
	if !bytes.Equal(res.data, payload) {
		t.Fatalf("payload mismatch (len got=%d want=%d)", len(res.data), len(payload))
	}
}

// --- Receive: bad-CRC retransmit -------------------------------------------

func TestReceiveNAKsBadCRC(t *testing.T) {
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	resCh := runReceive(server, xmodem.Config{ReadTimeout: 2 * time.Second})

	cap.WaitFor(t, hasByte(xmodem.CRC), 2*time.Second)

	// Build a packet with the CRC corrupted.
	good := buildCrcPacket(1, []byte("payload"))
	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 0xff
	mustWrite(t, client, bad)
	cap.WaitFor(t, countAtLeast(xmodem.NAK, 1), 2*time.Second)

	// Resend with the correct CRC — must be accepted.
	mustWrite(t, client, good)
	cap.WaitFor(t, countAtLeast(xmodem.ACK, 1), 2*time.Second)

	mustWrite(t, client, []byte{xmodem.EOT})
	cap.WaitFor(t, countAtLeast(xmodem.NAK, 2), 2*time.Second)
	mustWrite(t, client, []byte{xmodem.EOT})

	res := <-resCh
	if res.err != nil {
		t.Fatalf("Receive: %v", res.err)
	}
	if !bytes.Equal(res.data, []byte("payload")) {
		t.Fatalf("got %q", res.data)
	}
}

// --- Receive: duplicate retransmit (sender resends previous block) ---------

func TestReceiveDuplicateBlockACKedNotStored(t *testing.T) {
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	resCh := runReceive(server, xmodem.Config{ReadTimeout: 2 * time.Second})

	cap.WaitFor(t, hasByte(xmodem.CRC), 2*time.Second)

	mustWrite(t, client, buildCrcPacket(1, []byte("aaa")))
	cap.WaitFor(t, countAtLeast(xmodem.ACK, 1), 2*time.Second)
	// Retransmit of block 1 (sender thinks ACK was lost). Must be ACKed
	// without doubling the data.
	mustWrite(t, client, buildCrcPacket(1, []byte("aaa")))
	cap.WaitFor(t, countAtLeast(xmodem.ACK, 2), 2*time.Second)
	mustWrite(t, client, buildCrcPacket(2, []byte("bbb")))
	cap.WaitFor(t, countAtLeast(xmodem.ACK, 3), 2*time.Second)

	mustWrite(t, client, []byte{xmodem.EOT})
	cap.WaitFor(t, countAtLeast(xmodem.NAK, 1), 2*time.Second)
	mustWrite(t, client, []byte{xmodem.EOT})

	res := <-resCh
	if res.err != nil {
		t.Fatalf("Receive: %v", res.err)
	}
	want := append(append([]byte("aaa"), bytes.Repeat([]byte{xmodem.SUB}, 0)...), []byte("aaa")...)
	_ = want
	// Block 1 ("aaa" + 125 SUB) once + block 2 ("bbb" + 125 SUB), then SUB
	// padding gets stripped from the tail. So we expect first block's full
	// 128 bytes (real "aaa" + SUB padding intact, since trailing SUB strip
	// only consumes the tail) followed by "bbb".
	expected := make([]byte, 0, 128+3)
	expected = append(expected, 'a', 'a', 'a')
	for i := 0; i < 125; i++ {
		expected = append(expected, xmodem.SUB)
	}
	expected = append(expected, 'b', 'b', 'b')
	if !bytes.Equal(res.data, expected) {
		t.Fatalf("payload mismatch:\nwant %x\ngot  %x", expected, res.data)
	}
}

// --- Receive: checksum-mode fallback ---------------------------------------

func TestReceiveChecksumModeFallback(t *testing.T) {
	// Drain the initial CRC kicks for ~6 seconds without responding, so the
	// receiver gives up and falls through to NAK (checksum). Then send a
	// checksum-format packet.
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	resCh := runReceive(server, xmodem.Config{ReadTimeout: 5 * time.Second})

	// Wait for the receiver to switch to NAK mode.
	cap.WaitFor(t, countAtLeast(xmodem.NAK, 1), 12*time.Second)
	mustWrite(t, client, buildChecksumPacket(1, []byte("ck")))
	cap.WaitFor(t, countAtLeast(xmodem.ACK, 1), 2*time.Second)

	mustWrite(t, client, []byte{xmodem.EOT})
	cap.WaitFor(t, countAtLeast(xmodem.NAK, 2), 2*time.Second)
	mustWrite(t, client, []byte{xmodem.EOT})

	res := <-resCh
	if res.err != nil {
		t.Fatalf("Receive: %v", res.err)
	}
	if !bytes.Equal(res.data, []byte("ck")) {
		t.Fatalf("got %q", res.data)
	}
}

// --- Helpers ---------------------------------------------------------------

func mustWrite(t *testing.T, w io.Writer, b []byte) {
	t.Helper()
	if _, err := w.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

type recvResult struct {
	data []byte
	err  error
}

func runReceive(server net.Conn, cfg xmodem.Config) <-chan recvResult {
	ch := make(chan recvResult, 1)
	go func() {
		d, err := xmodem.Receive(server, cfg)
		ch <- recvResult{d, err}
	}()
	return ch
}

func buildCrcPacket(blk byte, data []byte) []byte {
	chunk := make([]byte, 128)
	n := copy(chunk, data)
	for i := n; i < 128; i++ {
		chunk[i] = xmodem.SUB
	}
	out := []byte{xmodem.SOH, blk, ^blk}
	out = append(out, chunk...)
	c := xmodem.CRC16Ccitt(chunk)
	return append(out, byte(c>>8), byte(c&0xff))
}

func buildChecksumPacket(blk byte, data []byte) []byte {
	chunk := make([]byte, 128)
	n := copy(chunk, data)
	for i := n; i < 128; i++ {
		chunk[i] = xmodem.SUB
	}
	out := []byte{xmodem.SOH, blk, ^blk}
	out = append(out, chunk...)
	var sum byte
	for _, b := range chunk {
		sum += b
	}
	return append(out, sum)
}

func hasByte(target byte) func([]byte) bool {
	return func(b []byte) bool {
		for _, x := range b {
			if x == target {
				return true
			}
		}
		return false
	}
}

func countAtLeast(target byte, n int) func([]byte) bool {
	return func(b []byte) bool {
		c := 0
		for _, x := range b {
			if x == target {
				c++
			}
		}
		return c >= n
	}
}


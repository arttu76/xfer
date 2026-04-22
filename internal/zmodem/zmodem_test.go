package zmodem_test

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/solvalou/xfer/internal/testutil"
	"github.com/solvalou/xfer/internal/zmodem"
)

// runSend spawns SendBuffer in a goroutine. Returns a channel for the error.
func runSend(conn net.Conn, data []byte, name string) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- zmodem.SendBuffer(conn, data, name) }()
	return ch
}

// pinClock freezes the clock used by sender for deterministic ZFILE mtime.
func pinClock(t *testing.T, unix int64) {
	t.Helper()
	restore := zmodem.SetNow(func() time.Time { return time.Unix(unix, 0) })
	t.Cleanup(restore)
}

// Pinned to an arbitrary but recognizable octal value so the ZFILE mtime
// field in the fileinfo subpacket is deterministic across test runs.
const fixedEpoch int64 = 0o17200000000

// --- Receiver-side frame helpers ------------------------------------------

func zrinit(flags byte) []byte {
	// ZRINIT on-wire payload: [frame, ZF3, ZF2, ZF1, ZF0]. Capability bits
	// (CANFDX / CANOVIO / ESCCTL / ...) live in ZF0, the trailing byte.
	// Buffer-size 0 is fine for our flow.
	payload := []byte{zmodem.FrameZRINIT, 0x00, 0x00, 0x00, flags}
	crc := zmodem.CRC16(payload)
	full := append(payload, byte(crc>>8), byte(crc))
	hex := make([]byte, 0, 16)
	const d = "0123456789abcdef"
	for _, b := range full {
		hex = append(hex, d[b>>4], d[b&0xf])
	}
	out := []byte{zmodem.ZPAD, zmodem.ZPAD, zmodem.ZDLE, zmodem.ZHEX}
	out = append(out, hex...)
	out = append(out, 0x0d, 0x8a, zmodem.XON)
	return out
}

func zrpos(offset uint32) []byte {
	return zmodem.BuildZhexHeader(zmodem.FrameZRPOS, offset)
}

func zack(count uint32) []byte {
	return zmodem.BuildZhexHeader(zmodem.FrameZACK, count)
}

func zfin() []byte {
	return zmodem.BuildZhexHeader(zmodem.FrameZFIN, 0)
}

// Capability bytes observed in real ZRINIT frames:
//   0x23 — Term 4.8 on Amiga, and lrzsz without --escape
//   0x63 — lrzsz with --escape (adds the ESCCTL bit 0x40)
const (
	capsTerm48      = 0x23
	capsLrzszEscape = 0x63
)

var zrqinitPrefix = []byte{zmodem.ZPAD, zmodem.ZPAD, zmodem.ZDLE, zmodem.ZHEX}
var rzTrigger = []byte("rz\r")

// --- Tests ------------------------------------------------------------------

func TestPreamble(t *testing.T) {
	pinClock(t, fixedEpoch)
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	errCh := runSend(server, []byte("x"), "a.txt")

	cap.WaitFor(t, func(b []byte) bool { return len(b) >= 3+len(zrqinitPrefix) }, 2*time.Second)
	got := cap.Bytes()
	if !bytes.Equal(got[:3], rzTrigger) {
		t.Fatalf("preamble: got %x want %x", got[:3], rzTrigger)
	}
	if !bytes.Equal(got[3:3+4], zrqinitPrefix) {
		t.Fatalf("post-trigger prefix: got %x", got[3:3+4])
	}
	// Cancel to end.
	_, _ = client.Write(bytes.Repeat([]byte{0x18}, 8))
	if err := <-errCh; !errors.Is(err, zmodem.ErrCancelled) {
		t.Fatalf("expected ErrCancelled, got %v", err)
	}
}

func TestCancelFiveCAN(t *testing.T) {
	pinClock(t, fixedEpoch)
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	errCh := runSend(server, []byte("x"), "a.txt")

	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, zrqinitPrefix) }, 2*time.Second)
	_, _ = client.Write([]byte{0x18, 0x18, 0x18, 0x18, 0x18})
	if err := <-errCh; !errors.Is(err, zmodem.ErrCancelled) {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
	want := []byte{
		0x18, 0x18, 0x18, 0x18, 0x18, 0x18, 0x18, 0x18,
		0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08,
	}
	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, want) }, 1*time.Second)
}

func TestZfileLrzszFileinfo(t *testing.T) {
	pinClock(t, fixedEpoch)
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	errCh := runSend(server, bytes.Repeat([]byte{0x42}, 4321), "my file.bin")

	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, zrqinitPrefix) }, 2*time.Second)
	_, _ = client.Write(zrinit(capsTerm48))
	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, []byte{zmodem.ZDLE, zmodem.ZCRCW}) }, 2*time.Second)

	info := extractZfilePayload(t, cap.Bytes())
	nul1 := bytes.IndexByte(info, 0x00)
	if nul1 < 0 {
		t.Fatalf("no first NUL in fileinfo: %x", info)
	}
	name := string(info[:nul1])
	nul2 := bytes.IndexByte(info[nul1+1:], 0x00)
	if nul2 < 0 {
		t.Fatalf("no second NUL")
	}
	meta := string(info[nul1+1 : nul1+1+nul2])
	if name != "my file.bin" {
		t.Fatalf("name: %q", name)
	}
	// Octal-encoded mtime.
	wantMeta := "4321 17200000000 100644 0 1 4321"
	if meta != wantMeta {
		t.Fatalf("meta:\n got %q\nwant %q", meta, wantMeta)
	}

	_, _ = client.Write(bytes.Repeat([]byte{0x18}, 8))
	_ = <-errCh
}

func TestSubpacketSizing(t *testing.T) {
	pinClock(t, fixedEpoch)
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	errCh := runSend(server, data, "f.bin")

	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, zrqinitPrefix) }, 2*time.Second)
	_, _ = client.Write(zrinit(capsTerm48))
	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, []byte{zmodem.ZDLE, zmodem.ZCRCW}) }, 2*time.Second)
	_, _ = client.Write(zrpos(0))

	// Wait for 2 ZCRCWs (ZFILE + final ZDATA).
	cap.WaitFor(t, func(b []byte) bool { return countTerm(b, zmodem.ZCRCW) >= 2 }, 2*time.Second)
	time.Sleep(50 * time.Millisecond)

	out := cap.Bytes()
	if g := countTerm(out, zmodem.ZCRCG); g != 2 {
		t.Fatalf("ZCRCG count: got %d want 2", g)
	}
	if w := countTerm(out, zmodem.ZCRCW); w != 2 {
		t.Fatalf("ZCRCW count: got %d want 2", w)
	}

	_, _ = client.Write(bytes.Repeat([]byte{0x18}, 8))
	_ = <-errCh
}

func TestAckPacing(t *testing.T) {
	pinClock(t, fixedEpoch)
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	data := make([]byte, 1024*10)
	for i := range data {
		data[i] = byte(i)
	}
	errCh := runSend(server, data, "big.bin")

	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, zrqinitPrefix) }, 2*time.Second)
	_, _ = client.Write(zrinit(capsTerm48))
	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, []byte{zmodem.ZDLE, zmodem.ZCRCW}) }, 2*time.Second)
	_, _ = client.Write(zrpos(0))

	cap.WaitFor(t, func(b []byte) bool { return countTerm(b, zmodem.ZCRCW) >= 2 }, 3*time.Second)
	time.Sleep(50 * time.Millisecond)

	out := cap.Bytes()
	if g := countTerm(out, zmodem.ZCRCG); g != 7 {
		t.Fatalf("first burst ZCRCG: got %d want 7", g)
	}
	if w := countTerm(out, zmodem.ZCRCW); w != 2 {
		t.Fatalf("first burst ZCRCW: got %d want 2", w)
	}

	_, _ = client.Write(bytes.Repeat([]byte{0x18}, 8))
	_ = <-errCh
}

func TestZrposResume(t *testing.T) {
	pinClock(t, fixedEpoch)
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)
	data := make([]byte, 2560)
	for i := range data {
		data[i] = byte(i)
	}
	errCh := runSend(server, data, "resume.bin")

	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, zrqinitPrefix) }, 2*time.Second)
	_, _ = client.Write(zrinit(capsTerm48))
	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, []byte{zmodem.ZDLE, zmodem.ZCRCW}) }, 2*time.Second)
	_, _ = client.Write(zrpos(1500))

	cap.WaitFor(t, func(b []byte) bool {
		return bytes.Contains(b, []byte{zmodem.ZPAD, zmodem.ZDLE, zmodem.ZBIN, zmodem.FrameZDATA})
	}, 2*time.Second)
	time.Sleep(40 * time.Millisecond)

	out := cap.Bytes()
	hdrIdx := bytes.Index(out, []byte{zmodem.ZPAD, zmodem.ZDLE, zmodem.ZBIN, zmodem.FrameZDATA})
	if hdrIdx < 0 {
		t.Fatalf("ZDATA header missing")
	}
	// Unescape next 4 payload bytes (offset LE).
	offBytes := unescapeN(out[hdrIdx+4:], 4)
	got := uint32(offBytes[0]) | uint32(offBytes[1])<<8 | uint32(offBytes[2])<<16 | uint32(offBytes[3])<<24
	if got != 1500 {
		t.Fatalf("offset: got %d want 1500", got)
	}

	_, _ = client.Write(bytes.Repeat([]byte{0x18}, 8))
	_ = <-errCh
}

func TestSocketDisconnectCleansUp(t *testing.T) {
	pinClock(t, fixedEpoch)
	server, client := testutil.Pair(t)
	errCh := runSend(server, bytes.Repeat([]byte{0x41}, 500), "f.bin")
	time.Sleep(30 * time.Millisecond)
	_ = client.Close()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("expected error on client disconnect")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Send did not return after client close")
	}
}

// --- Full-session golden --------------------

func TestFullSessionGolden_1025(t *testing.T) {
	pinClock(t, fixedEpoch)
	server, client := testutil.Pair(t)
	cap := testutil.Start(client)

	payload := make([]byte, 1025)
	for i := range payload {
		payload[i] = byte((i*37 + 13) & 0xff)
	}
	errCh := runSend(server, payload, "boundary.bin")

	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, zrqinitPrefix) }, 2*time.Second)
	_, _ = client.Write(zrinit(capsTerm48))

	cap.WaitFor(t, func(b []byte) bool { return bytes.Contains(b, []byte{zmodem.ZDLE, zmodem.ZCRCW}) }, 2*time.Second)
	_, _ = client.Write(zrpos(0))

	cap.WaitFor(t, func(b []byte) bool { return countTerm(b, zmodem.ZCRCW) >= 2 }, 4*time.Second)
	_, _ = client.Write(zack(uint32(len(payload))))

	cap.WaitFor(t, func(b []byte) bool {
		return bytes.Contains(b, []byte{zmodem.ZPAD, zmodem.ZDLE, zmodem.ZBIN, zmodem.FrameZEOF})
	}, 4*time.Second)
	_, _ = client.Write(zrinit(capsTerm48))

	cap.WaitFor(t, func(b []byte) bool {
		idx := 0
		for {
			i := bytes.Index(b[idx:], []byte{zmodem.ZPAD, zmodem.ZPAD, zmodem.ZDLE, zmodem.ZHEX})
			if i < 0 {
				return false
			}
			if i+6 <= len(b[idx:]) && bytes.Equal(b[idx+i+4:idx+i+6], []byte("08")) {
				return true
			}
			idx += i + 4
		}
	}, 4*time.Second)
	_, _ = client.Write(zfin())

	if err := <-errCh; err != nil {
		t.Fatalf("Send: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	got := cap.Bytes()
	want, err := os.ReadFile(filepath.Join("..", "..", "test", "golden", "full-session-1025.bin"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("wire bytes differ from committed golden.\n%s", testutil.FirstDiff(want, got))
	}
}

// --- Helpers ---------------------------------------------------------------

func countTerm(buf []byte, kind byte) int {
	count := 0
	for i := 0; i < len(buf)-1; i++ {
		if buf[i] == zmodem.ZDLE && buf[i+1] == kind {
			count++
			i++ // skip the terminator byte
		}
	}
	return count
}

func extractZfilePayload(t *testing.T, buf []byte) []byte {
	t.Helper()
	// Header prefix for ZFILE: ZPAD ZDLE 'A' 0x04
	hdr := []byte{zmodem.ZPAD, zmodem.ZDLE, zmodem.ZBIN, zmodem.FrameZFILE}
	i := bytes.Index(buf, hdr)
	if i < 0 {
		t.Fatalf("ZFILE header not found")
	}
	// Fixed ZFILE header: 3 prefix + 1 frame + 4 flags (unescaped) + 2 CRC = 10 bytes.
	start := i + 10
	term := bytes.Index(buf[start:], []byte{zmodem.ZDLE, zmodem.ZCRCW})
	if term < 0 {
		t.Fatalf("ZFILE terminator not found")
	}
	return buf[start : start+term]
}

func unescapeN(buf []byte, n int) []byte {
	out := make([]byte, 0, n)
	for i := 0; len(out) < n && i < len(buf); {
		if buf[i] == zmodem.ZDLE {
			if i+1 >= len(buf) {
				break
			}
			out = append(out, buf[i+1]^0x40)
			i += 2
		} else {
			out = append(out, buf[i])
			i++
		}
	}
	return out
}


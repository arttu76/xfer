package kermit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/solvalou/xfer/internal/testutil"
)

// ---------- Unit tests ----------

func TestCRC16KermitKnown(t *testing.T) {
	// CRC-16/KERMIT("123456789") = 0x2189 per crccalc.com's reference and
	// E-Kermit/G-Kermit sources (reflected CCITT, poly 0x8408, init 0).
	got := crc16Kermit([]byte("123456789"))
	if got != 0x2189 {
		t.Fatalf("crc16Kermit(\"123456789\") = 0x%04x, want 0x2189", got)
	}
}

func TestEncodeDataRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		in      []byte
		use8bit bool
		useRept bool
	}{
		{"plain 7-bit", []byte("Hello, world!"), false, false},
		{"control chars", []byte{0x00, 0x01, 0x1f, 0x7f}, false, false},
		{"8-bit", []byte{0x80, 0xff, 0x81, 0x20, '#', '&', '~'}, true, false},
		{"RLE-eligible run", bytes.Repeat([]byte{'A'}, 30), false, true},
		{"RLE ctrl run", bytes.Repeat([]byte{0x00}, 10), false, true},
		{"RLE 8-bit run", bytes.Repeat([]byte{0xff}, 20), true, true},
		{"mixed", append(append([]byte{}, bytes.Repeat([]byte{'X'}, 8)...), []byte{'a', 'b', 0x00, 0x80, '~', '#', '&'}...), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded := encodeData(tc.in, '#', '&', '~', tc.use8bit, tc.useRept)
			for j, b := range encoded {
				if b < 0x20 || b > 0x7e {
					t.Fatalf("byte %d = 0x%02x is not printable", j, b)
				}
			}
			back := decodeData(encoded, '#', '&', '~', tc.use8bit, tc.useRept)
			if !bytes.Equal(back, tc.in) {
				t.Fatalf("round-trip mismatch\n  in:      %x\n  encoded: %s\n  out:     %x", tc.in, encoded, back)
			}
		})
	}
}

// decodeData is the receive-side inverse of encodeData. Kept here because
// production code never needs to decode its own sent data.
func decodeData(in []byte, qctl, qbin, reptCh byte, use8bit, useRept bool) []byte {
	out := make([]byte, 0, len(in))
	i := 0
	for i < len(in) {
		// RLE?
		if useRept && in[i] == reptCh {
			i++ // consume '~'
			if i >= len(in) {
				break
			}
			runLen := int(unchar(in[i]))
			i++
			c, consumed := decodeOne(in[i:], qctl, qbin, use8bit)
			i += consumed
			for k := 0; k < runLen; k++ {
				out = append(out, c)
			}
			continue
		}
		c, consumed := decodeOne(in[i:], qctl, qbin, use8bit)
		i += consumed
		out = append(out, c)
	}
	return out
}

// decodeOne decodes one logical source byte from in[0:]; returns the byte
// and the number of input bytes consumed.
func decodeOne(in []byte, qctl, qbin byte, use8bit bool) (byte, int) {
	if len(in) == 0 {
		return 0, 0
	}
	i := 0
	high := false
	if use8bit && in[i] == qbin {
		high = true
		i++
		if i >= len(in) {
			return 0, i
		}
	}
	c := in[i]
	i++
	if c == qctl {
		if i >= len(in) {
			return 0, i
		}
		n := in[i]
		i++
		// Kermit quoting: after QCTL, `ctl(x) = x ^ 0x40` decodes back to a
		// control character iff the result is in the control range. Any
		// other printable stands for itself (covers quoted QCTL, QBIN, REPT).
		dec := n ^ 0x40
		if dec < 0x20 || dec == 0x7f {
			c = dec
		} else {
			c = n
		}
	}
	if high {
		c |= 0x80
	}
	return c, i
}

// ---------- Loopback tests ----------

type connAdapter struct{ net.Conn }

func (c connAdapter) SetReadDeadline(t time.Time) error { return c.Conn.SetReadDeadline(t) }

type recvParams struct {
	chkt     byte // '1' '2' '3'
	qbin     byte // 'N' 'Y' or '&'
	rept     byte // ' ' or '~'
	windo    byte // 1..31
	supLong  bool // advertise long-packet CAPAS bit
	supAttrs bool // advertise attribute-packet CAPAS bit
	maxl     byte // 94 typically
	maxlongX int  // our accepted long size in bytes (for long packets); 0 = none
}

type recvResult struct {
	data     []byte
	filename string
	attrSize string // as reported via '!' attribute (empty if no A packet seen)
	attrType byte   // as reported via '"' attribute ('A'/'B', 0 if none)
	err      error
}

func runReceiver(conn net.Conn, rp recvParams) recvResult {
	var out bytes.Buffer
	var fname string
	var attrSize string
	var attrType byte

	// Parameters we're going to agree on — start as type-1 until S negotiation.
	chkLen := 1
	use8bit := false
	qctl := byte('#')
	qbin := byte('&')
	reptCh := byte('~')
	useRept := false

	for {
		p, err := recvReadPacket(conn, chkLen, 3*time.Second)
		if err != nil {
			return recvResult{err: fmt.Errorf("read: %w", err)}
		}
		switch p.typ {
		case 'S':
			// Apply our desired receive-side params for the rest of the session.
			switch rp.chkt {
			case '2':
				chkLen = 2
			case '3':
				chkLen = 3
			default:
				chkLen = 1
			}
			use8bit = rp.qbin != 'N'
			useRept = rp.rept == '~'
			maxl := rp.maxl
			if maxl == 0 {
				maxl = 94
			}
			var capas byte
			if rp.supLong {
				capas |= 0x02
			}
			if rp.supAttrs {
				capas |= 0x08
			}
			maxlx1, maxlx2 := byte(0), byte(0)
			if rp.maxlongX > 0 {
				maxlx1 = byte(rp.maxlongX / 95)
				maxlx2 = byte(rp.maxlongX % 95)
			}
			reply := []byte{
				tochar(maxl),
				tochar(5),
				tochar(0),
				ctl(0),
				tochar(CR),
				'#',
				rp.qbin,
				rp.chkt,
				rp.rept,
				tochar(capas),
				tochar(rp.windo),
				tochar(maxlx1),
				tochar(maxlx2),
			}
			// ACK for S is still type-1 (check type only applies to subsequent
			// packets — the S exchange uses type-1 by spec).
			if err := sendAckRaw(conn, p.seq, 'Y', reply, 1); err != nil {
				return recvResult{err: err}
			}
		case 'F':
			fname = string(p.data)
			if err := sendAckRaw(conn, p.seq, 'Y', nil, chkLen); err != nil {
				return recvResult{err: err}
			}
		case 'A':
			attrSize, attrType = parseAttributes(p.data)
			if err := sendAckRaw(conn, p.seq, 'Y', nil, chkLen); err != nil {
				return recvResult{err: err}
			}
		case 'E':
			return recvResult{err: fmt.Errorf("sender aborted: %s", string(p.data))}
		case 'D':
			chunk := decodeData(p.data, qctl, qbin, reptCh, use8bit, useRept)
			out.Write(chunk)
			if err := sendAckRaw(conn, p.seq, 'Y', nil, chkLen); err != nil {
				return recvResult{err: err}
			}
		case 'Z':
			if err := sendAckRaw(conn, p.seq, 'Y', nil, chkLen); err != nil {
				return recvResult{err: err}
			}
		case 'B':
			_ = sendAckRaw(conn, p.seq, 'Y', nil, chkLen)
			return recvResult{data: out.Bytes(), filename: fname, attrSize: attrSize, attrType: attrType}
		default:
			return recvResult{err: fmt.Errorf("unknown packet type %q", p.typ)}
		}
	}
}

// parseAttributes walks an A-packet data field and returns the '!' (size)
// and '"' (type) attribute values. Other attributes are skipped.
func parseAttributes(data []byte) (size string, ftype byte) {
	i := 0
	for i < len(data) {
		tag := data[i]
		i++
		if i >= len(data) {
			break
		}
		length := int(unchar(data[i]))
		i++
		if i+length > len(data) {
			break
		}
		val := data[i : i+length]
		i += length
		switch tag {
		case '!':
			size = string(val)
		case '"':
			if len(val) >= 1 {
				ftype = val[0]
			}
		}
	}
	return
}

func sendAckRaw(conn net.Conn, seq int, typ byte, data []byte, chkLen int) error {
	pkt := buildShortPacket(seq, typ, data, chkLen)
	_, err := conn.Write(pkt)
	return err
}

// recvReadPacket is the test-side mirror of readPacket. Duplicated because
// we want to exercise real net.Conn behavior without leaning on production code.
func recvReadPacket(conn net.Conn, chkLen int, timeout time.Duration) (*packet, error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	return readPacket(connAdapter{conn}, chkLen)
}

func TestSendReceiveLoopback(t *testing.T) {
	scenarios := []struct {
		name string
		rp   recvParams
	}{
		{"minimal (type-1, 7-bit, no rle, no windowing)", recvParams{
			chkt: '1', qbin: 'N', rept: ' ', windo: 1, supLong: false, maxl: 94,
		}},
		{"type-3 CRC, 8-bit", recvParams{
			chkt: '3', qbin: '&', rept: ' ', windo: 1, supLong: false, maxl: 94,
		}},
		{"RLE enabled", recvParams{
			chkt: '3', qbin: '&', rept: '~', windo: 1, supLong: false, maxl: 94,
		}},
		{"sliding window 4", recvParams{
			chkt: '3', qbin: '&', rept: '~', windo: 4, supLong: false, maxl: 94,
		}},
		{"long packets 1000", recvParams{
			chkt: '3', qbin: '&', rept: '~', windo: 4, supLong: true, maxl: 94, maxlongX: 1003,
		}},
		{"the works (all features)", recvParams{
			chkt: '3', qbin: '&', rept: '~', windo: 8, supLong: true, maxl: 94, maxlongX: 1003,
		}},
		{"attributes supported", recvParams{
			chkt: '3', qbin: '&', rept: '~', windo: 8, supLong: true, supAttrs: true, maxl: 94, maxlongX: 1003,
		}},
	}

	payloads := []struct {
		name string
		data []byte
	}{
		{"small text", []byte("Hello, Kermit!\nSecond line.\n")},
		{"medium binary", func() []byte {
			b := make([]byte, 4096)
			for i := range b {
				b[i] = byte(i * 37)
			}
			return b
		}()},
		{"runs for RLE", bytes.Repeat([]byte{0x00}, 2000)},
		{"empty", []byte{}},
		{"large binary", func() []byte {
			b := make([]byte, 50_000)
			for i := range b {
				b[i] = byte(i*13 + (i >> 3))
			}
			return b
		}()},
	}

	for _, sc := range scenarios {
		for _, pl := range payloads {
			t.Run(sc.name+"/"+pl.name, func(t *testing.T) {
				runLoopback(t, sc.rp, pl.data)
			})
		}
	}
}

func runLoopback(t *testing.T, rp recvParams, data []byte) {
	t.Helper()
	server, client := testutil.Pair(t)

	done := make(chan error, 1)
	received := make(chan recvResult, 1)

	go func() {
		received <- runReceiver(client, rp)
	}()
	go func() {
		done <- Send(connAdapter{server}, data, "TEST.BIN", Config{ReadTimeout: 2 * time.Second})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Send returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Send timed out")
	}

	select {
	case r := <-received:
		if r.err != nil {
			t.Fatalf("receiver error: %v", r.err)
		}
		expected := data
		if rp.qbin == 'N' {
			// 7-bit link lose high bits by design — the receiver reconstructs
			// 7-bit data from a (possibly 8-bit) source. Compare stripped.
			expected = make([]byte, len(data))
			for i, b := range data {
				expected[i] = b & 0x7f
			}
		}
		if !bytes.Equal(r.data, expected) {
			t.Fatalf("file mismatch\n  want (%d bytes)\n  got  (%d bytes)\n  first diff: %s",
				len(expected), len(r.data), testutil.FirstDiff(expected, r.data))
		}
		if r.filename != "TEST.BIN" {
			t.Fatalf("filename: want TEST.BIN got %q", r.filename)
		}
		if rp.supAttrs {
			if r.attrSize != fmt.Sprintf("%d", len(data)) {
				t.Fatalf("attr size: want %d got %q", len(data), r.attrSize)
			}
			// Binary-content test data → 'B', text-content → 'A'.
			wantType := byte('B')
			if detectTextFile(data) {
				wantType = 'A'
			}
			if r.attrType != wantType {
				t.Fatalf("attr type: want %c got %c", wantType, r.attrType)
			}
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("receiver timed out")
	}
}

// TestAbortSendsEPacket: force the sender into retry exhaustion and
// verify it emits an E packet with a readable reason before returning.
func TestAbortSendsEPacket(t *testing.T) {
	server, client := testutil.Pair(t)

	// Capture everything the sender writes to the wire.
	cap := testutil.Start(client)

	// Simulated receiver that ACKs S, F, then silently drops D packets.
	// Sender will retransmit until retry limit, then abort with E.
	go func() {
		_, _ = recvReadPacket(client, 1, 2*time.Second) // S
		_ = sendAckRaw(client, 0, 'Y', []byte{
			tochar(94), tochar(5), tochar(0), ctl(0), tochar(CR),
			'#', '&', '3', '~', tochar(0), tochar(1), tochar(0), tochar(0),
		}, 1)
		_, _ = recvReadPacket(client, 3, 2*time.Second) // F
		_ = sendAckRaw(client, 1, 'Y', nil, 3)
		// Now just read and discard; don't ACK D packets.
		for {
			if _, err := recvReadPacket(client, 3, 2*time.Second); err != nil {
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- Send(connAdapter{server}, []byte("this will not make it"), "F.BIN", Config{ReadTimeout: 100 * time.Millisecond})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Send never returned")
	}

	// Wait briefly to let the E packet hit the wire.
	time.Sleep(100 * time.Millisecond)
	got := cap.Bytes()
	// Scan for an E packet (SOH LEN SEQ 'E' ...).
	found := false
	for i := 0; i+3 < len(got); i++ {
		if got[i]&0x7f == SOH && got[i+3]&0x7f == 'E' {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no E packet in wire capture (%d bytes): %x", len(got), got)
	}
}

// Retransmit test: drop the first D packet on the wire; sender must
// recover via either NAK or timeout-driven retransmit.
func TestRetransmitOnDrop(t *testing.T) {
	server, client := testutil.Pair(t)

	rp := recvParams{chkt: '1', qbin: 'N', rept: ' ', windo: 1, supLong: false, maxl: 94}

	done := make(chan error, 1)
	received := make(chan recvResult, 1)

	// Wrap the client side to drop the first D packet seen on the wire.
	dropper := &dropFirstD{Conn: client}
	go func() {
		received <- runReceiver(dropper, rp)
	}()
	go func() {
		done <- Send(connAdapter{server}, []byte("recoverable payload"), "R.BIN", Config{ReadTimeout: 500 * time.Millisecond})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("timeout")
	}
	r := <-received
	if r.err != nil {
		t.Fatalf("receiver: %v", r.err)
	}
	if string(r.data) != "recoverable payload" {
		t.Fatalf("got %q", r.data)
	}
}

// dropFirstD wraps a net.Conn and corrupts the first D packet it sees on
// reads, so the receiver's check fails and sender is forced to retransmit.
type dropFirstD struct {
	net.Conn
	dropped bool
	buf     []byte // carry-over bytes after partial parse
}

func (d *dropFirstD) Read(p []byte) (int, error) {
	// Flush any carry-over first.
	if len(d.buf) > 0 {
		n := copy(p, d.buf)
		d.buf = d.buf[n:]
		return n, nil
	}
	n, err := d.Conn.Read(p)
	if n <= 0 || d.dropped {
		return n, err
	}
	// Scan buffer for SOH followed by LEN SEQ TYPE with TYPE='D'.
	for i := 0; i < n-3; i++ {
		if p[i]&0x7f == SOH && p[i+3]&0x7f == 'D' {
			// Corrupt TYPE so the check fails; receiver's read errors and
			// that counts as a drop (sender will retransmit via NAK or timeout).
			// Even simpler: replace the whole byte range with junk and let
			// receiver resync.
			p[i+3] = '?'
			d.dropped = true
			break
		}
	}
	return n, err
}

// ---------- sanity for edge cases in the parser ----------

func TestReadPacketNoise(t *testing.T) {
	var b bytes.Buffer
	b.Write([]byte{0x00, 0xff, 0x55}) // pre-SOH noise
	b.Write(buildShortPacket(3, 'Y', []byte("hi"), 1))

	r := &fakeConn{Buffer: &b}
	p, err := readPacket(r, 1)
	if err != nil {
		t.Fatalf("readPacket: %v", err)
	}
	if p.seq != 3 || p.typ != 'Y' || string(p.data) != "hi" {
		t.Fatalf("got %+v", p)
	}
}

type fakeConn struct{ *bytes.Buffer }

func (f *fakeConn) SetReadDeadline(time.Time) error { return nil }
func (f *fakeConn) Read(p []byte) (int, error) {
	n, err := f.Buffer.Read(p)
	if errors.Is(err, io.EOF) {
		return n, os.ErrDeadlineExceeded // pretend it's a timeout so best-effort EOL read doesn't error
	}
	return n, err
}

// Package kermit implements a send-only Kermit file transfer protocol.
//
// Supported features:
//   - Short and extended-length (long) packets
//   - Type-1 (8-bit sum), type-2 (12-bit sum), and type-3 (CRC-16) block checks
//   - 8-bit (&) and control (#) quoting
//   - Run-length encoding (~Xc form) for repeated bytes
//   - Sliding-window flow control (up to 31 packets in flight)
//
// What's negotiated is driven by the receiver's reply to our Send-Init (S)
// packet: we propose the richest feature set we support and fall back to
// whatever subset the peer agrees to.
package kermit

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------- Wire constants ----------

const (
	SOH = 0x01
	CR  = 0x0d
)

// Ceiling on any length byte encoded with tochar (94 is `~`).
const tocharMax = 94

// ---------- Sender configuration ----------

// Config lets callers tune timing and subscribe to progress.
type Config struct {
	ReadTimeout time.Duration     // default 10s; used for the initial handshake
	OnStart     func()            // called once we see the first ACK
	OnStatus    func(StatusEvent) // called after each D packet is ACKed
}

// StatusEvent reports cumulative bytes acknowledged.
type StatusEvent struct {
	BytesSent  int
	TotalBytes int
}

// deadlineConn mirrors the xmodem package's interface. Kept small so tests
// can supply pipe-backed conns without importing net.
type deadlineConn interface {
	io.ReadWriter
	SetReadDeadline(t time.Time) error
}

// ---------- Negotiated parameters ----------

// params carries both our proposal (pre-negotiation) and the resolved
// settings (post-negotiation). The resolved fields are what the data-path
// code reads.
type params struct {
	// proposal (bytes as they appear in the S packet data field)
	maxl, ptime, npad                   byte
	padc, eol                           byte
	qctl, qbin, chkt, rept              byte
	capas                               byte
	windo, maxlx1, maxlx2               byte

	// resolved by negotiate()
	use8bit  bool
	chkType  int // 1, 2, or 3
	chkLen   int // 1, 2, or 3
	useRept  bool
	reptCh   byte
	window   int  // 1 (no windowing) .. 31
	longMax  int  // max DATA bytes per long D packet; 0 → short only
	shortMax int  // max DATA bytes per short D packet (respects their MAXL)
	hasAttrs bool // receiver advertised attribute-packet support
}

// CAPAS bit values — match C-Kermit's (ckcmai.c:769..781):
//
//	lpcapb=2  Long Packets
//	swcapb=4  Sliding Windows
//	atcapb=8  Attribute packets
//	rscapb=16 RESEND
//	lscapb=32 Locking Shifts
const (
	capLongPackets = 0x02
	capSlidingWin  = 0x04
	capAttributes  = 0x08
)

// defaultSendParams builds the richest proposal we're willing to make.
// A receiver that supports everything will negotiate to: type-3 CRC, 8-bit
// quoting, RLE, 4-packet window, 1000-byte long packets.
func defaultSendParams() params {
	return params{
		maxl:   tocharMax, // we accept up to 94 in short packets
		ptime:  5,
		npad:   0,
		padc:   0x00,
		eol:    CR,
		qctl:   '#',
		qbin:   '&',
		chkt:   '3', // propose type-3 CRC
		rept:   '~', // propose RLE
		capas:  capLongPackets | capSlidingWin | capAttributes,
		windo:  8,    // propose 8-packet window
		maxlx1: 10,   // propose up to 10*95 + 50 = 1000 bytes / long packet
		maxlx2: 50,
	}
}

// negotiate updates the resolved fields in p from the receiver's ACK-S
// data field. Missing/garbled fields fall back to conservative defaults.
func negotiate(p *params, recv []byte) {
	// Defaults (conservative — what we get if the peer sends nothing).
	p.chkType = 1
	p.chkLen = 1
	p.use8bit = false
	p.useRept = false
	p.reptCh = '~'
	p.window = 1
	p.longMax = 0
	p.shortMax = 91 // 94 - SEQ(1) - TYPE(1) - CHECK(1, type-1)

	// Position 1 (index 0): receiver's MAXL — cap on short packets we send.
	if len(recv) >= 1 {
		rm := int(unchar(recv[0]))
		if rm > 0 && rm <= tocharMax {
			// data = rm - SEQ(1) - TYPE(1) - CHECK(chkLen); chkLen resolved below
			// We re-derive shortMax at the end.
			p.shortMax = rm
		}
	}
	// Position 7 (index 6): QBIN.
	if len(recv) >= 7 {
		switch qb := recv[6]; qb {
		case 'N':
			p.use8bit = false
		case 'Y':
			p.use8bit = true // both said Y → use our '&'
		default:
			if (qb >= 33 && qb <= 62) || (qb >= 96 && qb <= 126) {
				p.qbin = qb
				p.use8bit = true
			}
		}
	}
	// Position 8 (index 7): CHKT.
	if len(recv) >= 8 {
		switch recv[7] {
		case '1':
			p.chkType, p.chkLen = 1, 1
		case '2':
			p.chkType, p.chkLen = 2, 2
		case '3':
			p.chkType, p.chkLen = 3, 3
		}
	}
	// Position 9 (index 8): REPT — RLE is on iff both sides name the same char.
	if len(recv) >= 9 && recv[8] == p.rept && p.rept != ' ' {
		p.useRept = true
		p.reptCh = p.rept
	}
	// Position 10 (index 9): CAPAS — bitfield for optional features.
	supportsLong := false
	if len(recv) >= 10 {
		capas := unchar(recv[9])
		if capas&capLongPackets != 0 {
			supportsLong = true
		}
		if capas&capAttributes != 0 {
			p.hasAttrs = true
		}
		// We already ran negotiation for sliding windows via WINDO field;
		// don't gate on capSlidingWin since many receivers honor WINDO
		// without setting the bit.
	}
	// Position 11 (index 10): WINDO — window size; take min of ours and theirs.
	if len(recv) >= 11 {
		their := int(unchar(recv[10]))
		ours := int(p.windo)
		w := ours
		if their < w {
			w = their
		}
		if w < 1 {
			w = 1
		}
		if w > 31 {
			w = 31
		}
		p.window = w
	}
	// Positions 12-13 (indices 11-12): MAXLX1, MAXLX2 — their long-packet max.
	if supportsLong && len(recv) >= 13 {
		theirLong := int(unchar(recv[11]))*95 + int(unchar(recv[12]))
		ourLong := int(p.maxlx1)*95 + int(p.maxlx2)
		lm := ourLong
		if theirLong < lm {
			lm = theirLong
		}
		if lm > 9024 {
			lm = 9024
		}
		if lm > 0 {
			// DATA length per long packet = lm - CHECK
			p.longMax = lm - p.chkLen
		}
	}

	// Finalize shortMax now that chkLen is known.
	p.shortMax = p.shortMax - 2 - p.chkLen
	if p.shortMax < 1 {
		p.shortMax = 1
	}
}

// ---------- Entry point ----------

// Send transfers `data` as a single file named `name` via Kermit.
func Send(conn deadlineConn, data []byte, name string, cfg Config) error {
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 10 * time.Second
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	s := &sender{
		conn:    conn,
		p:       defaultSendParams(),
		cfg:     cfg,
		timeout: cfg.ReadTimeout,
		inbox:   make(chan event, 64),
		stop:    make(chan struct{}),
	}

	seq := 0

	// S packet handshake — done synchronously before we start the reader
	// goroutine, so chkLen is stable by the time the reader begins.
	// The S packet and its ACK are always type-1 checked (per Kermit spec;
	// the negotiated check only takes effect for subsequent packets).
	ack, err := syncExchange(conn, seq, 'S', buildSendInitData(s.p), 1, cfg.ReadTimeout, 10)
	if err != nil {
		return fmt.Errorf("kermit: send-init: %w", err)
	}
	negotiate(&s.p, ack)
	if cfg.OnStart != nil {
		cfg.OnStart()
	}
	seq = (seq + 1) & 0x3f

	// Now the reader can start with a fixed chkLen for the rest of the session.
	s.startReader()
	defer s.stopReader()

	// F packet.
	fname := sanitizeFilename(name)
	if _, err := s.exchange(seq, 'F', []byte(fname), 10); err != nil {
		return fmt.Errorf("kermit: file-header: %w", err)
	}
	seq = (seq + 1) & 0x3f

	// A (attributes) packet — only if receiver advertised support. Sends
	// size and file-type (text vs binary) so the receiver can show a
	// progress bar and apply (or skip) line-ending conversion correctly.
	if s.p.hasAttrs {
		attr := buildAttributes(len(data), detectTextFile(data))
		if _, err := s.exchange(seq, 'A', attr, 10); err != nil {
			return fmt.Errorf("kermit: attributes: %w", err)
		}
		seq = (seq + 1) & 0x3f
	}

	// D packets — windowed.
	nextSeq, err := s.sendDataWindowed(seq, data)
	if err != nil {
		return fmt.Errorf("kermit: data: %w", err)
	}
	seq = nextSeq

	// Z packet (EOF).
	if _, err := s.exchange(seq, 'Z', nil, 10); err != nil {
		return fmt.Errorf("kermit: EOF: %w", err)
	}
	seq = (seq + 1) & 0x3f

	// B packet (break / end-of-transaction).
	if _, err := s.exchange(seq, 'B', nil, 10); err != nil {
		return fmt.Errorf("kermit: break: %w", err)
	}
	return nil
}

// ---------- Sender state & event loop ----------

type sender struct {
	conn    deadlineConn
	p       params
	cfg     Config
	timeout time.Duration

	inbox chan event
	stop  chan struct{}
	wg    sync.WaitGroup
}

type event struct {
	pkt *packet // set on successful parse
	err error   // set on I/O error
}

// startReader launches a goroutine that parses packets off the wire and
// forwards them to s.inbox. It exits when s.stop is closed or the conn
// errors out.
func (s *sender) startReader() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(s.inbox)
		for {
			select {
			case <-s.stop:
				return
			default:
			}
			// Short deadline so we can poll s.stop.
			_ = s.conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
			p, err := readPacket(s.conn, s.p.chkLen)
			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					continue
				}
				select {
				case s.inbox <- event{err: err}:
				case <-s.stop:
				}
				return
			}
			select {
			case s.inbox <- event{pkt: p}:
			case <-s.stop:
				return
			}
		}
	}()
}

func (s *sender) stopReader() {
	close(s.stop)
	s.wg.Wait()
}

// syncExchange is a standalone send-and-wait used for the S handshake
// (before the reader goroutine exists). Uses its own blocking reads.
func syncExchange(conn deadlineConn, seq int, typ byte, data []byte, chkLen int, timeout time.Duration, maxRetries int) ([]byte, error) {
	pkt := buildShortPacket(seq, typ, data, chkLen)
	retries := 0
	for {
		if _, err := conn.Write(pkt); err != nil {
			return nil, err
		}
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		p, err := readPacket(conn, chkLen)
		if err == nil {
			if p.typ == 'E' {
				return nil, fmt.Errorf("receiver error: %s", string(p.data))
			}
			if p.typ == 'Y' && p.seq == seq {
				return p.data, nil
			}
		}
		retries++
		if retries > maxRetries {
			if err != nil {
				return nil, fmt.Errorf("send-init: %w", err)
			}
			return nil, fmt.Errorf("send-init: no ACK after %d tries", maxRetries)
		}
	}
}

// exchange sends one packet and waits for its matching ACK with retries.
// Used for S, F, Z, B — all non-pipelined handshakes.
func (s *sender) exchange(seq int, typ byte, data []byte, maxRetries int) ([]byte, error) {
	pkt := s.buildPacket(seq, typ, data)
	retries := 0
	for {
		if _, err := s.conn.Write(pkt); err != nil {
			return nil, err
		}
		deadline := time.After(s.timeout)
		for {
			select {
			case ev, ok := <-s.inbox:
				if !ok {
					return nil, errors.New("reader closed")
				}
				if ev.err != nil {
					return nil, ev.err
				}
				p := ev.pkt
				if p.typ == 'E' {
					return nil, fmt.Errorf("receiver error: %s", string(p.data))
				}
				if p.typ == 'Y' && p.seq == seq {
					return p.data, nil
				}
				if p.typ == 'N' && p.seq == seq {
					// NAK → retransmit now.
					goto retransmit
				}
				// Anything else: keep waiting.
			case <-deadline:
				goto retransmit
			}
		}
	retransmit:
		retries++
		if retries > maxRetries {
			reason := fmt.Sprintf("too many retries on seq %d type %c", seq, typ)
			s.abort(seq, reason)
			return nil, errors.New(reason)
		}
	}
}

// sendDataWindowed pipelines D packets up to the negotiated window size.
// Returns the next seq number to use.
func (s *sender) sendDataWindowed(startSeq int, data []byte) (int, error) {
	seq := startSeq
	sent := 0 // bytes consumed into packets (written to wire)
	acked := 0

	var window []winEntry
	const maxRetries = 10
	retransmitAfter := s.timeout
	if retransmitAfter > 5*time.Second {
		retransmitAfter = 5 * time.Second
	}

	for {
		// Fill the window.
		for len(window) < s.p.window && sent < len(data) {
			chunkSize := s.p.shortMax
			useLong := s.p.longMax >= s.p.shortMax*2 && len(data)-sent >= s.p.shortMax*2
			if useLong {
				chunkSize = s.p.longMax
			}
			end := sent + chunkSize
			if end > len(data) {
				end = len(data)
			}
			chunk := data[sent:end]
			encoded := encodeData(chunk, s.p.qctl, s.p.qbin, s.p.reptCh, s.p.use8bit, s.p.useRept)
			pkt := s.buildPacket(seq, 'D', encoded)
			if _, err := s.conn.Write(pkt); err != nil {
				return 0, err
			}
			window = append(window, winEntry{
				seq:     seq,
				bytes:   pkt,
				dataLen: end - sent,
				sentAt:  time.Now(),
			})
			sent = end
			seq = (seq + 1) & 0x3f
		}

		// Done?
		if sent == len(data) && len(window) == 0 {
			return seq, nil
		}

		// Wait for an event or a retransmit timer.
		var tick <-chan time.Time
		if len(window) > 0 {
			due := window[0].sentAt.Add(retransmitAfter)
			remaining := time.Until(due)
			if remaining < 0 {
				remaining = 0
			}
			tick = time.After(remaining)
		}
		select {
		case ev, ok := <-s.inbox:
			if !ok {
				return 0, errors.New("reader closed mid-transfer")
			}
			if ev.err != nil {
				return 0, ev.err
			}
			p := ev.pkt
			if p.typ == 'E' {
				return 0, fmt.Errorf("receiver error: %s", string(p.data))
			}
			idx := findBySeq(window, p.seq)
			if idx < 0 {
				continue // stale / unknown seq
			}
			switch p.typ {
			case 'Y':
				// Ack: slide through the leading run of acked entries.
				window[idx].bytes = nil // mark acked
				for len(window) > 0 && window[0].bytes == nil {
					acked += window[0].dataLen
					window = window[1:]
					if s.cfg.OnStatus != nil {
						s.cfg.OnStatus(StatusEvent{BytesSent: acked, TotalBytes: len(data)})
					}
				}
			case 'N':
				// Explicit NAK: retransmit that entry.
				if err := s.retransmit(&window[idx], maxRetries); err != nil {
					return 0, err
				}
			}
		case <-tick:
			// Retransmit oldest.
			if err := s.retransmit(&window[0], maxRetries); err != nil {
				return 0, err
			}
		}
	}
}

// winEntry tracks one unacked D packet in the sliding window.
type winEntry struct {
	seq     int
	bytes   []byte // serialized packet for retransmit; nil once acked
	dataLen int    // decoded source-byte count (for progress accounting)
	sentAt  time.Time
	retries int
}

func (s *sender) retransmit(e *winEntry, maxRetries int) error {
	if e.bytes == nil {
		return nil // already acked in a race — nothing to do
	}
	e.retries++
	if e.retries > maxRetries {
		reason := fmt.Sprintf("packet seq %d exceeded retry limit", e.seq)
		s.abort(e.seq, reason)
		return errors.New(reason)
	}
	e.sentAt = time.Now()
	_, err := s.conn.Write(e.bytes)
	return err
}

func findBySeq(win []winEntry, seq int) int {
	for i, e := range win {
		if e.seq == seq {
			return i
		}
	}
	return -1
}

// ---------- Packet framing ----------

// buildPacket assembles a Kermit packet in short or extended form,
// selecting the form based on the (encoded) data length plus chkLen.
func (s *sender) buildPacket(seq int, typ byte, data []byte) []byte {
	chkLen := s.p.chkLen
	// For S/F/Z/B packets, chkLen might not be resolved yet. Default to 1.
	if chkLen == 0 {
		chkLen = 1
	}
	total := 2 + len(data) + chkLen // SEQ + TYPE + DATA + CHECK

	if total <= tocharMax {
		return buildShortPacket(seq, typ, data, chkLen)
	}
	return buildLongPacket(seq, typ, data, chkLen)
}

// buildShortPacket: SOH LEN SEQ TYPE DATA... CHECK CR
func buildShortPacket(seq int, typ byte, data []byte, chkLen int) []byte {
	lenCh := tochar(byte(2 + len(data) + chkLen))
	seqCh := tochar(byte(seq & 0x3f))

	pkt := make([]byte, 0, 4+len(data)+chkLen+1)
	pkt = append(pkt, SOH, lenCh, seqCh, typ)
	pkt = append(pkt, data...)
	// Block check covers LEN+SEQ+TYPE+DATA.
	chk := blockCheck(chkLen, pkt[1:])
	pkt = append(pkt, chk...)
	pkt = append(pkt, CR)
	return pkt
}

// buildLongPacket: SOH LEN(=SP) SEQ TYPE LENX1 LENX2 HCHECK DATA... CHECK CR
// LENX1*95+LENX2 = len(DATA) + chkLen.
func buildLongPacket(seq int, typ byte, data []byte, chkLen int) []byte {
	lenCh := tochar(0) // signals extended
	seqCh := tochar(byte(seq & 0x3f))
	x := len(data) + chkLen
	lenx1 := tochar(byte(x / 95))
	lenx2 := tochar(byte(x % 95))
	// HCHECK = type-1 check over LEN+SEQ+TYPE+LENX1+LENX2.
	hsum := int(lenCh) + int(seqCh) + int(typ) + int(lenx1) + int(lenx2)
	hcheck := tochar(byte((hsum + ((hsum >> 6) & 0x03)) & 0x3f))

	pkt := make([]byte, 0, 7+len(data)+chkLen+1)
	pkt = append(pkt, SOH, lenCh, seqCh, typ, lenx1, lenx2, hcheck)
	pkt = append(pkt, data...)
	// Block check covers LEN+SEQ+TYPE+LENX1+LENX2+HCHECK+DATA (everything after SOH, before CHECK).
	chk := blockCheck(chkLen, pkt[1:])
	pkt = append(pkt, chk...)
	pkt = append(pkt, CR)
	return pkt
}

// ---------- Block checks ----------

// blockCheck returns the block check bytes for the given region.
func blockCheck(chkLen int, region []byte) []byte {
	switch chkLen {
	case 1:
		return []byte{chk1(region)}
	case 2:
		return chk2(region)
	case 3:
		return chk3(region)
	}
	return []byte{chk1(region)}
}

// chk1: single-byte arithmetic checksum, folded into 6 bits, tochar'd.
func chk1(region []byte) byte {
	var sum int
	for _, b := range region {
		sum += int(b)
	}
	return tochar(byte((sum + ((sum >> 6) & 0x03)) & 0x3f))
}

// chk2: two-byte 12-bit sum, split into two 6-bit tochar'd bytes.
func chk2(region []byte) []byte {
	var sum int
	for _, b := range region {
		sum += int(b)
	}
	sum &= 0xfff
	return []byte{tochar(byte((sum >> 6) & 0x3f)), tochar(byte(sum & 0x3f))}
}

// chk3: three-byte CRC-16-Kermit (reflected CCITT, poly 0x8408), split 4/6/6.
func chk3(region []byte) []byte {
	crc := crc16Kermit(region)
	return []byte{
		tochar(byte((crc >> 12) & 0x0f)),
		tochar(byte((crc >> 6) & 0x3f)),
		tochar(byte(crc & 0x3f)),
	}
}

// crc16Kermit computes Kermit's CRC-16 (reflected CCITT): poly 0x8408,
// init 0, no final XOR, bytes processed LSB-first.
func crc16Kermit(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0x8408
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// ---------- Packet parsing ----------

type packet struct {
	seq  int
	typ  byte
	data []byte // decoded data field (still quoted as per wire)
}

// readPacket reads one Kermit packet with the given block-check length.
// Skips noise bytes before the SOH mark. Tolerates stuck parity by masking
// 0x80 on all header bytes.
func readPacket(conn deadlineConn, chkLen int) (*packet, error) {
	if chkLen == 0 {
		chkLen = 1
	}
	buf := [1]byte{}

	// Scan for SOH.
	for {
		if _, err := io.ReadFull(conn, buf[:]); err != nil {
			return nil, err
		}
		if buf[0]&0x7f == SOH {
			break
		}
	}
	// LEN
	if _, err := io.ReadFull(conn, buf[:]); err != nil {
		return nil, err
	}
	lenCh := buf[0] & 0x7f

	// Read SEQ + TYPE.
	header2 := [2]byte{}
	if _, err := io.ReadFull(conn, header2[:]); err != nil {
		return nil, err
	}
	header2[0] &= 0x7f
	header2[1] &= 0x7f
	seqCh := header2[0]
	typ := header2[1]

	var dataLen int
	var extHeader []byte // LENX1 LENX2 HCHECK (for long packets)

	if lenCh == tochar(0) {
		// Extended packet.
		ext := [3]byte{}
		if _, err := io.ReadFull(conn, ext[:]); err != nil {
			return nil, err
		}
		for i := range ext {
			ext[i] &= 0x7f
		}
		lenx1, lenx2, hcheck := ext[0], ext[1], ext[2]
		// Verify HCHECK.
		hsum := int(lenCh) + int(seqCh) + int(typ) + int(lenx1) + int(lenx2)
		wantH := tochar(byte((hsum + ((hsum >> 6) & 0x03)) & 0x3f))
		if wantH != hcheck {
			return nil, fmt.Errorf("hcheck mismatch: got %q want %q", hcheck, wantH)
		}
		x := int(unchar(lenx1))*95 + int(unchar(lenx2))
		if x < chkLen || x > 9024 {
			return nil, fmt.Errorf("bad extended length %d", x)
		}
		dataLen = x - chkLen
		extHeader = []byte{lenx1, lenx2, hcheck}
	} else {
		total := int(unchar(lenCh)) // SEQ+TYPE+DATA+CHECK
		if total < 2+chkLen || total > tocharMax {
			return nil, fmt.Errorf("bad LEN %d", total)
		}
		dataLen = total - 2 - chkLen
	}

	data := make([]byte, dataLen)
	if dataLen > 0 {
		if _, err := io.ReadFull(conn, data); err != nil {
			return nil, err
		}
		for i := range data {
			data[i] &= 0x7f
		}
	}
	check := make([]byte, chkLen)
	if _, err := io.ReadFull(conn, check); err != nil {
		return nil, err
	}
	for i := range check {
		check[i] &= 0x7f
	}

	// Verify check over LEN+SEQ+TYPE+(extHeader)+DATA.
	region := make([]byte, 0, 3+len(extHeader)+len(data))
	region = append(region, lenCh, seqCh, typ)
	region = append(region, extHeader...)
	region = append(region, data...)
	want := blockCheck(chkLen, region)
	for i := range want {
		if want[i] != check[i] {
			return nil, fmt.Errorf("check mismatch: got %x want %x", check, want)
		}
	}

	// Drain trailing EOL if present (best effort, ignore timeout).
	_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_, _ = conn.Read(buf[:])

	return &packet{
		seq:  int(unchar(seqCh)),
		typ:  typ,
		data: data,
	}, nil
}

// ---------- Encoding / decoding of data fields ----------

// encodeData applies 8-bit quoting, control quoting, and (optionally) RLE.
// The output contains only printable ASCII (the caller appends the CHECK
// + EOL outside this function).
func encodeData(in []byte, qctl, qbin, reptCh byte, use8bit, useRept bool) []byte {
	out := make([]byte, 0, len(in)+len(in)/8)
	for i := 0; i < len(in); {
		b := in[i]
		runLen := 1
		for runLen < 94 && i+runLen < len(in) && in[i+runLen] == b {
			runLen++
		}
		if useRept && runLen >= 4 {
			out = append(out, reptCh, tochar(byte(runLen)))
			out = appendEncoded(out, b, qctl, qbin, reptCh, use8bit, useRept)
			i += runLen
		} else {
			out = appendEncoded(out, b, qctl, qbin, reptCh, use8bit, useRept)
			i++
		}
	}
	return out
}

// appendEncoded emits one byte in its quoted form (1–4 chars).
func appendEncoded(out []byte, b, qctl, qbin, reptCh byte, use8bit, useRept bool) []byte {
	c := b
	highBit := false
	if c&0x80 != 0 {
		if use8bit {
			highBit = true
			c &= 0x7f
		} else {
			// 7-bit link: strip the high bit so the checksum we compute
			// matches what a parity-stripping receiver reconstructs.
			// Binary data is lossy in this mode — a limitation of the
			// negotiated 7-bit link, not a bug.
			c &= 0x7f
		}
	}
	isCtrl := c < 0x20 || c == 0x7f
	needsQctl := isCtrl || c == qctl
	if use8bit && c == qbin {
		needsQctl = true
	}
	if useRept && c == reptCh {
		needsQctl = true
	}
	if highBit {
		out = append(out, qbin)
	}
	if needsQctl {
		out = append(out, qctl)
		if isCtrl {
			out = append(out, ctl(c))
		} else {
			out = append(out, c)
		}
	} else {
		out = append(out, c)
	}
	return out
}

// ---------- Packet helpers ----------

// buildSendInitData lays out the 13 proposal bytes in the S packet.
func buildSendInitData(p params) []byte {
	return []byte{
		tochar(p.maxl),
		tochar(p.ptime),
		tochar(p.npad),
		ctl(p.padc),
		tochar(p.eol),
		p.qctl,
		p.qbin,
		p.chkt,
		p.rept,
		tochar(p.capas),
		tochar(p.windo),
		tochar(p.maxlx1),
		tochar(p.maxlx2),
	}
}

// buildAttributes lays out the data field for an A packet.
//
// Each attribute is "<tag><tochar(length)><value>". We emit:
//
//	'!' length (decimal ASCII)          — file size in bytes
//	'"' length (single char: 'A' or 'B') — file type, ASCII vs Binary
//
// That's enough for C-Kermit and G-Kermit to show a progress bar and
// pick the right line-ending policy. Additional attributes (date,
// creator, charset, etc.) are optional per spec and we don't send them.
func buildAttributes(size int, isText bool) []byte {
	sizeStr := fmt.Sprintf("%d", size)
	if len(sizeStr) > tocharMax {
		sizeStr = sizeStr[:tocharMax]
	}
	ft := byte('B')
	if isText {
		ft = 'A'
	}
	out := make([]byte, 0, 2+len(sizeStr)+3)
	out = append(out, '!', tochar(byte(len(sizeStr))))
	out = append(out, sizeStr...)
	out = append(out, '"', tochar(1), ft)
	return out
}

// detectTextFile classifies file content as text (true) or binary (false)
// by scanning the first 8 KB: any high-bit byte or control byte outside
// TAB/LF/FF/CR tips us to binary. Empty files are reported as text.
func detectTextFile(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		b := data[i]
		if b >= 0x80 {
			return false
		}
		if b < 0x20 && b != 0x09 && b != 0x0a && b != 0x0c && b != 0x0d {
			return false
		}
	}
	return true
}

// abort sends a best-effort E packet so the receiver can display a
// human-readable reason rather than timing out. Never blocks and
// ignores write errors — by the time we abort, we're already on the
// way to returning an error to the caller.
func (s *sender) abort(seq int, reason string) {
	chkLen := s.p.chkLen
	if chkLen == 0 {
		chkLen = 1
	}
	// Keep the message printable and short enough for a short packet.
	maxData := tocharMax - 2 - chkLen
	r := make([]byte, 0, len(reason))
	for i := 0; i < len(reason) && len(r) < maxData; i++ {
		b := reason[i]
		if b < 0x20 || b >= 0x7f {
			b = '?'
		}
		r = append(r, b)
	}
	pkt := buildShortPacket(seq&0x3f, 'E', r, chkLen)
	_, _ = s.conn.Write(pkt)
}

// sanitizeFilename produces a Kermit-friendly filename: basename only,
// uppercased, constrained to alphanumerics + . - _.
func sanitizeFilename(name string) string {
	base := filepath.Base(name)
	up := strings.ToUpper(base)
	var b strings.Builder
	for _, r := range up {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = "FILE"
	}
	return out
}

// ---------- tochar / unchar / ctl helpers ----------

func tochar(n byte) byte { return n + 0x20 }
func unchar(b byte) byte { return b - 0x20 }
func ctl(b byte) byte    { return b ^ 0x40 }

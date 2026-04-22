package zmodem

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/solvalou/xfer/internal/logger"
)

const (
	// xprzmodem.library (Term 4.8, NComm) hard-codes a 1024-byte zrdata
	// buffer. Larger subpackets cause it to abort with "Data packet too long".
	subpacketSize = 1024

	// Eight 1024-byte subpackets per ACK = 8 KB between real round-trips.
	// Short enough that slow serial clients with small FIFO buffers don't
	// overflow while waiting for the wire to catch up.
	subpacketsPerAck = 8

	activityTimeout    = 60 * time.Second
	canCancelThreshold = 5
)

// cancelEcho is what the sender writes on the wire when the receiver has
// cancelled the session: 8× CAN followed by 10× backspace. Matches lrzsz.
var cancelEcho = []byte{
	0x18, 0x18, 0x18, 0x18, 0x18, 0x18, 0x18, 0x18,
	0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08,
}

// ErrCancelled is returned when the receiver aborts via ≥5 consecutive CANs.
var ErrCancelled = errors.New("ZMODEM transfer cancelled by receiver")

// rzTrigger is the preamble terminals like NComm / Term 4.8 require before
// recognizing a ZMODEM session. Modern receivers (lrzsz) tolerate it harmlessly.
var rzTrigger = []byte("rz\r")

// nowFn is swapped by tests to pin timestamps for deterministic ZFILE mtime.
var nowFn = func() time.Time { return time.Now() }

// SetNow replaces the clock used to stamp ZFILE mtime. Returns a restore func.
// Tests-only hook; no production caller should touch this.
func SetNow(f func() time.Time) func() {
	prev := nowFn
	nowFn = f
	return func() { nowFn = prev }
}

// SendBuffer transfers `data` to the ZMODEM receiver on the other end of conn,
// advertising `filename` in the ZFILE frame. Honors the receiver's ZRPOS
// resume offset and emits subpackets paced at 8 KB per ACK.
//
// The function takes full ownership of conn for the duration of the transfer:
// reads and writes must be routed through the returned error only. The caller
// is responsible for restoring any pre-attached read handlers afterwards.
func SendBuffer(conn net.Conn, data []byte, filename string) error {
	DisableEscctl()
	defer DisableEscctl()

	s := newSender(conn, data, filename)
	err := s.run()
	if err == nil || errors.Is(err, ErrCancelled) {
		return err
	}
	// Strip Go's "io:" prefix on closed-pipe errors; surface cleanly to the
	// user-visible log line.
	logger.Error(fmt.Sprintf("ZMODEM transfer failed: %s", strings.TrimPrefix(err.Error(), "io: ")))
	return err
}

// --- Internals --------------------------------------------------------------

type sender struct {
	conn     net.Conn
	data     []byte
	filename string

	reader *reader

	// escctlApplied flips to true after we've committed to an ESCCTL decision
	// based on the first ZRINIT we sniff from the receiver.
	escctlApplied bool

	// Rolling windows for incremental frame detection.
	sniff    []byte // 24-byte ESCCTL negotiation window
	hexSniff []byte // 32-byte hex-header match window

	startedAt time.Time
}

func newSender(conn net.Conn, data []byte, filename string) *sender {
	s := &sender{conn: conn, data: data, filename: filename, startedAt: nowFn()}
	s.reader = newReader(conn)
	return s
}

func (s *sender) run() error {
	defer s.reader.close()

	logger.Info(fmt.Sprintf("Starting ZMODEM transfer: %s (%d bytes)", s.filename, len(s.data)))

	// Preamble + ZRQINIT.
	if _, err := s.conn.Write(rzTrigger); err != nil {
		return err
	}
	if _, err := s.conn.Write(BuildZhexHeader(FrameZRQINIT, 0)); err != nil {
		return err
	}

	// Negotiate: wait for ZRINIT from the receiver.
	if err := s.waitForZrinit(); err != nil {
		return err
	}

	// Send ZFILE (CRC16, lrzsz fileinfo).
	if _, err := s.conn.Write(s.buildZfile()); err != nil {
		return err
	}

	// Receiver responds with ZRPOS(offset). Read it.
	offset, err := s.waitForZrpos()
	if err != nil {
		return err
	}

	// Stream ZDATA bursts from `offset` to end-of-file, pacing ACKs.
	if err := s.streamData(offset); err != nil {
		return err
	}

	// ZEOF + wait for ZRINIT confirmation.
	if _, err := s.conn.Write(BuildZbinHeader(FrameZEOF, uint32(len(s.data)))); err != nil {
		return err
	}
	if err := s.waitForZrinit(); err != nil {
		return err
	}

	// ZFIN handshake: send ZFIN, wait for receiver's ZFIN, write the "OO"
	// (Over and Out) terminator that closes the session.
	if _, err := s.conn.Write(BuildZhexHeader(FrameZFIN, 0)); err != nil {
		return err
	}
	if err := s.waitForZfin(); err != nil {
		return err
	}
	if _, err := s.conn.Write([]byte{'O', 'O'}); err != nil {
		return err
	}

	logger.Info(fmt.Sprintf("ZMODEM transfer completed in %.1fs", time.Since(s.startedAt).Seconds()))
	return nil
}

// maybeNegotiateEscctl accumulates a rolling sniff buffer and, once the
// receiver's first ZRINIT is fully in the window, toggles the ZDLE table.
func (s *sender) maybeNegotiateEscctl(chunk []byte) {
	if s.escctlApplied {
		return
	}
	s.sniff = append(s.sniff, chunk...)
	if len(s.sniff) > 24 {
		s.sniff = s.sniff[len(s.sniff)-24:]
	}
	requested, ok := DetectEscctlFromZrinit(s.sniff)
	if !ok {
		return
	}
	if requested {
		EnableEscctl()
	}
	s.escctlApplied = true
}

// waitForZrinit reads bytes until a ZRINIT header appears, piping all
// consumed bytes through the ESCCTL sniff logic along the way.
func (s *sender) waitForZrinit() error {
	return s.awaitHexFrame(FrameZRINIT, true)
}

// waitForZfin reads until we see a ZHEX ZFIN frame from the receiver.
func (s *sender) waitForZfin() error {
	return s.awaitHexFrame(FrameZFIN, false)
}

// awaitHexFrame drives the reader until a ZHEX header of the given type
// arrives. When `sniffEscctl` is true, each byte is also fed through the
// ESCCTL negotiation window — only the first waitForZrinit does this since
// once we've committed to an ESCCTL decision for the session there's no
// value re-evaluating later frames.
func (s *sender) awaitHexFrame(want byte, sniffEscctl bool) error {
	for {
		b, err := s.reader.readByte(activityTimeout)
		if err != nil {
			return err
		}
		if sniffEscctl {
			s.maybeNegotiateEscctl([]byte{b})
		}
		if frame, ok := s.matchNextHexFrame(b); ok && frame == want {
			return nil
		}
	}
}

// waitForZrpos waits for a ZRPOS header (can be ZHEX or ZBIN), parses the
// 4-byte offset, and returns it.
func (s *sender) waitForZrpos() (uint32, error) {
	return s.awaitFrameWithCount(FrameZRPOS)
}

// matchNextHexFrame appends `cur` to the rolling hex-sniff window and
// reports any complete ZHEX header it can now decode out of the tail.
func (s *sender) matchNextHexFrame(cur byte) (frame byte, ok bool) {
	s.hexSniff = append(s.hexSniff, cur)
	if len(s.hexSniff) > 32 {
		s.hexSniff = s.hexSniff[len(s.hexSniff)-32:]
	}
	buf := s.hexSniff
	for i := 0; i+18 <= len(buf); i++ {
		if buf[i] != ZPAD || buf[i+1] != ZPAD || buf[i+2] != ZDLE || buf[i+3] != ZHEX {
			continue
		}
		if f, _, decOk := DecodeHexHeader(buf, i+4); decOk {
			return f, true
		}
	}
	return 0, false
}

// buildZfile emits the ZBIN16 ZFILE header + lrzsz-format fileinfo subpacket.
// CRC16 (not CRC32) is load-bearing: Term 4.8 advertises CANFC32 but hangs
// when it actually receives a CRC32 ZFILE. See amigalove.com writeup.
func (s *sender) buildZfile() []byte {
	// Header: ZBIN ZFILE with ZF3..ZF0 = 00 00 00 01 (ZCBIN binary).
	out := []byte{ZPAD, ZDLE, ZBIN}
	header := []byte{FrameZFILE, 0x00, 0x00, 0x00, 0x01}
	out = AppendEscaped(out, header)
	crc := CRC16(header)
	out = AppendEscaped(out, []byte{byte(crc >> 8), byte(crc)})

	// Subpacket: "<name>\0<size> <mtime_octal> 100644 0 1 <size>\0"
	info := make([]byte, 0, 128+len(s.filename))
	info = append(info, []byte(s.filename)...)
	info = append(info, 0)
	meta := fmt.Sprintf("%d %s 100644 0 1 %d",
		len(s.data),
		strconv.FormatInt(nowFn().Unix(), 8),
		len(s.data))
	info = append(info, []byte(meta)...)
	info = append(info, 0)
	out = AppendEscaped(out, info)

	// ZDLE + ZCRCW + CRC16 of (info + ZCRCW).
	out = append(out, ZDLE, ZCRCW)
	crcInput := make([]byte, len(info)+1)
	copy(crcInput, info)
	crcInput[len(info)] = ZCRCW
	subCrc := CRC16(crcInput)
	out = AppendEscaped(out, []byte{byte(subCrc >> 8), byte(subCrc)})
	return out
}

// streamData emits ZDATA bursts from `startOffset` to end-of-file, flushing
// every `subpacketsPerAck` subpackets with a ZCRCW that waits for a ZACK.
func (s *sender) streamData(startOffset uint32) error {
	offset := startOffset
	for int(offset) < len(s.data) {
		burstStart := offset
		// Assemble one ZDATA burst (up to subpacketsPerAck subpackets or EOF).
		frame := BuildZbinHeader(FrameZDATA, burstStart)

		for i := 0; i < subpacketsPerAck; i++ {
			remaining := len(s.data) - int(offset)
			if remaining <= 0 {
				break
			}
			chunk := s.data[offset:]
			if len(chunk) > subpacketSize {
				chunk = chunk[:subpacketSize]
			}
			kind := byte(ZCRCG)
			last := int(offset)+len(chunk) == len(s.data) || i == subpacketsPerAck-1
			if last {
				kind = ZCRCW
			}
			frame = AppendEscaped(frame, chunk)
			frame = append(frame, ZDLE, kind)
			crcInput := make([]byte, len(chunk)+1)
			copy(crcInput, chunk)
			crcInput[len(chunk)] = kind
			crc := CRC16(crcInput)
			frame = AppendEscaped(frame, []byte{byte(crc >> 8), byte(crc)})
			offset += uint32(len(chunk))
			if last {
				break
			}
		}
		if _, err := s.conn.Write(frame); err != nil {
			return err
		}
		// A ZCRCW terminator always requests an ACK — wait for it before
		// either starting the next burst or emitting ZEOF. Skipping the
		// wait on EOF works against modern receivers but confuses some
		// retro ones (they NAK the ZEOF because they haven't ACKed the
		// last subpacket yet).
		if err := s.waitForZack(); err != nil {
			return err
		}
	}
	return nil
}

// waitForZack reads bytes until a ZACK header (ZHEX or ZBIN) is parsed.
func (s *sender) waitForZack() error {
	_, err := s.awaitFrameWithCount(FrameZACK)
	return err
}

// awaitFrameWithCount scans incoming bytes for a frame of the given type and
// returns its 32-bit count/offset payload. Accepts both ZHEX and ZBIN
// encodings, which matters because most retro receivers send ZHEX while
// lrzsz-in-CANOVIO-mode can send ZBIN.
func (s *sender) awaitFrameWithCount(want byte) (uint32, error) {
	const windowSize = 64
	var rolling []byte
	for {
		b, err := s.reader.readByte(activityTimeout)
		if err != nil {
			return 0, err
		}
		s.maybeNegotiateEscctl([]byte{b})
		rolling = append(rolling, b)
		if len(rolling) > windowSize {
			rolling = rolling[len(rolling)-windowSize:]
		}
		if count, ok := scanZhexFrame(rolling, want); ok {
			return count, nil
		}
		if count, ok := scanZbinFrame(rolling, want); ok {
			return count, nil
		}
	}
}

// scanZhexFrame looks for a ZHEX header of type `want` in `buf` and returns
// its 4-byte count if found.
func scanZhexFrame(buf []byte, want byte) (uint32, bool) {
	for i := 0; i+18 <= len(buf); i++ {
		if buf[i] != ZPAD || buf[i+1] != ZPAD || buf[i+2] != ZDLE || buf[i+3] != ZHEX {
			continue
		}
		frame, flags, ok := DecodeHexHeader(buf, i+4)
		if !ok || frame != want {
			continue
		}
		return uint32(flags[0]) | uint32(flags[1])<<8 | uint32(flags[2])<<16 | uint32(flags[3])<<24, true
	}
	return 0, false
}

// scanZbinFrame looks for a ZBIN (CRC16) header of type `want` in `buf` and
// returns its 4-byte count if found.
func scanZbinFrame(buf []byte, want byte) (uint32, bool) {
	for i := 0; i+3 < len(buf); i++ {
		if buf[i] != ZPAD || buf[i+1] != ZDLE || buf[i+2] != ZBIN {
			continue
		}
		payload, ok := parseZbinPayload(buf[i+3:])
		if !ok || payload[0] != want {
			continue
		}
		return uint32(payload[1]) | uint32(payload[2])<<8 | uint32(payload[3])<<16 | uint32(payload[4])<<24, true
	}
	return 0, false
}

// parseZbinPayload unescapes up to 7 bytes (5 payload + 2 CRC) from a ZBIN
// frame body that starts immediately after the ZBIN marker. Returns the 5
// payload bytes on success (CRC is checked).
func parseZbinPayload(buf []byte) ([]byte, bool) {
	payload := make([]byte, 0, 5)
	crcIn := make([]byte, 0, 2)
	i := 0
	for len(payload) < 5 && i < len(buf) {
		b := buf[i]
		if b == ZDLE {
			if i+1 >= len(buf) {
				return nil, false
			}
			payload = append(payload, buf[i+1]^0x40)
			i += 2
		} else {
			payload = append(payload, b)
			i++
		}
	}
	if len(payload) < 5 {
		return nil, false
	}
	for len(crcIn) < 2 && i < len(buf) {
		b := buf[i]
		if b == ZDLE {
			if i+1 >= len(buf) {
				return nil, false
			}
			crcIn = append(crcIn, buf[i+1]^0x40)
			i += 2
		} else {
			crcIn = append(crcIn, b)
			i++
		}
	}
	if len(crcIn) < 2 {
		return nil, false
	}
	expected := uint16(crcIn[0])<<8 | uint16(crcIn[1])
	if CRC16(payload) != expected {
		return nil, false
	}
	return payload, true
}

// --- reader ----------------------------------------------------------------

// reader wraps a net.Conn with:
//   - byte-by-byte reads with per-call deadlines (activity timeout)
//   - async CAN-run detection: 5 consecutive CAN bytes cancel the session
//   - listener bookkeeping so the caller can restore the conn cleanly
type reader struct {
	conn      net.Conn
	mu        sync.Mutex
	buf       []byte
	cond      *sync.Cond
	done      chan struct{}
	closeOnce sync.Once
	cancelled atomic.Bool
	err       error
}

func newReader(conn net.Conn) *reader {
	r := &reader{conn: conn, done: make(chan struct{})}
	r.cond = sync.NewCond(&r.mu)
	go r.loop()
	return r
}

// readBufLimit caps the pending-byte buffer so a misbehaving or stalled peer
// can't grow our memory use unbounded. 4 KB is far more than any valid ZMODEM
// handshake — headers are < 30 bytes each.
const readBufLimit = 4096

func (r *reader) loop() {
	defer close(r.done)
	buf := make([]byte, 1024)
	consecutiveCan := 0
	for {
		n, err := r.conn.Read(buf)
		if n > 0 {
			for _, b := range buf[:n] {
				if b == CAN {
					consecutiveCan++
					if consecutiveCan >= canCancelThreshold {
						r.cancelled.Store(true)
					}
				} else {
					consecutiveCan = 0
				}
			}
			r.mu.Lock()
			r.buf = append(r.buf, buf[:n]...)
			if len(r.buf) > readBufLimit {
				r.buf = r.buf[len(r.buf)-readBufLimit:]
			}
			r.cond.Broadcast()
			r.mu.Unlock()
		}
		if err != nil {
			r.mu.Lock()
			if r.err == nil {
				r.err = err
			}
			r.cond.Broadcast()
			r.mu.Unlock()
			return
		}
	}
}

// CAN control byte (duplicate of xmodem.CAN; kept local to avoid import cycle).
const CAN = 0x18

// readByte blocks until a byte is available, the deadline elapses, the
// receiver cancels, or the socket fails. Returns ErrCancelled on cancel.
func (r *reader) readByte(timeout time.Duration) (byte, error) {
	deadline := time.Now().Add(timeout)
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.buf) == 0 {
		if r.cancelled.Load() {
			go r.emitCancelEcho()
			return 0, ErrCancelled
		}
		if r.err != nil {
			return 0, r.err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, fmt.Errorf("ZMODEM activity timeout - no progress")
		}
		// Wait with a guard goroutine that broadcasts on timeout expiry.
		timer := time.AfterFunc(remaining, func() {
			r.mu.Lock()
			r.cond.Broadcast()
			r.mu.Unlock()
		})
		r.cond.Wait()
		timer.Stop()
	}
	// If more than one CAN arrived while we were serving another byte, still
	// surface the cancellation first.
	if r.cancelled.Load() {
		go r.emitCancelEcho()
		return 0, ErrCancelled
	}
	b := r.buf[0]
	r.buf = r.buf[1:]
	return b, nil
}

func (r *reader) emitCancelEcho() {
	_, _ = r.conn.Write(cancelEcho)
	logger.Info("ZMODEM transfer cancelled by receiver")
}

func (r *reader) close() {
	r.closeOnce.Do(func() {
		// Nudge the read loop out of its blocking Read.
		_ = r.conn.SetReadDeadline(time.Now())
		// Restore deadline after the loop exits — nothing else will read conn.
		<-r.done
		_ = r.conn.SetReadDeadline(time.Time{})
	})
}

package zmodem

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/arttu76/xfer/internal/logger"
)

// receiveTimeout caps how long we wait between bytes from the sender. Matches
// the sender's activityTimeout so a stalled link is reported the same way in
// both directions.
const receiveTimeout = activityTimeout

// ReceiveResult bundles the bytes + filename + (optional) advertised size
// pulled out of the sender's ZFILE info subpacket.
type ReceiveResult struct {
	Data     []byte
	Filename string
	Size     int // 0 if the sender didn't include a size in the info field
}

// ReceiveConfig lets callers subscribe to status events.
type ReceiveConfig struct {
	OnStart  func()
	OnStatus func(ReceiveStatus)
}

// ReceiveStatus is emitted after each accepted subpacket.
type ReceiveStatus struct {
	Bytes int
	Total int // 0 if size unknown
}

// Receive runs the ZMODEM receiver on conn until the sender finishes (ZEOF +
// ZFIN handshake) or aborts. The function takes ownership of conn for the
// duration: reads and writes go through it until Receive returns. The caller
// should restore any pre-attached read handlers afterwards.
//
// CRC handling: we don't set CanFC32 in our ZRINIT, but several vintage
// senders (xprzmodem.library on Amiga, etc.) latch into ZBIN32/CRC32 mode
// anyway as soon as we advertise a non-zero bufsize. The receiver therefore
// decodes both ZBIN (CRC16) and ZBIN32 (CRC32) headers and reciprocates with
// matching ZACK/ZRPOS frames. ZHEX headers stay CRC16 in both directions.
func Receive(conn net.Conn, cfg ReceiveConfig) (ReceiveResult, error) {
	DisableEscctl()
	defer DisableEscctl()

	r := &receiver{conn: conn, reader: newReader(conn), cfg: cfg}
	defer r.reader.close()

	res, err := r.run()
	switch {
	case err == nil:
		return res, nil
	case errors.Is(err, ErrCancelled):
		return ReceiveResult{}, err
	default:
		logger.Error(fmt.Sprintf("ZMODEM receive failed: %s", strings.TrimPrefix(err.Error(), "io: ")))
		return ReceiveResult{}, err
	}
}

type receiver struct {
	conn   net.Conn
	reader *reader
	cfg    ReceiveConfig

	out      []byte
	filename string
	totalSize int

	// crc32Mode latches once the sender emits its first ZBIN32 header.
	// From then on every subsequent subpacket carries a 4-byte CRC32
	// (little-endian) instead of a 2-byte CRC16, and our outgoing
	// ZACK/ZRPOS frames switch to ZBIN32 too — many vintage senders
	// only accept reciprocated CRC types in mid-transfer responses.
	crc32Mode bool

	startedAt time.Time
}

// buildHeader emits an outbound binary header in whichever CRC flavor the
// sender is currently using. ZHEX is unchanged (it's CRC16 always) and
// stays at the BuildZhexHeader call sites.
func (r *receiver) buildHeader(frame byte, count uint32) []byte {
	if r.crc32Mode {
		return BuildZbin32Header(frame, count)
	}
	return BuildZbinHeader(frame, count)
}

// ---------- Top-level state machine ----------

func (r *receiver) run() (ReceiveResult, error) {
	r.startedAt = nowFn()

	// Send ZRINIT to nudge the sender into emitting ZFILE. CanFDX |
	// CanOVIO advertise full duplex + overlapped IO; CanFC32 stays
	// CLEARED so the sender uses CRC16 (which is all we decode).
	if err := r.sendZRINIT(); err != nil {
		return ReceiveResult{}, err
	}

	// Wait for ZFILE. ZRQINIT means the sender wants us to advertise our
	// capabilities — re-send ZRINIT in that case.
	if err := r.waitForZFile(); err != nil {
		return ReceiveResult{}, err
	}

	// ZFILE header has been consumed; the info subpacket follows.
	if err := r.readZFileSubpacket(); err != nil {
		return ReceiveResult{}, err
	}
	if r.cfg.OnStart != nil {
		r.cfg.OnStart()
	}

	// Always start fresh — no resume support.
	if _, err := r.conn.Write(r.buildHeader(FrameZRPOS, 0)); err != nil {
		return ReceiveResult{}, err
	}

	// Receive ZDATA bursts until ZEOF.
	if err := r.receiveData(); err != nil {
		return ReceiveResult{}, err
	}

	// ZEOF acknowledged with another ZRINIT.
	if err := r.sendZRINIT(); err != nil {
		return ReceiveResult{}, err
	}

	// Wait for ZFIN, reply ZFIN, read "OO" (best effort — the sender may
	// already be closing the connection by the time we look for it).
	if err := r.awaitFrame(FrameZFIN); err != nil {
		return ReceiveResult{}, err
	}
	if _, err := r.conn.Write(BuildZhexHeader(FrameZFIN, 0)); err != nil {
		return ReceiveResult{}, err
	}
	r.readOOBestEffort()

	logger.Info(fmt.Sprintf("ZMODEM receive completed in %.1fs (%s, %d bytes)",
		time.Since(r.startedAt).Seconds(), r.filename, len(r.out)))

	return ReceiveResult{
		Data:     r.out,
		Filename: r.filename,
		Size:     r.totalSize,
	}, nil
}

// zrinitBufsize is the receive-window size advertised in ZRINIT (ZP2/ZP3).
// Senders stream subpackets with ZCRCG (no ACK) until they've sent this many
// bytes, then switch to ZCRCW (ACK required). Setting this matters because
// some vintage receivers treat 0 as "I can buffer almost nothing — ACK every
// subpacket", which collapses real-world throughput to a handful of bytes
// per second on a high-latency link (xprzmodem.library on Amiga is the
// motivating case). 8192 matches our own sender's subpacketsPerAck*subpacketSize
// burst size and keeps modern senders (lrzsz) streaming.
const zrinitBufsize = 8192

func (r *receiver) sendZRINIT() error {
	// ZHEX form: lrzsz/Term 4.8 senders look for ZRINIT in hex headers
	// only (their wait-for-ZRINIT scanner doesn't match ZBIN). Any other
	// header from the receive side can be ZBIN since the sender's reply
	// scanners accept either, but ZRINIT is special.
	//
	// Layout: ZP0=ZF0 capability flags, ZP1=ZF1 (unused), ZP2=bufsize lo,
	// ZP3=bufsize hi.
	//
	// CanFC32 is set because we actually decode CRC32 headers and
	// subpackets — being explicit beats letting old senders infer it
	// from a byte they happened to misparse out of our bufsize bytes.
	flags := uint32(CanFDX|CanOVIO|CanFC32) | (uint32(zrinitBufsize) << 16)
	header := BuildZhexHeader(FrameZRINIT, flags)
	_, err := r.conn.Write(header)
	return err
}

// waitForZFile loops reading headers until a ZFILE arrives. ZRQINIT bounces
// us back to advertising. Anything else is ignored.
func (r *receiver) waitForZFile() error {
	for {
		frame, _, err := r.readHeader()
		if err != nil {
			return err
		}
		switch frame {
		case FrameZFILE:
			return nil
		case FrameZRQINIT:
			if err := r.sendZRINIT(); err != nil {
				return err
			}
		case FrameZFIN, FrameZABORT, FrameZFERR:
			return fmt.Errorf("ZMODEM: sender aborted before ZFILE (frame %d)", frame)
		default:
			// Spurious headers (e.g. an extra ZRINIT echoed back, or a
			// duplicate ZSINIT) — ignore and keep waiting.
		}
	}
}

// readZFileSubpacket reads the info subpacket that follows a ZFILE header.
// Format: "<name>\0<size> <mtime_octal> <mode> <serial> <files> <bytesleft>\0"
// terminated by a ZDLE+ZCRCW (or rarely ZCRCE) + CRC16.
func (r *receiver) readZFileSubpacket() error {
	data, _, err := r.readSubpacket()
	if err != nil {
		return fmt.Errorf("ZMODEM: ZFILE info: %w", err)
	}
	// First NUL splits filename from metadata.
	nul := -1
	for i, b := range data {
		if b == 0 {
			nul = i
			break
		}
	}
	if nul < 0 {
		// No NUL terminator — accept the whole field as a filename.
		r.filename = string(data)
		return nil
	}
	r.filename = string(data[:nul])
	// Metadata: "size mtime mode serial files left". We only care about size.
	rest := data[nul+1:]
	if end := indexByte(rest, 0); end >= 0 {
		rest = rest[:end]
	}
	fields := strings.Fields(string(rest))
	if len(fields) >= 1 {
		if n, err := strconv.Atoi(fields[0]); err == nil && n >= 0 {
			r.totalSize = n
		}
	}
	return nil
}

// receiveData reads ZDATA bursts and ZEOF. Each burst is a sequence of
// subpackets; the terminator on each subpacket tells us whether to expect
// more, ACK first, or stop and look for the next header.
func (r *receiver) receiveData() error {
	for {
		frame, count, err := r.readHeader()
		if err != nil {
			return err
		}
		switch frame {
		case FrameZDATA:
			if int(count) != len(r.out) {
				// Sender resumed at an offset we don't have yet. Ask it to
				// rewind to where we actually are. This shouldn't happen
				// against our own sender (we always reply ZRPOS(0)) but
				// some implementations check on every burst.
				if _, werr := r.conn.Write(r.buildHeader(FrameZRPOS, uint32(len(r.out)))); werr != nil {
					return werr
				}
				continue
			}
			done, err := r.readDataBurst()
			if err != nil {
				return err
			}
			if done {
				// A subpacket told us "no more data in this frame, ack
				// requested" (ZCRCW) — the sender is now waiting for our
				// ZACK before it sends the next ZDATA or ZEOF. Already
				// handled inside readDataBurst.
			}
		case FrameZEOF:
			if int(count) != len(r.out) {
				// Length mismatch — request retransmit from where we are.
				if _, werr := r.conn.Write(r.buildHeader(FrameZRPOS, uint32(len(r.out)))); werr != nil {
					return werr
				}
				continue
			}
			return nil
		case FrameZRQINIT, FrameZRINIT:
			// Sender is re-advertising; resend ZRINIT to nudge it forward.
			if err := r.sendZRINIT(); err != nil {
				return err
			}
		case FrameZFIN, FrameZABORT, FrameZFERR:
			return fmt.Errorf("ZMODEM: sender aborted mid-transfer (frame %d)", frame)
		default:
			// Unknown / unexpected: ask for retransmit at our current offset.
			if _, werr := r.conn.Write(r.buildHeader(FrameZRPOS, uint32(len(r.out)))); werr != nil {
				return werr
			}
		}
	}
}

// readDataBurst reads subpackets following a ZDATA header until a frame
// terminator says the burst is done. Each accepted subpacket extends r.out.
// On CRC failure, sends ZRPOS(currentOffset) and returns nil so the caller's
// next readHeader() picks up either a fresh ZDATA at our offset or a
// repeated ZEOF.
func (r *receiver) readDataBurst() (done bool, err error) {
	for {
		data, kind, err := r.readSubpacket()
		if err != nil {
			// Request retransmit at our current offset.
			if _, werr := r.conn.Write(r.buildHeader(FrameZRPOS, uint32(len(r.out)))); werr != nil {
				return false, werr
			}
			// Drain any leftover sender bytes from the bad burst so the
			// next header read starts clean. Best effort.
			r.reader.drain()
			return false, nil
		}
		r.out = append(r.out, data...)
		if r.cfg.OnStatus != nil {
			r.cfg.OnStatus(ReceiveStatus{Bytes: len(r.out), Total: r.totalSize})
		}
		switch kind {
		case ZCRCG:
			// More subpackets coming, no ACK requested.
		case ZCRCQ:
			// More subpackets coming, but ACK requested first.
			if _, werr := r.conn.Write(r.buildHeader(FrameZACK, uint32(len(r.out)))); werr != nil {
				return false, werr
			}
		case ZCRCE:
			// End of burst, no ACK — next header follows.
			return true, nil
		case ZCRCW:
			// End of burst, ACK then next header.
			if _, werr := r.conn.Write(r.buildHeader(FrameZACK, uint32(len(r.out)))); werr != nil {
				return false, werr
			}
			return true, nil
		default:
			return false, fmt.Errorf("ZMODEM: unknown subpacket terminator 0x%02x", kind)
		}
	}
}

// awaitFrame loops reading headers until one matches `want`. Used during
// the ZFIN tail.
func (r *receiver) awaitFrame(want byte) error {
	for {
		frame, _, err := r.readHeader()
		if err != nil {
			return err
		}
		if frame == want {
			return nil
		}
	}
}

// readOOBestEffort drains up to two bytes looking for the "OO" terminator.
// The sender writes OO and closes; we just want to consume those bytes
// before we hand the conn back. Failures are ignored.
func (r *receiver) readOOBestEffort() {
	for i := 0; i < 4; i++ {
		if _, err := r.reader.readByte(2 * time.Second); err != nil {
			return
		}
	}
}

// ---------- Header parsing ----------

// readHeader reads bytes until a complete ZHEX or ZBIN header arrives.
// Returns the frame type and the 4-byte payload (offset/flags).
//
// We only emit ZBIN (CRC16) headers ourselves and we advertised CRC16-only
// to the sender, so we never expect ZBIN32 in normal operation. If one
// arrives we ignore it and keep scanning — better than hard-failing on a
// non-conforming peer.
func (r *receiver) readHeader() (byte, uint32, error) {
	for {
		b, err := r.reader.readByte(receiveTimeout)
		if err != nil {
			return 0, 0, err
		}
		if b != ZPAD {
			continue
		}
		// One ZPAD seen — peek ahead.
		b2, err := r.reader.readByte(receiveTimeout)
		if err != nil {
			return 0, 0, err
		}
		switch {
		case b2 == ZPAD:
			// ZHEX form: ZPAD ZPAD ZDLE 'B' <14 hex chars> CR LF [XON]
			b3, err := r.reader.readByte(receiveTimeout)
			if err != nil {
				return 0, 0, err
			}
			if b3 != ZDLE {
				continue
			}
			b4, err := r.reader.readByte(receiveTimeout)
			if err != nil {
				return 0, 0, err
			}
			if b4 != ZHEX {
				continue
			}
			frame, count, ok, err := r.readHexBody()
			if err != nil {
				return 0, 0, err
			}
			if !ok {
				continue
			}
			return frame, count, nil
		case b2 == ZDLE:
			b3, err := r.reader.readByte(receiveTimeout)
			if err != nil {
				return 0, 0, err
			}
			switch b3 {
			case ZBIN:
				frame, count, ok, err := r.readBinBody(false)
				if err != nil {
					return 0, 0, err
				}
				if !ok {
					continue
				}
				return frame, count, nil
			case ZBIN32:
				frame, count, ok, err := r.readBin32Body()
				if err != nil {
					return 0, 0, err
				}
				if !ok {
					continue
				}
				// Some vintage senders (Amiga xprzmodem and similar) latch
				// onto CRC32 the moment they think the receiver advertised
				// any meaningful buffer size. From this point on, all data
				// subpackets and our reciprocated responses are CRC32.
				r.crc32Mode = true
				return frame, count, nil
			default:
				continue
			}
		default:
			// Spurious ZPAD; keep scanning.
		}
	}
}

// readHexBody reads the 14 hex chars + CR LF (and optional XON) tail of a
// ZHEX header. Returns ok=false on a parse / CRC failure so the outer loop
// can keep scanning.
func (r *receiver) readHexBody() (byte, uint32, bool, error) {
	hex := make([]byte, 14)
	for i := range hex {
		b, err := r.reader.readByte(receiveTimeout)
		if err != nil {
			return 0, 0, false, err
		}
		hex[i] = b
	}
	frame, flags, ok := DecodeHexHeader(hex, 0)
	if !ok {
		return 0, 0, false, nil
	}
	// We deliberately don't try to drain the CR/LF/XON trailer here. Spec
	// allows several variants: '\r\n[XON]', '\r\x8a[XON]', or just '\r\n'
	// for ZACK/ZFIN — and lrzsz uses '\r\x8a' which trips up any
	// "wait for plain 0x0a" loop. The next readHeader() scans for ZPAD
	// and naturally skips whatever trailer bytes were on the wire, so
	// there's nothing to gain from consuming them eagerly.
	payload := []byte{frame, flags[0], flags[1], flags[2], flags[3]}
	expected := uint16(decodeHexByte(hex, 10))<<8 | uint16(decodeHexByte(hex, 12))
	if CRC16(payload) != expected {
		return 0, 0, false, nil
	}
	count := uint32(flags[0]) | uint32(flags[1])<<8 | uint32(flags[2])<<16 | uint32(flags[3])<<24
	return frame, count, true, nil
}

// decodeHexByte reads two ASCII hex chars at offset and returns the byte.
// 0 on parse error (caller's CRC check will catch the corruption).
func decodeHexByte(buf []byte, offset int) byte {
	if offset+2 > len(buf) {
		return 0
	}
	hi, ok1 := hexNybble(buf[offset])
	lo, ok2 := hexNybble(buf[offset+1])
	if !ok1 || !ok2 {
		return 0
	}
	return (hi << 4) | lo
}

// readBin32Body reads a ZBIN32 (CRC32) header body: 5 escaped payload bytes
// + 4 escaped CRC32 bytes (little-endian). Returns ok=false on CRC mismatch.
func (r *receiver) readBin32Body() (byte, uint32, bool, error) {
	payload, err := r.readEscapedBytes(5)
	if err != nil {
		return 0, 0, false, err
	}
	crcBytes, err := r.readEscapedBytes(4)
	if err != nil {
		return 0, 0, false, err
	}
	expected := uint32(crcBytes[0]) | uint32(crcBytes[1])<<8 |
		uint32(crcBytes[2])<<16 | uint32(crcBytes[3])<<24
	if CRC32(payload) != expected {
		return 0, 0, false, nil
	}
	count := uint32(payload[1]) | uint32(payload[2])<<8 |
		uint32(payload[3])<<16 | uint32(payload[4])<<24
	return payload[0], count, true, nil
}

// readBinBody reads a ZBIN (CRC16) header body: 5 escaped payload bytes
// + 2 escaped CRC bytes. Returns ok=false on CRC mismatch.
func (r *receiver) readBinBody(_ bool) (byte, uint32, bool, error) {
	payload, err := r.readEscapedBytes(5)
	if err != nil {
		return 0, 0, false, err
	}
	crcBytes, err := r.readEscapedBytes(2)
	if err != nil {
		return 0, 0, false, err
	}
	expected := uint16(crcBytes[0])<<8 | uint16(crcBytes[1])
	if CRC16(payload) != expected {
		return 0, 0, false, nil
	}
	count := uint32(payload[1]) | uint32(payload[2])<<8 | uint32(payload[3])<<16 | uint32(payload[4])<<24
	return payload[0], count, true, nil
}

// readEscapedBytes reads n logical (post-unescape) bytes. ZDLE+x is mapped
// back via unescapeByte. Returns an error if the stream ends mid-escape.
func (r *receiver) readEscapedBytes(n int) ([]byte, error) {
	out := make([]byte, 0, n)
	for len(out) < n {
		b, err := r.reader.readByte(receiveTimeout)
		if err != nil {
			return nil, err
		}
		if b == ZDLE {
			b2, err := r.reader.readByte(receiveTimeout)
			if err != nil {
				return nil, err
			}
			out = append(out, unescapeByte(b2))
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// unescapeByte inverts the ZDLE escape mapping. Most bytes follow the simple
// XOR 0x40 rule, but the spec carves out two specials — 'l' (0x6c) decodes
// to 0x7f (DEL) and 'm' (0x6d) to 0xff. Without those carve-outs payloads
// containing DEL or 0xff round-trip incorrectly and the subpacket CRC fails.
func unescapeByte(b byte) byte {
	switch b {
	case 0x6c:
		return 0x7f
	case 0x6d:
		return 0xff
	default:
		return b ^ 0x40
	}
}

// ---------- Subpacket parsing ----------

// readSubpacket reads one subpacket that started immediately after a ZFILE
// or ZDATA header: data bytes (with ZDLE-escapes), then ZDLE+terminator,
// then 2 escaped CRC16 bytes. Validates the CRC16 over (data + terminator).
//
// Returns the unescaped data, the terminator kind, and an error iff the
// CRC failed or the read aborted. CRC failures don't drain past the bad
// CRC bytes — the outer code requests retransmit and lets readHeader's
// junk-skipping logic resync on the next header.
func (r *receiver) readSubpacket() ([]byte, byte, error) {
	data := make([]byte, 0, subpacketSize)
	for {
		b, err := r.reader.readByte(receiveTimeout)
		if err != nil {
			return nil, 0, err
		}
		if b != ZDLE {
			data = append(data, b)
			continue
		}
		b2, err := r.reader.readByte(receiveTimeout)
		if err != nil {
			return nil, 0, err
		}
		switch b2 {
		case ZCRCE, ZCRCG, ZCRCQ, ZCRCW:
			crcInput := make([]byte, len(data)+1)
			copy(crcInput, data)
			crcInput[len(data)] = b2
			if r.crc32Mode {
				crcBytes, err := r.readEscapedBytes(4)
				if err != nil {
					return nil, 0, err
				}
				expected := uint32(crcBytes[0]) | uint32(crcBytes[1])<<8 |
					uint32(crcBytes[2])<<16 | uint32(crcBytes[3])<<24
				if CRC32(crcInput) != expected {
					return nil, b2, fmt.Errorf("ZMODEM: subpacket CRC32 mismatch")
				}
			} else {
				crcBytes, err := r.readEscapedBytes(2)
				if err != nil {
					return nil, 0, err
				}
				expected := uint16(crcBytes[0])<<8 | uint16(crcBytes[1])
				if CRC16(crcInput) != expected {
					return nil, b2, fmt.Errorf("ZMODEM: subpacket CRC mismatch")
				}
			}
			return data, b2, nil
		default:
			data = append(data, unescapeByte(b2))
		}
	}
}

// ---------- Helpers ----------

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

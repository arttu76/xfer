// Package zmodem implements a ZMODEM file-transfer sender, tuned for old
// terminal programs (Amiga Term 4.8, NComm, xprzmodem.library, lrzsz,
// SyncTerm). The wire-format primitives (frame headers, ZDLE escape table,
// CRC16) live in this file; the state machine lives in sender.go.
//
// Reference: Chuck Forsberg, "The ZMODEM Inter Application File Transfer
// Protocol" (1988).
package zmodem

// Protocol bytes.
const (
	ZPAD = 0x2a // '*'
	ZDLE = 0x18 // Ctrl-X escape marker
	XON  = 0x11
)

// Encoding markers (the byte immediately after ZPAD ZDLE in a header).
const (
	ZBIN   = 0x41 // 'A' — CRC16 binary header
	ZHEX   = 0x42 // 'B' — ASCII-hex header
	ZBIN32 = 0x43 // 'C' — CRC32 binary header (we don't emit these)
)

// Frame types.
const (
	FrameZRQINIT = 0
	FrameZRINIT  = 1
	FrameZSINIT  = 2
	FrameZACK    = 3
	FrameZFILE   = 4
	FrameZSKIP   = 5
	FrameZNAK    = 6
	FrameZABORT  = 7
	FrameZFIN    = 8
	FrameZRPOS   = 9
	FrameZDATA   = 10
	FrameZEOF    = 11
	FrameZFERR   = 12
)

// Subpacket terminators (the byte after ZDLE that ends a subpacket).
const (
	ZCRCE = 0x68 // 'h' — end of frame, no ACK wanted
	ZCRCG = 0x69 // 'i' — data continues, CRC next, no ACK
	ZCRCQ = 0x6a // 'j' — data continues, CRC next, ZACK expected
	ZCRCW = 0x6b // 'k' — end of frame, CRC next, ZACK expected
)

// ZRINIT capability flags (ZF0).
const (
	CanFDX   = 0x01
	CanOVIO  = 0x02
	CanBRK   = 0x04
	CanCRY   = 0x08
	CanLZW   = 0x10
	CanFC32  = 0x20
	EscCtl   = 0x40
	Esc8     = 0x80
)

// ZdleTable maps each byte to its on-wire form. When ZdleTable[b] != b, the
// sender must prepend ZDLE. The canonical default set escapes 0x0d, 0x10,
// 0x11, 0x13, 0x18 and their high-bit twins; 0x7f→0x6c and 0xff→0x6d are
// the two non-XOR special cases from the spec.
//
// EnableEscctl/DisableEscctl mutate the table in place per session: the
// table is effectively a global because the receiver's ESCCTL preference
// isn't known until we sniff ZRINIT, and we'd rather patch once than branch
// on every escape call.
var ZdleTable [256]byte

var escctlDeltas = [][2]byte{}

func init() {
	for i := 0; i < 256; i++ {
		ZdleTable[i] = byte(i)
	}
	for k, v := range map[byte]byte{
		0x0d: 0x4d, 0x10: 0x50, 0x11: 0x51, 0x13: 0x53, 0x18: 0x58, 0x7f: 0x6c,
		0x8d: 0xcd, 0x90: 0xd0, 0x91: 0xd1, 0x93: 0xd3, 0xff: 0x6d,
	} {
		ZdleTable[k] = v
	}
	// Precompute the extra escapes that ESCCTL adds — any control byte not
	// already escaped, plus its high-bit twin.
	for i := 0; i < 0x20; i++ {
		if ZdleTable[i] == byte(i) {
			escctlDeltas = append(escctlDeltas, [2]byte{byte(i), byte(i) ^ 0x40})
		}
		hi := byte(i) | 0x80
		if ZdleTable[hi] == hi {
			escctlDeltas = append(escctlDeltas, [2]byte{hi, hi ^ 0x40})
		}
	}
}

// EnableEscctl adds the "all control bytes escaped" deltas to the table.
func EnableEscctl() {
	for _, d := range escctlDeltas {
		ZdleTable[d[0]] = d[1]
	}
}

// DisableEscctl restores the canonical table.
func DisableEscctl() {
	for _, d := range escctlDeltas {
		ZdleTable[d[0]] = d[0]
	}
}

// AppendEscaped appends each byte from src to dst, inserting ZDLE escapes
// where the current table demands.
func AppendEscaped(dst []byte, src []byte) []byte {
	for _, b := range src {
		esc := ZdleTable[b]
		if esc != b {
			dst = append(dst, ZDLE)
		}
		dst = append(dst, esc)
	}
	return dst
}

// CRC16 is the XMODEM CRC (poly 0x1021, init 0) used for ZBIN (CRC16) frames.
func CRC16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// BuildZbinHeader emits a ZBIN (CRC16) header: ZPAD ZDLE 'A' + escaped
// 5-byte payload + escaped CRC16 (big-endian).
func BuildZbinHeader(frame byte, count uint32) []byte {
	out := []byte{ZPAD, ZDLE, ZBIN}
	payload := []byte{
		frame,
		byte(count),
		byte(count >> 8),
		byte(count >> 16),
		byte(count >> 24),
	}
	out = AppendEscaped(out, payload)
	crc := CRC16(payload)
	out = AppendEscaped(out, []byte{byte(crc >> 8), byte(crc)})
	return out
}

// BuildZhexHeader emits a ZHEX (CRC16, ASCII-hex) header: ZPAD ZPAD ZDLE 'B'
// + 14 ASCII hex chars (7 bytes encoded) + CR LF XON. Used for ZRQINIT
// and ZFIN where escaping isn't needed and human-readability helps.
func BuildZhexHeader(frame byte, count uint32) []byte {
	payload := []byte{
		frame,
		byte(count),
		byte(count >> 8),
		byte(count >> 16),
		byte(count >> 24),
	}
	crc := CRC16(payload)
	full := append(payload, byte(crc>>8), byte(crc))
	hex := make([]byte, 0, 16)
	const hexDigits = "0123456789abcdef"
	for _, b := range full {
		hex = append(hex, hexDigits[b>>4], hexDigits[b&0xf])
	}
	out := []byte{ZPAD, ZPAD, ZDLE, ZHEX}
	out = append(out, hex...)
	// CR LF trailer. Per spec, every hex frame ends with XON for flow
	// resume, EXCEPT ZACK and ZFIN — those omit it so the receiver doesn't
	// confuse the XON with flow control during session teardown.
	out = append(out, 0x0d, 0x0a)
	if frame != FrameZACK && frame != FrameZFIN {
		out = append(out, XON)
	}
	return out
}

// DecodeHexHeader parses 14 ASCII hex chars starting at `p` in buf, returning
// (frameType, flags[4], ok). The flags[4] array packs ZP0..ZP3 in order.
func DecodeHexHeader(buf []byte, p int) (frame byte, flags [4]byte, ok bool) {
	if p+14 > len(buf) {
		return
	}
	vals := [7]byte{}
	for i := 0; i < 7; i++ {
		hi, h1 := hexNybble(buf[p+2*i])
		lo, h2 := hexNybble(buf[p+2*i+1])
		if !h1 || !h2 {
			return 0, [4]byte{}, false
		}
		vals[i] = (hi << 4) | lo
	}
	frame = vals[0]
	flags = [4]byte{vals[1], vals[2], vals[3], vals[4]}
	ok = true
	return
}

func hexNybble(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}

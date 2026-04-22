package xmodem

import (
	"errors"
	"io"
	"os"
	"time"
)

// XMODEM control bytes.
const (
	SOH = 0x01
	EOT = 0x04
	ACK = 0x06
	NAK = 0x15
	CAN = 0x18
	SUB = 0x1a // CP/M padding
	CRC = 0x43 // 'C' — receiver requests CRC-16 mode
)

const (
	BlockSize         = 128
	maxTimeoutsInARow = 5
)

// mode selects the checksum variant negotiated by the receiver.
type mode int

const (
	modeChecksum mode = iota + 1
	modeCrc
)

// StatusEvent is emitted after each acknowledged block so callers can surface
// progress + ETA to the user.
type StatusEvent struct {
	Block       int // 1-indexed block that was just acknowledged
	TotalBlocks int
}

// Config lets callers tune timing and subscribe to progress updates.
// Leave ReadTimeout zero to use the default.
type Config struct {
	ReadTimeout time.Duration     // per-read timeout; default 10s
	OnStatus    func(StatusEvent) // called after each ACK (may be nil)
	OnStart     func()            // called after CRC/NAK received (may be nil)
}

// Send transfers `data` via XMODEM over conn. The caller is responsible for
// reading from the connection only through this function for the duration of
// the transfer (we ReadDeadline-drive the session).
func Send(conn deadlineConn, data []byte, cfg Config) error {
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 10 * time.Second
	}
	totalBlocks := (len(data) + BlockSize - 1) / BlockSize
	if totalBlocks == 0 {
		totalBlocks = 1 // always send at least one (padded) block
	}

	m, err := waitForStart(conn, cfg.ReadTimeout)
	if err != nil {
		return err
	}
	if cfg.OnStart != nil {
		cfg.OnStart()
	}

	for blk := 1; blk <= totalBlocks; blk++ {
		start := (blk - 1) * BlockSize
		end := start + BlockSize
		if end > len(data) {
			end = len(data)
		}
		packet := buildPacket(byte(blk&0xff), data[start:end], m)

		if err := sendBlockWithRetries(conn, packet, cfg.ReadTimeout); err != nil {
			return err
		}
		if cfg.OnStatus != nil {
			cfg.OnStatus(StatusEvent{Block: blk, TotalBlocks: totalBlocks})
		}
	}

	return finishEOT(conn, cfg.ReadTimeout)
}

// deadlineConn is the subset of net.Conn we need. A plain *bytes.Buffer won't
// work — tests supply a pipe-backed connection that supports SetReadDeadline.
type deadlineConn interface {
	io.ReadWriter
	SetReadDeadline(t time.Time) error
}

// waitForStart reads until the receiver sends NAK (checksum mode) or 'C' (CRC).
// Accumulates up to maxTimeoutsInARow read timeouts before giving up.
func waitForStart(conn deadlineConn, timeout time.Duration) (mode, error) {
	buf := make([]byte, 1)
	timeouts := 0
	for {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		_, err := conn.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				timeouts++
				if timeouts >= maxTimeoutsInARow {
					return 0, errors.New("XMODEM: receiver never sent start byte")
				}
				continue
			}
			return 0, err
		}
		switch buf[0] {
		case NAK:
			return modeChecksum, nil
		case CRC:
			return modeCrc, nil
		case CAN:
			return 0, errors.New("XMODEM: receiver cancelled before start")
		}
		// Ignore other bytes (stray echoes from the terminal line).
	}
}

// sendBlockWithRetries writes the packet, waits for ACK/NAK, retransmits on NAK.
func sendBlockWithRetries(conn deadlineConn, packet []byte, timeout time.Duration) error {
	const maxErrors = 10
	errCount := 0
	for {
		if _, err := conn.Write(packet); err != nil {
			return err
		}
		resp, err := readOne(conn, timeout)
		if err != nil {
			return err
		}
		switch resp {
		case ACK:
			return nil
		case NAK:
			errCount++
			if errCount > maxErrors {
				return errors.New("XMODEM: too many NAKs for single block")
			}
			continue
		case CAN:
			return errors.New("XMODEM: receiver cancelled mid-transfer")
		default:
			// Ignore unknown response and keep waiting.
		}
	}
}

// finishEOT drives the two-step EOT handshake: sender emits EOT; receiver may
// NAK (request resend) then ACK, or ACK directly.
func finishEOT(conn deadlineConn, timeout time.Duration) error {
	for tries := 0; tries < 10; tries++ {
		if _, err := conn.Write([]byte{EOT}); err != nil {
			return err
		}
		resp, err := readOne(conn, timeout)
		if err != nil {
			return err
		}
		if resp == ACK {
			return nil
		}
		// NAK or anything else → retransmit EOT.
	}
	return errors.New("XMODEM: receiver never ACKed EOT")
}

func readOne(conn deadlineConn, timeout time.Duration) (byte, error) {
	buf := make([]byte, 1)
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	_, err := conn.Read(buf)
	if err != nil {
		return 0, err
	}
	return buf[0], nil
}

// buildPacket assembles a single XMODEM packet:
//   SOH, block, ~block, 128 data bytes (pad 0x1a), then 1-byte checksum
//   (modeChecksum) or 2-byte CRC-16/CCITT (modeCrc).
func buildPacket(block byte, chunk []byte, m mode) []byte {
	packet := make([]byte, 0, 3+BlockSize+2)
	packet = append(packet, SOH, block, ^block)
	packet = append(packet, chunk...)
	for len(packet) < 3+BlockSize {
		packet = append(packet, SUB)
	}
	data := packet[3 : 3+BlockSize]
	if m == modeCrc {
		c := CRC16Ccitt(data)
		packet = append(packet, byte(c>>8), byte(c&0xff))
	} else {
		packet = append(packet, checksum(data))
	}
	return packet
}

// CRC16Ccitt computes XMODEM CRC-16 (poly 0x1021, init 0, no reflection) over data.
// Exported so tests can cross-check.
func CRC16Ccitt(data []byte) uint16 {
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

func checksum(data []byte) byte {
	var sum byte
	for _, b := range data {
		sum += b
	}
	return sum
}


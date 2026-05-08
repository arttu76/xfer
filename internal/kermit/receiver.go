package kermit

import (
	"errors"
	"fmt"
	"time"
)

// ReceiveConfig mirrors Config but for the receive direction.
type ReceiveConfig struct {
	ReadTimeout time.Duration     // default 30s; bounds how long we wait for any packet
	OnStart     func()            // called once we have ACKed the S packet
	OnStatus    func(StatusEvent) // called after each D packet is accepted
}

// ReceiveResult bundles what came off the wire.
type ReceiveResult struct {
	Data     []byte
	Filename string
	Size     int  // value reported via the '!' attribute, 0 if unknown
	IsText   bool // true iff sender's '"' attribute said 'A' (ASCII text)
}

// Receive runs the Kermit receiver state machine on conn until the sender
// signals end-of-transaction (B packet) or aborts (E packet). Returns the
// reassembled file payload plus the filename advertised in the F packet.
//
// The receiver advertises support for type-3 CRC, 8-bit quoting, RLE,
// long packets, attribute packets, and a small sliding window — but
// downgrades each individually based on what the sender proposed in the
// S packet.
func Receive(conn deadlineConn, cfg ReceiveConfig) (ReceiveResult, error) {
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	r := &receiver{
		conn:    conn,
		cfg:     cfg,
		chkLen:  1,
		qctl:    '#',
		qbin:    '&',
		reptCh:  '~',
		expSeq:  0,
		lastSeq: -1,
	}
	return r.run()
}

type receiver struct {
	conn deadlineConn
	cfg  ReceiveConfig

	// Negotiated post-S parameters.
	chkLen  int
	qctl    byte
	qbin    byte
	reptCh  byte
	use8bit bool
	useRept bool

	// Sequence tracking. expSeq is the seq we expect next; lastSeq is the
	// most recently accepted seq (-1 before any packet has been processed).
	expSeq  int
	lastSeq int

	out []byte
	res ReceiveResult
}

// run drives the receiver until a B packet (end of transaction) or an E
// packet (sender abort) arrives.
func (r *receiver) run() (ReceiveResult, error) {
	const maxJunkPackets = 100 // hard ceiling on parse-error retries
	junk := 0
	started := false

	for {
		p, err := r.readNext()
		if err != nil {
			if junk++; junk > maxJunkPackets {
				return ReceiveResult{}, fmt.Errorf("kermit: too many parse errors: %w", err)
			}
			// Best-effort NAK; ignore write errors (we'll surface the read
			// error on the next iteration if the conn is truly broken).
			_ = r.writeNak(r.expSeq)
			continue
		}
		junk = 0

		// Duplicate (sender's previous ACK got lost): re-ACK without
		// re-processing.
		if r.lastSeq >= 0 && p.seq == r.lastSeq {
			_ = r.writeAck(p.seq, nil)
			continue
		}
		// Wrong seq: drop and NAK to coax a retransmit.
		if p.seq != r.expSeq {
			_ = r.writeNak(r.expSeq)
			continue
		}

		switch p.typ {
		case 'S':
			reply := r.negotiateAndApply(p.data)
			// ACK to S is type-1 by spec — the negotiated check kicks in
			// only for SUBSEQUENT packets.
			if err := r.writeAckRaw(p.seq, reply, 1); err != nil {
				return ReceiveResult{}, err
			}
			r.advanceSeq(p.seq)
			if !started && r.cfg.OnStart != nil {
				r.cfg.OnStart()
				started = true
			}

		case 'F':
			r.res.Filename = string(p.data)
			if err := r.writeAck(p.seq, nil); err != nil {
				return ReceiveResult{}, err
			}
			r.advanceSeq(p.seq)

		case 'A':
			size, ftype := parseRecvAttributes(p.data)
			if size != "" {
				if n, ok := atoiPositive(size); ok {
					r.res.Size = n
				}
			}
			r.res.IsText = ftype == 'A'
			if err := r.writeAck(p.seq, nil); err != nil {
				return ReceiveResult{}, err
			}
			r.advanceSeq(p.seq)

		case 'D':
			chunk := decodeKermitData(p.data, r.qctl, r.qbin, r.reptCh, r.use8bit, r.useRept)
			r.out = append(r.out, chunk...)
			if err := r.writeAck(p.seq, nil); err != nil {
				return ReceiveResult{}, err
			}
			r.advanceSeq(p.seq)
			if r.cfg.OnStatus != nil {
				r.cfg.OnStatus(StatusEvent{BytesSent: len(r.out), TotalBytes: r.res.Size})
			}

		case 'Z':
			if err := r.writeAck(p.seq, nil); err != nil {
				return ReceiveResult{}, err
			}
			r.advanceSeq(p.seq)

		case 'B':
			_ = r.writeAck(p.seq, nil)
			r.res.Data = r.out
			return r.res, nil

		case 'E':
			return ReceiveResult{}, fmt.Errorf("%w: %s", ErrSenderAborted, string(p.data))

		default:
			// Unknown packet types: NAK and let the sender retry. Some
			// senders (C-Kermit) may emit X (text-display) or G (generic
			// command) packets; we don't support those.
			_ = r.writeNak(r.expSeq)
		}
	}
}

// readNext reads one packet with the current chkLen, applying the configured
// read timeout. A timeout is reported to the caller so it can decide whether
// to NAK or give up.
func (r *receiver) readNext() (*packet, error) {
	if err := r.conn.SetReadDeadline(time.Now().Add(r.cfg.ReadTimeout)); err != nil {
		return nil, err
	}
	return readPacket(r.conn, r.chkLen)
}

func (r *receiver) advanceSeq(seq int) {
	r.lastSeq = seq
	r.expSeq = (seq + 1) & 0x3f
}

func (r *receiver) writeAck(seq int, data []byte) error {
	return r.writeAckRaw(seq, data, r.chkLen)
}

func (r *receiver) writeAckRaw(seq int, data []byte, chkLen int) error {
	_, err := r.conn.Write(buildShortPacket(seq, 'Y', data, chkLen))
	return err
}

func (r *receiver) writeNak(seq int) error {
	_, err := r.conn.Write(buildShortPacket(seq, 'N', nil, r.chkLen))
	return err
}

// negotiateAndApply parses the sender's S-packet proposal, picks the richest
// set of features both sides agree on, applies them to r, and returns the
// 13-byte reply data field for the ACK-S.
func (r *receiver) negotiateAndApply(s []byte) []byte {
	// Conservative defaults — what we use if the proposal is missing fields.
	chkLen := 1
	chkt := byte('1')
	qctl := byte('#')
	qbin := byte('N') // no 8-bit unless sender opts in
	rept := byte(' ') // no RLE unless sender opts in
	use8bit := false
	useRept := false
	maxl := byte(tocharMax)
	windo := byte(1)
	var capasOut byte
	maxlx1, maxlx2 := byte(0), byte(0)

	// Position 6 (QCTL): mirror sender's choice; default '#' if invalid.
	if len(s) >= 6 {
		if c := s[5]; (c >= 33 && c <= 62) || (c >= 96 && c <= 126) {
			qctl = c
		}
	}
	// Position 7 (QBIN): pick 8-bit iff sender named '&'/'Y' or a valid
	// printable. Both sides naming 'Y' resolves to '&' (the default).
	if len(s) >= 7 {
		switch q := s[6]; q {
		case 'Y':
			qbin = 'Y'
			use8bit = true
		case '&':
			qbin = '&'
			use8bit = true
		default:
			if (q >= 33 && q <= 62) || (q >= 96 && q <= 126) {
				qbin = q
				use8bit = true
			}
		}
	}
	// Position 8 (CHKT): take whatever the sender proposed (we support all).
	if len(s) >= 8 {
		switch s[7] {
		case '1':
			chkt, chkLen = '1', 1
		case '2':
			chkt, chkLen = '2', 2
		case '3':
			chkt, chkLen = '3', 3
		}
	}
	// Position 9 (REPT): if sender named '~', enable RLE.
	if len(s) >= 9 && s[8] == '~' {
		rept = '~'
		useRept = true
	}
	// Position 10 (CAPAS): mirror long-packet + attribute support; sliding
	// windows handled below via WINDO.
	supportsLong := false
	if len(s) >= 10 {
		theirs := unchar(s[9])
		if theirs&capLongPackets != 0 {
			supportsLong = true
			capasOut |= capLongPackets
		}
		if theirs&capAttributes != 0 {
			capasOut |= capAttributes
		}
	}
	// Position 11 (WINDO): we accept up to 8.
	if len(s) >= 11 {
		w := int(unchar(s[10]))
		if w > 8 {
			w = 8
		}
		if w < 1 {
			w = 1
		}
		windo = byte(w)
		if w > 1 {
			capasOut |= capSlidingWin
		}
	}
	// Positions 12/13 (MAXLX1/MAXLX2): cap to 1024 to match what we tested.
	if supportsLong {
		// Accept up to 1024 bytes per long packet.
		const ourLong = 1024
		maxlx1 = byte(ourLong / 95)
		maxlx2 = byte(ourLong % 95)
	}

	// Apply to receiver state.
	r.chkLen = chkLen
	r.qctl = qctl
	if qbin == 'Y' {
		r.qbin = '&'
	} else {
		r.qbin = qbin
	}
	r.use8bit = use8bit
	r.useRept = useRept
	r.reptCh = '~'

	return []byte{
		tochar(maxl),     // MAXL
		tochar(5),        // PTIME
		tochar(0),        // NPAD
		ctl(0),           // PADC
		tochar(CR),       // EOL
		qctl,             // QCTL
		qbin,             // QBIN
		chkt,             // CHKT
		rept,             // REPT
		tochar(capasOut), // CAPAS
		tochar(windo),    // WINDO
		tochar(maxlx1),   // MAXLX1
		tochar(maxlx2),   // MAXLX2
	}
}

// decodeKermitData is the receive-side inverse of encodeData. Production
// code path; mirrors the test helper that lives in kermit_test.go.
func decodeKermitData(in []byte, qctl, qbin, reptCh byte, use8bit, useRept bool) []byte {
	out := make([]byte, 0, len(in))
	i := 0
	for i < len(in) {
		if useRept && in[i] == reptCh {
			i++
			if i >= len(in) {
				break
			}
			runLen := int(unchar(in[i]))
			i++
			c, consumed := decodeOneByte(in[i:], qctl, qbin, use8bit)
			i += consumed
			for k := 0; k < runLen; k++ {
				out = append(out, c)
			}
			continue
		}
		c, consumed := decodeOneByte(in[i:], qctl, qbin, use8bit)
		i += consumed
		out = append(out, c)
	}
	return out
}

// decodeOneByte decodes one logical source byte from in[0:]; returns the
// byte and the number of input bytes consumed.
func decodeOneByte(in []byte, qctl, qbin byte, use8bit bool) (byte, int) {
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
		// Kermit quoting: after QCTL, ctl(x) = x ^ 0x40 decodes back to a
		// control byte iff the result is in the control range. Otherwise
		// the quoted byte stands for itself (covers quoted QCTL/QBIN/REPT).
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

// parseRecvAttributes pulls the '!' (size) and '"' (type) attributes out of
// an A-packet data field. Other attributes are ignored.
func parseRecvAttributes(data []byte) (size string, ftype byte) {
	i := 0
	for i < len(data) {
		tag := data[i]
		i++
		if i >= len(data) {
			break
		}
		length := int(unchar(data[i]))
		i++
		if length < 0 || i+length > len(data) {
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

func atoiPositive(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return 0, false
		}
	}
	return n, true
}

// Sentinel errors callers may inspect to render a friendlier message.
var ErrSenderAborted = errors.New("kermit: sender aborted")

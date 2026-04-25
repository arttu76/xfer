package session

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/solvalou/xfer/internal/constants"
)

// detectReplyRegex matches the standard CSI cursor-position report:
// ESC [ <rows> ; <cols> R. Searched anywhere in the buffer so stray bytes
// before or after the response don't break parsing.
var detectReplyRegex = regexp.MustCompile(`\x1b\[(\d+);(\d+)R`)

// DefaultDetectTimeout bounds how long we wait for a CSI-aware terminal
// to answer the cursor-position query. Modern terminals reply in
// milliseconds, but vintage terminals over a wifi modem can take ~1 s
// (Term 4.8 on Amiga has been measured at 1.05 s); two seconds is enough
// headroom for that round trip and short enough that a silent terminal
// doesn't make connect feel completely hung.
const DefaultDetectTimeout = 2 * time.Second

// detectStragglerWindow is a short post-decision drain — any bytes that
// arrive within this window after we've decided the probe outcome are
// discarded so a late reply (e.g. a terminal that answered just past our
// deadline) doesn't bleed into the menu prompt as bogus user input.
const detectStragglerWindow = 150 * time.Millisecond

// DetectTerminalSize probes the just-connected client for its terminal
// dimensions and returns cols, rows, detected. The returned cols/rows are
// always sane: when the probe fails, fallbackCols/fallbackRows are used.
//
// Sequence:
//  1. Print "Detecting terminal size..." so any echoed garbage on a
//     non-CSI terminal is framed by an explanation the user can read.
//  2. Send ESC[s ESC[999;999H ESC[6n ESC[u — save, jump past edge
//     (terminals clamp), report cursor position, restore.
//  3. Wait up to detectTimeout for ESC[<rows>;<cols>R.
//  4. Print either "Terminal size: WxH" or "Terminal size not detected,
//     using WxH" so the user sees the result of the probe.
func DetectTerminalSize(conn net.Conn, fallbackCols, fallbackRows int, timeout time.Duration) (cols, rows int, detected bool) {
	cols = fallbackCols
	rows = fallbackRows
	if timeout <= 0 {
		timeout = DefaultDetectTimeout
	}

	if _, err := conn.Write([]byte("Detecting terminal size...\r\n")); err != nil {
		return cols, rows, false
	}
	if _, err := conn.Write([]byte("\x1b[s\x1b[999;999H\x1b[6n\x1b[u")); err != nil {
		return cols, rows, false
	}

	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	var buf []byte
	chunk := make([]byte, 64)
	for {
		n, err := conn.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if m := detectReplyRegex.FindSubmatch(buf); m != nil {
				r, _ := strconv.Atoi(string(m[1]))
				c, _ := strconv.Atoi(string(m[2]))
				if c >= constants.TermMinWidth && c <= constants.TermMaxWidth &&
					r >= constants.TermMinHeight && r <= constants.TermMaxHeight {
					cols, rows, detected = c, r, true
				}
				// Whether accepted or rejected, the terminal has already
				// answered — no point waiting another full timeout for a
				// second reply that won't arrive.
				break
			}
		}
		if err != nil {
			break
		}
	}

	// Absorb any bytes that arrive shortly after the decision so a late
	// reply (terminal missed our deadline by tens of ms) doesn't echo
	// into the menu prompt as bogus user input.
	drainStragglers(conn, detectStragglerWindow)

	if detected {
		_, _ = conn.Write([]byte(fmt.Sprintf("Terminal size: %dx%d\r\n", cols, rows)))
	} else {
		_, _ = conn.Write([]byte(fmt.Sprintf("Terminal size not detected, using %dx%d\r\n", cols, rows)))
	}
	return cols, rows, detected
}

// drainStragglers reads-and-discards bytes from conn for up to window.
// Used right after detection so a late probe reply doesn't end up at
// the next prompt. The deadline is restored to "no deadline" by the
// outer caller's deferred SetReadDeadline.
func drainStragglers(conn net.Conn, window time.Duration) {
	_ = conn.SetReadDeadline(time.Now().Add(window))
	scratch := make([]byte, 64)
	for {
		if _, err := conn.Read(scratch); err != nil {
			return
		}
	}
}

// ResolveTerminalSize either runs the detection probe or returns the
// configured fallback dimensions, depending on cfg.TermDetect. Centralizing
// this lets the connection handler stay branch-free and gives tests a
// single function to exercise the flag-vs-probe plumbing.
func ResolveTerminalSize(conn net.Conn, cfg *Config) (cols, rows int, detected bool) {
	if !cfg.TermDetect {
		return cfg.TermWidth, cfg.TermHeight, false
	}
	return DetectTerminalSize(conn, cfg.TermWidth, cfg.TermHeight, cfg.TermDetectTimeout)
}

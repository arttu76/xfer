package session

import (
	"net"
	"time"
)

type Mode int

const (
	ModeNavigate Mode = iota
	ModeConfirmTransfer
	ModeTransferFile
	ModeView
	ModeEnterURL
)

type Context struct {
	Mode Mode
	Path string
	Conn net.Conn

	// TermWidth/TermHeight are the client's terminal dimensions, populated
	// once at connect time by DetectTerminalSize. Always non-zero — the
	// detector falls back to the default when the probe fails.
	TermWidth  int
	TermHeight int

	// RequestedFile is the full path (for local files) or URL (for URL
	// downloads) of the selected source — used for logs and for the
	// protocol prompt header.
	RequestedFile string

	// RequestedName is the short display name shown to the user in the
	// "Ready to download …" banner and used as the filename advertised
	// to ZMODEM/Kermit receivers. Set by the navigator or URL handler.
	RequestedName string

	// RequestedBody holds the file contents to transfer. Populated by
	// the navigator when selecting a local file, or by the URL handler
	// after a successful download. Transfer handlers read from this
	// slice — they never re-open the source — so every path that
	// returns to the listing must clear the buffer.
	RequestedBody []byte

	// View holds opaque per-connection state while Mode == ModeView.
	// The session package never inspects it; the viewer package owns the type.
	View any

	// NavState holds opaque per-connection navigator state — currently the
	// directory-listing pager so it can resume across reads while the user
	// answers "[M]ore, [S]earch". The session package never inspects it.
	NavState any
}

type Config struct {
	SecureMode bool
	NoURL      bool

	// TermDetect enables the connect-time ANSI cursor-position probe. When
	// false, TermWidth/TermHeight are used directly with no probe and no
	// "Detecting…/Terminal size:" lines on the wire.
	TermDetect bool
	// TermWidth/TermHeight are the fallback dimensions used when detection
	// is disabled or fails. Validated against constants.TermMin/Max bounds
	// at startup.
	TermWidth  int
	TermHeight int

	// TermDetectTimeout bounds how long DetectTerminalSize waits for the
	// terminal's reply. Zero falls back to DefaultDetectTimeout.
	TermDetectTimeout time.Duration
}

// Write/Writeln send text to the client terminal. Writeln appends CRLF since
// the receiving side is typically a serial/telnet terminal expecting it.
func (c *Context) Write(s string) error {
	_, err := c.Conn.Write([]byte(s))
	return err
}

func (c *Context) Writeln(s string) error {
	_, err := c.Conn.Write([]byte(s + "\r\n"))
	return err
}

package session

import "net"

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
}

type Config struct {
	SecureMode bool
	NoURL      bool
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

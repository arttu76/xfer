package session

import "net"

type Mode int

const (
	ModeNavigate Mode = iota
	ModeConfirmTransfer
	ModeTransferFile
	ModeView
)

type Context struct {
	Mode          Mode
	Path          string
	Conn          net.Conn
	RequestedFile string
	// View holds opaque per-connection state while Mode == ModeView.
	// The session package never inspects it; the viewer package owns the type.
	View any
}

type Config struct {
	SecureMode bool
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

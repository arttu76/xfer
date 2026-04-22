package session

import "net"

type Mode int

const (
	ModeNavigate Mode = iota
	ModeConfirmTransfer
	ModeTransferFile
)

type Context struct {
	Mode          Mode
	Path          string
	Conn          net.Conn
	RequestedFile string
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

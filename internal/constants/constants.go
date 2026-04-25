package constants

const (
	DefaultPort = 23
	MinPort     = 0
	MaxPort     = 65535

	DirectoryPrefix = "<D>"
	FilePrefix      = "   "

	// Terminal size bounds shared by the connect-time auto-detect (in
	// internal/session) and the viewer's manual layout-override prompt.
	TermDefaultWidth  = 40
	TermDefaultHeight = 20
	TermMinWidth      = 20
	TermMinHeight     = 4
	TermMaxWidth      = 500
	TermMaxHeight     = 200
)

package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/solvalou/xfer/internal/constants"
	"github.com/solvalou/xfer/internal/logger"
	"github.com/solvalou/xfer/internal/navigator"
	"github.com/solvalou/xfer/internal/protocol"
	"github.com/solvalou/xfer/internal/session"
	"github.com/solvalou/xfer/internal/urlconsole"
	"github.com/solvalou/xfer/internal/urlfetch"
	"github.com/solvalou/xfer/internal/viewer"
)

var version = "1.2.0"

func main() {
	port := flag.Int("p", constants.DefaultPort, "port to use")
	flag.IntVar(port, "port", constants.DefaultPort, "port to use")
	dir := flag.String("d", "", "directory to serve (default: current directory)")
	flag.StringVar(dir, "directory", "", "directory to serve (default: current directory)")
	secure := flag.Bool("s", false, "secure mode: don't allow user to change directories")
	flag.BoolVar(secure, "secure", false, "secure mode")
	noURL := flag.Bool("n", false, "disallow the [U]RL download option in the file listing")
	flag.BoolVar(noURL, "no-url", false, "disallow URL downloads")
	noStdinURL := flag.Bool("c", false, "do not inject stdin lines into a client's URL prompt")
	flag.BoolVar(noStdinURL, "no-stdin-url", false, "do not inject stdin lines into a client's URL prompt")
	showVersion := flag.Bool("V", false, "print version and exit")
	flag.BoolVar(showVersion, "version", false, "print version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "xfer v%s — XMODEM / ZMODEM / Kermit file server + viewer for old computers\n\n", version)
		fmt.Fprintf(os.Stderr, "Usage: %s [flags]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  -p, --port <number>       port to use (default: %d)\n", constants.DefaultPort)
		fmt.Fprintf(os.Stderr, "  -d, --directory <string>  directory to serve (default: current directory)\n")
		fmt.Fprintf(os.Stderr, "  -s, --secure              secure mode: don't allow user to change directories\n")
		fmt.Fprintf(os.Stderr, "  -n, --no-url              disallow the [U]RL download option in the file listing\n")
		fmt.Fprintf(os.Stderr, "  -c, --no-stdin-url        do not inject stdin lines into a client's URL prompt\n")
		fmt.Fprintf(os.Stderr, "  -V, --version             print version and exit\n")
		fmt.Fprintf(os.Stderr, "  -h, --help                print this help and exit\n")
	}
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if *port < constants.MinPort || *port > constants.MaxPort {
		fmt.Fprintf(os.Stderr, "invalid port: %d\n", *port)
		os.Exit(2)
	}

	initialPath := *dir
	if initialPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot determine cwd: %v\n", err)
			os.Exit(1)
		}
		initialPath = cwd
	}
	abs, err := filepath.Abs(initialPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot resolve directory: %v\n", err)
		os.Exit(1)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "%s is not a valid directory\n", initialPath)
		os.Exit(2)
	}

	cfg := &session.Config{SecureMode: *secure, NoURL: *noURL}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	defer listener.Close()

	ips := serverIPs()
	endpoints := make([]string, 0, len(ips))
	for _, ip := range ips {
		endpoints = append(endpoints, fmt.Sprintf("%s:%d", ip, *port))
	}
	logger.Info(fmt.Sprintf("Server now listening on %s", strings.Join(endpoints, " / ")))

	// Set up the server-side URL-paste registry. The Run loop reads stdin
	// line by line and forwards each line to whichever client is in URL
	// mode; if stdin is closed (daemon, </dev/null) Scanner returns cleanly
	// so the goroutine just exits.
	reg := urlconsole.NewRegistry(logger.Info)
	if !*noStdinURL {
		go urlconsole.Run(reg, os.Stdin)
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			logger.Error(fmt.Sprintf("accept: %v", err))
			continue
		}
		go handleConnection(conn, abs, cfg, reg)
	}
}

// handleConnection drives one client through the state machine —
// navigate → confirm → transfer (or view, or URL entry) → back to the
// listing — dispatching each read to the handler for the current mode.
//
// Reads are lifted into a dedicated goroutine + buffered channel so the
// main loop can select over both the client's input and a second stream
// of bytes injected by the server-side URL paste feature. The dedicated
// reader lets either source wake up the dispatcher without the other
// having to cooperate.
func handleConnection(conn net.Conn, initialPath string, cfg *session.Config, reg *urlconsole.Registry) {
	defer conn.Close()
	logger.Info("Client connected")
	defer logger.Info("Client disconnected")

	ctx := &session.Context{
		Mode: session.ModeNavigate,
		Path: initialPath,
		Conn: conn,
	}
	navigator.ListFiles(ctx, cfg)

	var inputBuffer strings.Builder

	// connDone is closed when this function returns so the reader goroutine
	// unblocks even if it's parked waiting to send into readCh.
	connDone := make(chan struct{})
	defer close(connDone)

	readCh := make(chan []byte, 4)
	go func() {
		defer close(readCh)
		buf := make([]byte, 256)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			chunk := append([]byte(nil), buf[:n]...)
			select {
			case readCh <- chunk:
			case <-connDone:
				return
			}
		}
	}()

	injectCh := make(chan []byte, 4)
	var injectRegID int // 0 = not registered
	defer func() {
		if injectRegID != 0 {
			reg.Deregister(injectRegID)
		}
	}()

	for {
		// Keep the registry entry in sync with the current mode. Entering
		// URL mode registers us so the server's stdin can inject into this
		// session; leaving deregisters so stdin flows elsewhere (or nowhere).
		switch {
		case ctx.Mode == session.ModeEnterURL && injectRegID == 0:
			injectRegID = reg.Register(injectCh)
			logger.Info(fmt.Sprintf("session #%d waiting for URL — paste here or type on the client", injectRegID))
		case ctx.Mode != session.ModeEnterURL && injectRegID != 0:
			reg.Deregister(injectRegID)
			injectRegID = 0
		}

		// A nil channel blocks forever in select, so conditionally enable
		// the inject branch only while in URL mode.
		var injectSrc <-chan []byte
		if ctx.Mode == session.ModeEnterURL {
			injectSrc = injectCh
		}

		var data []byte
		select {
		case d, ok := <-readCh:
			if !ok {
				return // peer disconnected
			}
			data = d
		case d := <-injectSrc:
			data = d
		}

		if ctx.Mode == session.ModeTransferFile {
			// Transfers own the socket while running; if bytes land here it
			// means the handler exited without restoring mode. Discard.
			continue
		}
		if ctx.Mode == session.ModeNavigate {
			handleNavigateInput(ctx, cfg, data, &inputBuffer)
			continue
		}
		if ctx.Mode == session.ModeConfirmTransfer {
			done := func(name string, c *session.Context) session.OnDone {
				return func(ok bool, code int) {
					protocol.ShowTransferComplete(c, cfg, name, ok, code)
				}
			}
			protocol.ConfirmAndStartTransfer(ctx, string(data), cfg,
				func(c *session.Context) { session.XmodemTransfer(c, cfg, done("XMODEM", c)) },
				func(c *session.Context) { session.ZmodemTransfer(c, cfg, done("ZMODEM", c)) },
				func(c *session.Context) { session.KermitTransfer(c, cfg, done("KERMIT", c)) },
				func(c *session.Context) { viewer.Start(c, cfg) })
			continue
		}
		if ctx.Mode == session.ModeView {
			viewer.HandleInput(ctx, cfg, data)
			continue
		}
		if ctx.Mode == session.ModeEnterURL {
			handleURLInput(ctx, cfg, data, &inputBuffer)
			continue
		}
	}
}

// handleNavigateInput accepts per-character keyboard input: digits,
// backspace, newline to confirm the current number, R to refresh, X to exit.
func handleNavigateInput(ctx *session.Context, cfg *session.Config, data []byte, buf *strings.Builder) {
	input := string(data)
	if input == "" {
		return
	}
	trimmed := strings.TrimSpace(input)
	if trimmed != "" {
		switch trimmed[0] | 0x20 { // fold ASCII uppercase to lowercase
		case 'x':
			_ = ctx.Writeln("Goodbye!")
			_ = ctx.Conn.Close()
			return
		case 'r':
			_ = ctx.Writeln("Refreshing...")
			navigator.ListFiles(ctx, cfg)
			buf.Reset()
			return
		case 'u':
			if cfg.NoURL {
				// Feature disabled — silently ignore so the letter doesn't
				// accidentally get interpreted as something else below.
				return
			}
			ctx.Mode = session.ModeEnterURL
			buf.Reset()
			_ = ctx.Writeln("")
			_ = ctx.Write("Enter URL (empty=back): ")
			return
		}
	}
	for _, r := range input {
		if r == '\r' || r == '\n' {
			_ = ctx.Writeln("")
			numStr := buf.String()
			buf.Reset()
			n := atoiSafe(numStr)
			navigator.SelectFile(ctx, n, cfg, func(c *session.Context) {
				protocol.ShowProtocolPrompt(c)
			})
			return
		}
		if buf.Len() > 0 && (r == '\b' || r == 0x7f) {
			s := buf.String()
			buf.Reset()
			buf.WriteString(s[:len(s)-1])
			_, _ = ctx.Conn.Write([]byte{'\b', ' ', '\b'})
			continue
		}
		if unicode.IsDigit(r) {
			buf.WriteRune(r)
			_, _ = ctx.Conn.Write([]byte(string(r)))
		}
	}
}

// handleURLInput collects a URL one byte at a time. Enter submits; an empty
// submission returns to the listing; otherwise we attempt the download and
// on failure re-prompt from the same mode so the user can correct a typo
// without having to walk out and back in.
func handleURLInput(ctx *session.Context, cfg *session.Config, data []byte, buf *strings.Builder) {
	for _, b := range data {
		if b == '\r' || b == '\n' {
			_ = ctx.Writeln("")
			raw := strings.TrimSpace(buf.String())
			buf.Reset()
			if raw == "" {
				navigator.ListFiles(ctx, cfg)
				return
			}
			logger.Info(fmt.Sprintf("URL fetch: %s", raw))
			_ = ctx.Writeln(fmt.Sprintf("Downloading %s ...", raw))
			body, name, err := urlfetch.Fetch(raw)
			if err != nil {
				logger.Error(fmt.Sprintf("URL fetch failed: %v", err))
				_ = ctx.Writeln(fmt.Sprintf("Error: %v", err))
				_ = ctx.Write("Enter URL (empty=back): ")
				return
			}
			logger.Info(fmt.Sprintf("URL fetch OK: %s (%d bytes)", name, len(body)))
			ctx.RequestedFile = raw
			ctx.RequestedName = name
			ctx.RequestedBody = body
			ctx.Mode = session.ModeConfirmTransfer
			navigator.AnnounceBuffered(ctx)
			protocol.ShowProtocolPrompt(ctx)
			return
		}
		if (b == '\b' || b == 0x7f) && buf.Len() > 0 {
			s := buf.String()
			buf.Reset()
			buf.WriteString(s[:len(s)-1])
			_, _ = ctx.Conn.Write([]byte{'\b', ' ', '\b'})
			continue
		}
		// Printable ASCII only — URLs don't legitimately contain anything
		// below 0x20 or above 0x7e, and restricting here keeps garbled
		// terminal escape sequences out of the buffer.
		if b >= 0x20 && b < 0x7f {
			buf.WriteByte(b)
			_, _ = ctx.Conn.Write([]byte{b})
		}
	}
}

func atoiSafe(s string) int {
	n := 0
	if s == "" {
		return -1
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return -1
		}
	}
	return n
}

func serverIPs() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return []string{"localhost"}
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		out = append(out, ip.String())
	}
	if len(out) == 0 {
		return []string{"localhost"}
	}
	return out
}

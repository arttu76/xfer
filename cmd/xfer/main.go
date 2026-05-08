package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/solvalou/xfer/internal/constants"
	"github.com/solvalou/xfer/internal/logger"
	"github.com/solvalou/xfer/internal/navigator"
	"github.com/solvalou/xfer/internal/protocol"
	"github.com/solvalou/xfer/internal/session"
	"github.com/solvalou/xfer/internal/urlconsole"
	"github.com/solvalou/xfer/internal/urlfetch"
	"github.com/solvalou/xfer/internal/viewer"
	"github.com/solvalou/xfer/internal/wirelog"
)

var version = "1.3.0"

func main() {
	port := flag.Int("p", constants.DefaultPort, "port to use")
	flag.IntVar(port, "port", constants.DefaultPort, "port to use")
	dir := flag.String("d", "", "directory to serve (default: current directory)")
	flag.StringVar(dir, "directory", "", "directory to serve (default: current directory)")
	secure := flag.Bool("s", false, "secure mode: don't allow user to change directories")
	flag.BoolVar(secure, "secure", false, "secure mode")
	noURL := flag.Bool("n", false, "disallow the [U]RL download option in the file listing")
	flag.BoolVar(noURL, "no-url", false, "disallow URL downloads")
	noUpload := flag.Bool("no-upload", false, "disallow the [P]ut upload option in the file listing")
	onlyX := flag.Bool("onlyx", false, "force XMODEM for every transfer (skip protocol prompt; cancel/view no longer reachable)")
	onlyZ := flag.Bool("onlyz", false, "force ZMODEM for every transfer (skip protocol prompt; cancel/view no longer reachable)")
	onlyK := flag.Bool("onlyk", false, "force Kermit for every transfer (skip protocol prompt; cancel/view no longer reachable)")
	noStdinURL := flag.Bool("c", false, "do not inject stdin lines into a client's URL prompt")
	flag.BoolVar(noStdinURL, "no-stdin-url", false, "do not inject stdin lines into a client's URL prompt")
	wireLog := flag.String("w", "", "hexdump every byte each direction to this file (\"-\" for stderr)")
	flag.StringVar(wireLog, "wirelog", "", "hexdump every byte each direction to this file")
	termWidth := flag.Int("term-width", constants.TermDefaultWidth, "default/fallback terminal width")
	termHeight := flag.Int("term-height", constants.TermDefaultHeight, "default/fallback terminal height")
	noTermDetect := flag.Bool("no-term-detect", false, "skip terminal-size auto-detection on connect")
	termDetectTimeoutMs := flag.Int("term-detect-timeout", int(session.DefaultDetectTimeout/time.Millisecond), "how long to wait for the terminal-size probe reply, in milliseconds")
	showVersion := flag.Bool("V", false, "print version and exit")
	flag.BoolVar(showVersion, "version", false, "print version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "xfer v%s — XMODEM / ZMODEM / Kermit file server + viewer for old computers\n\n", version)
		fmt.Fprintf(os.Stderr, "Usage: %s [flags]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  -p, --port <number>       port to use (default: %d)\n", constants.DefaultPort)
		fmt.Fprintf(os.Stderr, "  -d, --directory <string>  directory to serve (default: current directory)\n")
		fmt.Fprintf(os.Stderr, "  -s, --secure              secure mode: don't allow user to change directories\n")
		fmt.Fprintf(os.Stderr, "  -n, --no-url              disallow the [U]RL download option in the file listing\n")
		fmt.Fprintf(os.Stderr, "      --no-upload           disallow the [P]ut upload option in the file listing\n")
		fmt.Fprintf(os.Stderr, "                            (implied by --secure)\n")
		fmt.Fprintf(os.Stderr, "      --onlyx               force XMODEM for every transfer (skip protocol prompt;\n")
		fmt.Fprintf(os.Stderr, "                            cancel/view no longer reachable)\n")
		fmt.Fprintf(os.Stderr, "      --onlyz               force ZMODEM for every transfer (skip protocol prompt;\n")
		fmt.Fprintf(os.Stderr, "                            cancel/view no longer reachable)\n")
		fmt.Fprintf(os.Stderr, "      --onlyk               force Kermit for every transfer (skip protocol prompt;\n")
		fmt.Fprintf(os.Stderr, "                            cancel/view no longer reachable)\n")
		fmt.Fprintf(os.Stderr, "  -c, --no-stdin-url        do not inject stdin lines into a client's URL prompt\n")
		fmt.Fprintf(os.Stderr, "  -w, --wirelog <path>      hexdump every wire byte to file (\"-\" for stderr)\n")
		fmt.Fprintf(os.Stderr, "      --term-width <n>      default/fallback terminal width (default: %d)\n", constants.TermDefaultWidth)
		fmt.Fprintf(os.Stderr, "      --term-height <n>     default/fallback terminal height (default: %d)\n", constants.TermDefaultHeight)
		fmt.Fprintf(os.Stderr, "      --no-term-detect      skip terminal-size auto-detection on connect\n")
		fmt.Fprintf(os.Stderr, "      --term-detect-timeout <ms> how long to wait for the probe reply (default: %d)\n", int(session.DefaultDetectTimeout/time.Millisecond))
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
	if *termWidth < constants.TermMinWidth || *termWidth > constants.TermMaxWidth {
		fmt.Fprintf(os.Stderr, "invalid --term-width %d (must be %d-%d)\n", *termWidth, constants.TermMinWidth, constants.TermMaxWidth)
		os.Exit(2)
	}
	if *termHeight < constants.TermMinHeight || *termHeight > constants.TermMaxHeight {
		fmt.Fprintf(os.Stderr, "invalid --term-height %d (must be %d-%d)\n", *termHeight, constants.TermMinHeight, constants.TermMaxHeight)
		os.Exit(2)
	}
	if *termDetectTimeoutMs < 1 {
		fmt.Fprintf(os.Stderr, "invalid --term-detect-timeout %d (must be >= 1 ms)\n", *termDetectTimeoutMs)
		os.Exit(2)
	}

	var forcedProtocol byte
	if n := boolCount(*onlyX, *onlyZ, *onlyK); n > 1 {
		fmt.Fprintln(os.Stderr, "--onlyx, --onlyz, --onlyk are mutually exclusive")
		os.Exit(2)
	} else if n == 1 {
		switch {
		case *onlyX:
			forcedProtocol = 'x'
		case *onlyZ:
			forcedProtocol = 'z'
		case *onlyK:
			forcedProtocol = 'k'
		}
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

	// --secure forces --no-upload: a host that has locked navigation also
	// shouldn't accept inbound files into the served tree.
	uploadDisabled := *noUpload || *secure

	cfg := &session.Config{
		SecureMode:        *secure,
		NoURL:             *noURL,
		NoUpload:          uploadDisabled,
		TermDetect:        !*noTermDetect,
		TermWidth:         *termWidth,
		TermHeight:        *termHeight,
		TermDetectTimeout: time.Duration(*termDetectTimeoutMs) * time.Millisecond,
		ForcedProtocol:    forcedProtocol,
	}

	if forcedProtocol != 0 {
		var name string
		switch forcedProtocol {
		case 'x':
			name = "XMODEM"
		case 'z':
			name = "ZMODEM"
		case 'k':
			name = "KERMIT"
		}
		logger.Info(fmt.Sprintf("Forcing %s for every transfer (protocol prompt skipped)", name))
	}

	var sink *wirelog.Sink
	if *wireLog != "" {
		s, err := wirelog.Open(*wireLog)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot open wire log %q: %v\n", *wireLog, err)
			os.Exit(1)
		}
		sink = s
		defer sink.Close()
		logger.Info(fmt.Sprintf("Wire log active: %s", *wireLog))
	}

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
		go handleConnection(wirelog.Wrap(conn, sink, conn.RemoteAddr().String()), abs, cfg, reg)
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

	// Probe the terminal for its size before any goroutine starts reading
	// from conn — the response (ESC[r;cR) needs to land in this read, not
	// in the input-reader goroutine that we spin up below.
	cols, rows, detected := session.ResolveTerminalSize(conn, cfg)
	if cfg.TermDetect {
		logger.Info(fmt.Sprintf("Terminal size %dx%d (detected=%v)", cols, rows, detected))
	} else {
		logger.Info(fmt.Sprintf("Terminal size %dx%d (detection disabled)", cols, rows))
	}

	ctx := &session.Context{
		Mode:       session.ModeNavigate,
		Path:       initialPath,
		Conn:       conn,
		TermWidth:  cols,
		TermHeight: rows,
	}
	navigator.ListFiles(ctx, cfg)

	var inputBuffer strings.Builder

	// connDone is closed when this function returns so the reader goroutine
	// unblocks even if it's parked waiting to send into readCh.
	connDone := make(chan struct{})
	defer close(connDone)

	// The input-reader goroutine must NOT be running concurrently with the
	// XMODEM/ZMODEM/Kermit transfer handlers — two goroutines racing on
	// conn.Read will split incoming packets between them, and any packet
	// won by this goroutine is silently dropped because we're in
	// ModeTransferFile. That manifested as Term 4.8 on the Amiga retrying
	// its ZRINIT two or three times because the first one(s) got eaten.
	// stop/start pairs bracket each transfer so the handler has the conn
	// to itself.
	var (
		readCh     chan []byte
		readerDone chan struct{}
	)
	startReader := func() {
		readCh = make(chan []byte, 4)
		readerDone = make(chan struct{})
		go func(ch chan<- []byte, done chan<- struct{}) {
			defer close(done)
			defer close(ch)
			buf := make([]byte, 256)
			for {
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				chunk := append([]byte(nil), buf[:n]...)
				select {
				case ch <- chunk:
				case <-connDone:
					return
				}
			}
		}(readCh, readerDone)
	}
	stopReader := func() {
		// Deadline-in-the-past kicks the goroutine out of any pending Read.
		_ = conn.SetReadDeadline(time.Now())
		<-readerDone
		_ = conn.SetReadDeadline(time.Time{})
	}
	startReader()

	// Hoisted start-fn closures: each transfer takes the conn for its own
	// protocol reader, so the input-reader goroutine must be paused for the
	// duration. These are referenced both by the existing ModeConfirmTransfer
	// / ModeUploadProtocol input handlers and by the auto-dispatch path that
	// fires when --onlyx/--onlyz/--onlyk skips the protocol prompt.
	withPause := func(f func(*session.Context)) func(*session.Context) {
		return func(c *session.Context) {
			stopReader()
			f(c)
			startReader()
		}
	}
	doneFn := func(name string, c *session.Context) session.OnDone {
		return func(ok bool, code int) {
			protocol.ShowTransferComplete(c, cfg, name, ok, code)
		}
	}
	startX := withPause(func(c *session.Context) { session.XmodemTransfer(c, cfg, doneFn("XMODEM", c)) })
	startZ := withPause(func(c *session.Context) { session.ZmodemTransfer(c, cfg, doneFn("ZMODEM", c)) })
	startK := withPause(func(c *session.Context) { session.KermitTransfer(c, cfg, doneFn("KERMIT", c)) })
	startV := func(c *session.Context) { viewer.Start(c, cfg) }
	startUploadZ := withPause(func(c *session.Context) { runZmodemUpload(c, cfg) })
	startUploadK := withPause(func(c *session.Context) { runKermitUpload(c, cfg) })

	// confirmReady is invoked when a transferable body has just been staged
	// (local pick or URL fetch landed in ModeConfirmTransfer). With a forced
	// protocol it dispatches the transfer immediately; otherwise it prints
	// the X/Z/K/V/C selection prompt and waits for a keystroke.
	confirmReady := func(c *session.Context) {
		if cfg.ForcedProtocol != 0 {
			protocol.ConfirmAndStartTransfer(c, string([]byte{cfg.ForcedProtocol}), cfg,
				startX, startZ, startK, startV)
			return
		}
		protocol.ShowProtocolPrompt(c)
	}
	// uploadStart is invoked by the [P]ut handler. With a forced protocol we
	// route through DispatchUploadProtocol directly (synthetic input byte) so
	// XMODEM still falls through to the filename-entry step and Z/K start
	// straight away. Otherwise we transition to ModeUploadProtocol and ask.
	// The leading newline matches the original handler — it breaks off the
	// listing's input line so the banner/prompt lands on its own row.
	uploadStart := func(c *session.Context) {
		_ = c.Writeln("")
		if cfg.ForcedProtocol != 0 {
			protocol.DispatchUploadProtocol(c, string([]byte{cfg.ForcedProtocol}), cfg,
				startUploadZ, startUploadK)
			return
		}
		c.Mode = session.ModeUploadProtocol
		protocol.ShowUploadProtocolPrompt(c)
	}

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
			if navigator.PagerActive(ctx) {
				rest := navigator.HandlePagerInput(ctx, cfg, data)
				if len(rest) == 0 {
					continue
				}
				// Pager deactivated mid-buffer because the user pressed a
				// final-menu shortcut at the [M]ore prompt; let the regular
				// handler interpret the unconsumed bytes (digit/U/P/R/X).
				data = rest
			}
			handleNavigateInput(ctx, cfg, data, &inputBuffer, confirmReady, uploadStart)
			continue
		}
		if ctx.Mode == session.ModeConfirmTransfer {
			protocol.ConfirmAndStartTransfer(ctx, string(data), cfg, startX, startZ, startK, startV)
			continue
		}
		if ctx.Mode == session.ModeView {
			viewer.HandleInput(ctx, cfg, data)
			continue
		}
		if ctx.Mode == session.ModeEnterURL {
			handleURLInput(ctx, cfg, data, &inputBuffer, confirmReady)
			continue
		}
		if ctx.Mode == session.ModeUploadProtocol {
			// One-keystroke menu — no buffering needed.
			protocol.DispatchUploadProtocol(ctx, string(data), cfg, startUploadZ, startUploadK)
			continue
		}
		if ctx.Mode == session.ModeEnterUploadName {
			handleUploadNameInput(ctx, cfg, data, &inputBuffer, func(c *session.Context) {
				// The receiver takes the conn for its own protocol reader,
				// so pause our input goroutine for the duration (same
				// pattern as the download dispatch).
				stopReader()
				runXmodemUpload(c, cfg)
				startReader()
			})
			continue
		}
	}
}

// handleNavigateInput accepts per-character keyboard input: digits,
// backspace, newline to confirm the current number, R to refresh, X to exit.
//
// confirmReady is invoked by SelectFile once a file body has been staged on
// ctx; it either shows the protocol prompt or, with --onlyx/z/k, dispatches
// the transfer immediately. uploadStart is invoked when the user presses
// [P] and similarly either prompts or auto-dispatches.
func handleNavigateInput(ctx *session.Context, cfg *session.Config, data []byte, buf *strings.Builder, confirmReady func(*session.Context), uploadStart func(*session.Context)) {
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
		case 's':
			navigator.BeginSearch(ctx, cfg)
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
		case 'p':
			if cfg.NoUpload {
				return
			}
			buf.Reset()
			uploadStart(ctx)
			return
		}
	}
	for _, r := range input {
		if r == '\r' || r == '\n' {
			_ = ctx.Writeln("")
			numStr := buf.String()
			buf.Reset()
			n := atoiSafe(numStr)
			navigator.SelectFile(ctx, n, cfg, confirmReady)
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
// without having to walk out and back in. confirmReady fires once the
// fetched body has been staged — same semantics as in handleNavigateInput.
func handleURLInput(ctx *session.Context, cfg *session.Config, data []byte, buf *strings.Builder, confirmReady func(*session.Context)) {
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
			confirmReady(ctx)
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

// handleUploadNameInput collects a destination filename one byte at a time
// (XMODEM has no in-protocol filename, so the user types it). Empty Enter
// cancels back to the listing; otherwise we stash the name on ctx and call
// startTransfer, which is expected to pause the input reader, drive the
// receiver, and resume the reader.
func handleUploadNameInput(ctx *session.Context, cfg *session.Config, data []byte, buf *strings.Builder, startTransfer func(*session.Context)) {
	for _, b := range data {
		if b == '\r' || b == '\n' {
			_ = ctx.Writeln("")
			name := strings.TrimSpace(buf.String())
			buf.Reset()
			if name == "" {
				navigator.ListFiles(ctx, cfg)
				return
			}
			ctx.UploadName = name
			startTransfer(ctx)
			return
		}
		if (b == '\b' || b == 0x7f) && buf.Len() > 0 {
			s := buf.String()
			buf.Reset()
			buf.WriteString(s[:len(s)-1])
			_, _ = ctx.Conn.Write([]byte{'\b', ' ', '\b'})
			continue
		}
		// Filenames: printable ASCII, no path separators (the navigator
		// helper will also reject those — this is just early echo
		// suppression so the user doesn't see what they can't submit).
		if b >= 0x20 && b < 0x7f && b != '/' && b != '\\' {
			buf.WriteByte(b)
			_, _ = ctx.Conn.Write([]byte{b})
		}
	}
}

// runXmodemUpload drives one XMODEM receive: it owns the conn for the
// duration (caller has paused the input reader), persists the file on
// success, and prints the completion banner before returning to the
// listing.
func runXmodemUpload(ctx *session.Context, cfg *session.Config) {
	name := ctx.UploadName
	session.XmodemReceive(ctx, cfg, name, func(success bool, n string, body []byte, errMsg string) {
		finishUpload(ctx, cfg, "XMODEM", n, body, success, errMsg)
	})
}

// runZmodemUpload drives one ZMODEM receive. Same mechanics as
// runXmodemUpload, but the destination filename comes from the ZFILE
// frame rather than the user, so we read it back off the OnReceive
// callback instead of stashing it on the context up front.
func runZmodemUpload(ctx *session.Context, cfg *session.Config) {
	session.ZmodemReceive(ctx, cfg, func(success bool, n string, body []byte, errMsg string) {
		finishUpload(ctx, cfg, "ZMODEM", n, body, success, errMsg)
	})
}

// runKermitUpload drives one Kermit receive. Filename is carried in the
// F packet.
func runKermitUpload(ctx *session.Context, cfg *session.Config) {
	session.KermitReceive(ctx, cfg, func(success bool, n string, body []byte, errMsg string) {
		finishUpload(ctx, cfg, "KERMIT", n, body, success, errMsg)
	})
}

// finishUpload persists the received bytes (if the transfer succeeded) and
// prints the completion banner. Shared across all three protocols so the
// "validate name → write file → log → banner" sequence stays consistent.
func finishUpload(ctx *session.Context, cfg *session.Config, proto, name string, body []byte, success bool, errMsg string) {
	if !success {
		protocol.ShowUploadComplete(ctx, cfg, proto, name, false, errMsg)
		return
	}
	dest, err := navigator.WriteUploadedFile(ctx, cfg, name, body)
	if err != nil {
		logger.Error(fmt.Sprintf("upload write failed: %v", err))
		protocol.ShowUploadComplete(ctx, cfg, proto, name, false, err.Error())
		return
	}
	logger.Info(fmt.Sprintf("Uploaded %s (%d bytes) to %s via %s", name, len(body), dest, proto))
	protocol.ShowUploadComplete(ctx, cfg, proto, name, true, "")
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
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

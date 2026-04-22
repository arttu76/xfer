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
)

const version = "1.6.1"

func main() {
	port := flag.Int("p", constants.DefaultPort, "port to use")
	flag.IntVar(port, "port", constants.DefaultPort, "port to use")
	dir := flag.String("d", "", "directory to serve (default: current directory)")
	flag.StringVar(dir, "directory", "", "directory to serve (default: current directory)")
	secure := flag.Bool("s", false, "secure mode: don't allow user to change directories")
	flag.BoolVar(secure, "secure", false, "secure mode")
	showVersion := flag.Bool("V", false, "print version and exit")
	flag.BoolVar(showVersion, "version", false, "print version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "xfer v%s — XMODEM / ZMODEM file server for retro computers\n\n", version)
		fmt.Fprintf(os.Stderr, "Usage: %s [flags]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  -p, --port <number>       port to use (default: %d)\n", constants.DefaultPort)
		fmt.Fprintf(os.Stderr, "  -d, --directory <string>  directory to serve (default: current directory)\n")
		fmt.Fprintf(os.Stderr, "  -s, --secure              secure mode: don't allow user to change directories\n")
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

	cfg := &session.Config{SecureMode: *secure}

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

	for {
		conn, err := listener.Accept()
		if err != nil {
			logger.Error(fmt.Sprintf("accept: %v", err))
			continue
		}
		go handleConnection(conn, abs, cfg)
	}
}

// handleConnection drives one client: file navigation → protocol confirm →
// transfer → back to file list. The Node version was event-driven and needed
// explicit listener bookkeeping to hand off the socket to the transfer
// engines; with blocking I/O + goroutines that layering disappears.
func handleConnection(conn net.Conn, initialPath string, cfg *session.Config) {
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
	buf := make([]byte, 256)

	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		if ctx.Mode == session.ModeTransferFile {
			// Transfers own the socket while running; if bytes land here it
			// means the handler exited without restoring mode. Discard.
			continue
		}
		data := buf[:n]
		if ctx.Mode == session.ModeNavigate {
			handleNavigateInput(ctx, cfg, data, &inputBuffer)
			continue
		}
		if ctx.Mode == session.ModeConfirmTransfer {
			protocol.ConfirmAndStartTransfer(ctx, string(data), cfg,
				func(c *session.Context) {
					session.XmodemTransfer(c, cfg, func(ok bool, code int) {
						protocol.ShowTransferComplete(c, cfg, "XMODEM", ok, code)
					})
				},
				func(c *session.Context) {
					session.ZmodemTransfer(c, cfg, func(ok bool) {
						protocol.ShowTransferComplete(c, cfg, "ZMODEM", ok, 0)
					})
				})
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

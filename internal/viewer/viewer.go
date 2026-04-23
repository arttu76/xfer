// Package viewer implements an interactive file viewer for old-terminal
// clients. Unlike the transfer protocols, it stays in a per-byte command
// loop driven by main.go — arrow-free, single-keystroke commands suitable
// for terminals that don't advertise cursor keys or line editing.
package viewer

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/solvalou/xfer/internal/logger"
	"github.com/solvalou/xfer/internal/navigator"
	"github.com/solvalou/xfer/internal/session"
)

type DisplayMode int

const (
	ModeChar DisplayMode = iota
	ModeHex
)

type promptMode int

const (
	promptNone promptMode = iota
	promptSearch
	promptLayoutWidth
	promptLayoutHeight
)

const (
	DefaultWidth  = 40
	DefaultHeight = 20
	MinWidth      = 20
	MinHeight     = 4
	MaxWidth      = 500
	MaxHeight     = 200
)

type state struct {
	data []byte
	name string

	mode    DisplayMode
	width   int
	height  int
	topLine int

	// charLines[i] is the byte offset of the first byte on display line i
	// when rendering in char mode. Rebuilt whenever data or width changes.
	charLines []int

	prompt     promptMode
	buffer     strings.Builder
	lastSearch string
	// lastMatchByte is the byte offset of the last successful search hit,
	// used so that repeating the same search advances past that hit rather
	// than re-finding it (the match may be mid-line, so the current top's
	// byte offset alone isn't enough). -1 means no recorded match.
	lastMatchByte int
	// pendingWidth buffers the width between the 'l' width and height prompts
	// so cancelling the height prompt leaves the old width untouched.
	pendingWidth int
}

// Start enters view mode using the already-staged body on ctx. The navigator
// (local pick) or URL handler has placed the bytes in ctx.RequestedBody and
// a display name in ctx.RequestedName before calling us, so the viewer
// never touches the disk.
func Start(ctx *session.Context, cfg *session.Config) {
	if ctx.RequestedBody == nil {
		logger.Error("viewer.Start called without a staged body")
		_ = ctx.Writeln("")
		_ = ctx.Writeln("Error: no file buffered for viewing")
		navigator.ListFiles(ctx, cfg)
		return
	}
	data := ctx.RequestedBody

	s := &state{
		data:          data,
		name:          ctx.RequestedName,
		width:         DefaultWidth,
		height:        DefaultHeight,
		lastMatchByte: -1,
	}
	if isBinary(data) {
		s.mode = ModeHex
	} else {
		s.mode = ModeChar
	}
	s.rebuildCharLines()

	ctx.View = s
	ctx.Mode = session.ModeView

	kind := "text"
	if s.mode == ModeHex {
		kind = "binary"
	}
	_ = ctx.Writeln("")
	_ = ctx.Writeln(fmt.Sprintf("--- %s (%d bytes, %s) --- press ? for help", s.name, len(data), kind))
	logger.Info(fmt.Sprintf("Viewing %s (%d bytes, %s)", ctx.RequestedFile, len(data), kind))
	s.draw(ctx)
}

// HandleInput routes bytes received from the client while Mode == ModeView.
// Commands are single-byte; search and layout prompts accept line input.
func HandleInput(ctx *session.Context, cfg *session.Config, data []byte) {
	s, ok := ctx.View.(*state)
	if !ok {
		exit(ctx, cfg)
		return
	}
	for _, b := range data {
		s.handleByte(ctx, cfg, b)
		if ctx.Mode != session.ModeView {
			return
		}
	}
}

func exit(ctx *session.Context, cfg *session.Config) {
	ctx.View = nil
	navigator.ListFiles(ctx, cfg)
}

func (s *state) handleByte(ctx *session.Context, cfg *session.Config, b byte) {
	switch s.prompt {
	case promptNone:
		s.handleCommand(ctx, cfg, b)
	case promptSearch:
		s.handleSearchInput(ctx, cfg, b)
	case promptLayoutWidth, promptLayoutHeight:
		s.handleLayoutInput(ctx, cfg, b)
	}
}

func (s *state) handleCommand(ctx *session.Context, cfg *session.Config, b byte) {
	key := b
	if key >= 'A' && key <= 'Z' {
		key += 0x20
	}
	switch key {
	case 'f':
		s.topLine++
		s.clampTop()
		s.draw(ctx)
	case 'b':
		s.topLine--
		s.clampTop()
		s.draw(ctx)
	case 'd', ' ':
		s.topLine += s.pageSize()
		s.clampTop()
		s.draw(ctx)
	case 'u':
		s.topLine -= s.pageSize()
		s.clampTop()
		s.draw(ctx)
	case 'm':
		s.toggleMode()
		s.draw(ctx)
	case 's':
		s.prompt = promptSearch
		s.buffer.Reset()
		if s.lastSearch != "" {
			_ = ctx.Write(fmt.Sprintf("\r\nsearch [%s]: ", s.lastSearch))
		} else {
			_ = ctx.Write("\r\nsearch: ")
		}
	case 'l':
		s.prompt = promptLayoutWidth
		s.buffer.Reset()
		_ = ctx.Write(fmt.Sprintf("\r\nterminal width [%d]: ", s.width))
	case '?':
		s.drawHelp(ctx)
	case 'q', 'c', 0x03, 0x04: // q, c, Ctrl-C, Ctrl-D
		_ = ctx.Writeln("")
		exit(ctx, cfg)
	case '\r', '\n':
		// re-render on bare Enter so the user can refresh after typing junk
		s.draw(ctx)
	default:
		// Ignore stray bytes silently — terminals send all sorts of things
		// (function-key escape sequences, XON/XOFF, etc.) that shouldn't
		// spam the screen with "unknown command".
	}
}

func (s *state) handleSearchInput(ctx *session.Context, cfg *session.Config, b byte) {
	if b == '\r' || b == '\n' {
		input := s.buffer.String()
		s.buffer.Reset()
		s.prompt = promptNone
		needle := input
		if needle == "" {
			needle = s.lastSearch
		}
		if needle == "" {
			_ = ctx.Writeln("")
			s.draw(ctx)
			return
		}
		// Repeat: blank input, or the same string the user just ran. In both
		// cases continue from just after the previous hit so we don't
		// re-land on it. Otherwise start from the current view position.
		isRepeat := input == "" || input == s.lastSearch
		var startByte int
		if isRepeat && s.lastMatchByte >= 0 {
			startByte = s.lastMatchByte + 1
		} else {
			startByte = s.currentByte()
		}
		s.lastSearch = needle
		s.doSearch(ctx, needle, startByte)
		return
	}
	if b == 0x7f || b == 0x08 {
		if s.buffer.Len() > 0 {
			str := s.buffer.String()
			s.buffer.Reset()
			s.buffer.WriteString(str[:len(str)-1])
			_, _ = ctx.Conn.Write([]byte{'\b', ' ', '\b'})
		}
		return
	}
	if b >= 0x20 && b < 0x7f {
		s.buffer.WriteByte(b)
		_, _ = ctx.Conn.Write([]byte{b})
	}
}

func (s *state) handleLayoutInput(ctx *session.Context, cfg *session.Config, b byte) {
	if b == '\r' || b == '\n' {
		val := strings.TrimSpace(s.buffer.String())
		s.buffer.Reset()
		if s.prompt == promptLayoutWidth {
			w := s.width
			if n, err := strconv.Atoi(val); err == nil && n >= MinWidth && n <= MaxWidth {
				w = n
			}
			s.pendingWidth = w
			s.prompt = promptLayoutHeight
			_ = ctx.Write(fmt.Sprintf("\r\nterminal height [%d]: ", s.height))
			return
		}
		// height phase
		h := s.height
		if n, err := strconv.Atoi(val); err == nil && n >= MinHeight && n <= MaxHeight {
			h = n
		}
		// Preserve visible position across width change.
		anchor := s.currentByte()
		s.width = s.pendingWidth
		s.height = h
		s.rebuildCharLines()
		s.setByteTopLine(anchor)
		s.clampTop()
		s.prompt = promptNone
		s.draw(ctx)
		return
	}
	if b == 0x7f || b == 0x08 {
		if s.buffer.Len() > 0 {
			str := s.buffer.String()
			s.buffer.Reset()
			s.buffer.WriteString(str[:len(str)-1])
			_, _ = ctx.Conn.Write([]byte{'\b', ' ', '\b'})
		}
		return
	}
	// digits only for layout
	if b >= '0' && b <= '9' {
		s.buffer.WriteByte(b)
		_, _ = ctx.Conn.Write([]byte{b})
	}
}

// toggleMode swaps display modes and preserves the byte offset at the top
// of the visible area so the user keeps their place.
func (s *state) toggleMode() {
	anchor := s.currentByte()
	if s.mode == ModeHex {
		s.mode = ModeChar
	} else {
		s.mode = ModeHex
	}
	s.setByteTopLine(anchor)
	s.clampTop()
}

func (s *state) pageSize() int {
	if s.height < 2 {
		return 1
	}
	return s.height - 1
}

func (s *state) totalLines() int {
	if s.mode == ModeHex {
		bpl := s.hexBytesPerLine()
		if bpl <= 0 {
			return 1
		}
		n := len(s.data) / bpl
		if len(s.data)%bpl != 0 {
			n++
		}
		if n == 0 {
			return 1
		}
		return n
	}
	if len(s.charLines) == 0 {
		return 1
	}
	return len(s.charLines)
}

func (s *state) clampTop() {
	total := s.totalLines()
	max := total - 1
	if max < 0 {
		max = 0
	}
	if s.topLine > max {
		s.topLine = max
	}
	if s.topLine < 0 {
		s.topLine = 0
	}
}

// hexBytesPerLine picks the largest power-of-two bytes-per-line that fits
// the terminal width, given the offset column and ASCII gutter.
// Format: "OOOOOO: HH HH .. HH aaaa..a"
// where OOOOOO is offsetDigits hex digits and the hex/ASCII gutter is 1 space.
// The tight gutter lets 8 bytes fit on a 40-wide terminal even with 6-digit
// offsets, which matters for browsing binaries > 64 KB.
func (s *state) hexBytesPerLine() int {
	od := s.offsetDigits()
	// overhead: od + 2 (": ") + 1 (gutter) - 1 (no trailing hex space after last byte) = od + 2
	overhead := od + 2
	avail := s.width - overhead
	if avail < 4 {
		return 1
	}
	// per byte: 3 hex + 1 ascii = 4
	n := avail / 4
	switch {
	case n >= 32:
		return 32
	case n >= 16:
		return 16
	case n >= 8:
		return 8
	case n >= 4:
		return 4
	case n >= 2:
		return 2
	default:
		return 1
	}
}

func (s *state) offsetDigits() int {
	if len(s.data) > 0xFFFFFFFF {
		return 16
	}
	if len(s.data) > 0xFFFFFF {
		return 8
	}
	if len(s.data) > 0xFFFF {
		return 6
	}
	return 4
}

func (s *state) currentByte() int {
	if s.mode == ModeHex {
		return s.topLine * s.hexBytesPerLine()
	}
	if len(s.charLines) == 0 {
		return 0
	}
	if s.topLine < 0 {
		return 0
	}
	if s.topLine >= len(s.charLines) {
		return s.charLines[len(s.charLines)-1]
	}
	return s.charLines[s.topLine]
}

// setByteTopLine positions topLine so the given byte offset is on or above
// the top visible line.
func (s *state) setByteTopLine(byteOffset int) {
	if byteOffset < 0 {
		byteOffset = 0
	}
	if s.mode == ModeHex {
		bpl := s.hexBytesPerLine()
		if bpl > 0 {
			s.topLine = byteOffset / bpl
		}
		return
	}
	if len(s.charLines) == 0 {
		s.topLine = 0
		return
	}
	// binary search: largest i such that charLines[i] <= byteOffset
	lo, hi := 0, len(s.charLines)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if s.charLines[mid] <= byteOffset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	s.topLine = lo
}

// rebuildCharLines splits the file into display lines: break on LF,
// discard bare CR, expand tabs to 8-column stops, and wrap at width.
func (s *state) rebuildCharLines() {
	lines := []int{0}
	col := 0
	for i := 0; i < len(s.data); i++ {
		b := s.data[i]
		if b == '\n' {
			if i+1 <= len(s.data) {
				lines = append(lines, i+1)
			}
			col = 0
			continue
		}
		if b == '\r' {
			continue
		}
		if b == '\t' {
			col += 8 - (col % 8)
		} else {
			col++
		}
		if col >= s.width {
			if i+1 < len(s.data) {
				lines = append(lines, i+1)
			}
			col = 0
		}
	}
	s.charLines = lines
}

func (s *state) draw(ctx *session.Context) {
	s.clampTop()
	_ = ctx.Write("\r\n")
	total := s.totalLines()
	maxLine := s.topLine + s.pageSize()
	if maxLine > total {
		maxLine = total
	}
	for i := s.topLine; i < maxLine; i++ {
		_ = ctx.Writeln(s.renderLine(i))
	}
	s.drawPrompt(ctx)
}

func (s *state) drawPrompt(ctx *session.Context) {
	modeName := "CHAR"
	if s.mode == ModeHex {
		modeName = "HEX"
	}
	total := s.totalLines()
	_ = ctx.Write(fmt.Sprintf("%s %d/%d fbdu mslq (?=help): ",
		modeName, s.topLine+1, total))
}

func (s *state) drawHelp(ctx *session.Context) {
	_ = ctx.Writeln("")
	_ = ctx.Writeln("Viewer commands:")
	_ = ctx.Writeln("  f / b   one line forward / back")
	_ = ctx.Writeln("  d / u   one page down / up (SPACE=d)")
	_ = ctx.Writeln("  m       toggle hex / char display")
	_ = ctx.Writeln("  s       search (blank=repeat last)")
	_ = ctx.Writeln("  l       set terminal width / height")
	_ = ctx.Writeln("  q / c   quit back to file list")
	s.drawPrompt(ctx)
}

func (s *state) renderLine(idx int) string {
	if s.mode == ModeHex {
		return s.renderHexLine(idx)
	}
	return s.renderCharLine(idx)
}

func (s *state) renderHexLine(idx int) string {
	bpl := s.hexBytesPerLine()
	start := idx * bpl
	if start >= len(s.data) {
		return ""
	}
	end := start + bpl
	if end > len(s.data) {
		end = len(s.data)
	}

	od := s.offsetDigits()
	var sb strings.Builder
	sb.Grow(s.width)
	sb.WriteString(fmt.Sprintf("%0*X: ", od, start))

	for i := 0; i < bpl; i++ {
		if start+i < end {
			sb.WriteString(fmt.Sprintf("%02X", s.data[start+i]))
		} else {
			sb.WriteString("  ")
		}
		if i < bpl-1 {
			sb.WriteByte(' ')
		}
	}
	sb.WriteByte(' ')
	for i := 0; i < bpl; i++ {
		if start+i < end {
			b := s.data[start+i]
			if b >= 0x20 && b < 0x7f {
				sb.WriteByte(b)
			} else {
				sb.WriteByte('.')
			}
		}
	}
	return sb.String()
}

func (s *state) renderCharLine(idx int) string {
	if idx < 0 || idx >= len(s.charLines) {
		return ""
	}
	start := s.charLines[idx]
	end := len(s.data)
	if idx+1 < len(s.charLines) {
		end = s.charLines[idx+1]
	}

	var sb strings.Builder
	sb.Grow(s.width)
	col := 0
	for i := start; i < end && col < s.width; i++ {
		b := s.data[i]
		switch {
		case b == '\n' || b == '\r':
			// line terminator not rendered
		case b == '\t':
			n := 8 - (col % 8)
			for j := 0; j < n && col < s.width; j++ {
				sb.WriteByte(' ')
				col++
			}
		case b >= 0x20 && b < 0x7f:
			sb.WriteByte(b)
			col++
		default:
			sb.WriteByte('.')
			col++
		}
	}
	return sb.String()
}

// doSearch finds the next occurrence of needle at or after startByte,
// wrapping to the beginning if needed. Case-insensitive over ASCII.
// Updates lastMatchByte so that repeated searches continue past the hit.
func (s *state) doSearch(ctx *session.Context, needle string, startByte int) {
	needleB := []byte(needle)
	if startByte < 0 {
		startByte = 0
	}
	if startByte > len(s.data) {
		startByte = len(s.data)
	}
	idx := indexCI(s.data, needleB, startByte)
	wrapped := false
	if idx < 0 && startByte > 0 {
		idx = indexCI(s.data, needleB, 0)
		wrapped = true
	}
	if idx < 0 {
		s.lastMatchByte = -1
		_ = ctx.Writeln("")
		_ = ctx.Writeln(fmt.Sprintf("Not found: %q", needle))
		s.drawPrompt(ctx)
		return
	}
	s.lastMatchByte = idx
	s.setByteTopLine(idx)
	s.clampTop()
	if wrapped {
		_ = ctx.Writeln("")
		_ = ctx.Writeln(fmt.Sprintf("(wrapped) found %q at offset 0x%X", needle, idx))
	}
	s.draw(ctx)
}

// indexCI returns the first index >= start where needle occurs in data,
// comparing ASCII case-insensitively. Byte-exact match for non-ASCII.
func indexCI(data, needle []byte, start int) int {
	n := len(needle)
	if n == 0 || start+n > len(data) {
		return -1
	}
	for i := start; i+n <= len(data); i++ {
		ok := true
		for j := 0; j < n; j++ {
			a := data[i+j]
			b := needle[j]
			if a >= 'A' && a <= 'Z' {
				a += 0x20
			}
			if b >= 'A' && b <= 'Z' {
				b += 0x20
			}
			if a != b {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// isBinary applies a two-rule heuristic: any NUL byte in the leading sample
// marks the file binary (common in executables, archives, images), and
// otherwise we count low-ASCII control bytes excluding TAB/LF/CR. Bytes
// >= 0x80 are allowed through (UTF-8, PETSCII, etc.) so UTF-8 text doesn't
// accidentally default to hex.
func isBinary(data []byte) bool {
	sample := len(data)
	if sample > 4096 {
		sample = 4096
	}
	if sample == 0 {
		return false
	}
	for i := 0; i < sample; i++ {
		if data[i] == 0 {
			return true
		}
	}
	ctrl := 0
	for i := 0; i < sample; i++ {
		b := data[i]
		if b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		if b < 0x20 || b == 0x7f {
			ctrl++
		}
	}
	return ctrl*100/sample > 10
}

package navigator

import (
	"crypto/md5"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/solvalou/xfer/internal/constants"
	"github.com/solvalou/xfer/internal/logger"
	"github.com/solvalou/xfer/internal/session"
)

// Entry is one item in a directory listing: just enough to render the menu
// and resolve a selection without re-stat-ing for every row.
type Entry struct {
	Name  string
	IsDir bool
}

// GetAbsoluteFilePath resolves `name` against ctx.Path, enforcing the
// secure-mode path-traversal guard.
func GetAbsoluteFilePath(ctx *session.Context, name string, cfg *session.Config) (string, error) {
	if name == ".." && cfg != nil && !cfg.SecureMode {
		return filepath.Clean(filepath.Join(ctx.Path, "..")), nil
	}

	resolved := filepath.Clean(filepath.Join(ctx.Path, name))

	if (cfg != nil && cfg.SecureMode) || strings.Contains(name, "../") || strings.Contains(name, `..\`) {
		base, err := filepath.Abs(ctx.Path)
		if err != nil {
			return "", err
		}
		abs, err := filepath.Abs(resolved)
		if err != nil {
			return "", err
		}
		// Trailing-separator guard avoids the /foo vs /foobar prefix bug.
		if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
			return "", errors.New("Path traversal attempt detected")
		}
	}

	return resolved, nil
}

// GetEntries lists visible items in ctx.Path, sorted case-insensitively.
// ".." is prepended when non-root and non-secure. Uses os.ReadDir's DirEntry
// for the directory check so we don't need a separate Lstat per item.
func GetEntries(ctx *session.Context, cfg *session.Config) []Entry {
	dirEntries, err := os.ReadDir(ctx.Path)
	if err != nil {
		logger.Error(fmt.Sprintf("Error reading directory %s: %v", ctx.Path, err))
		return nil
	}
	out := make([]Entry, 0, len(dirEntries))
	for _, de := range dirEntries {
		name := de.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		isDir := entryIsDir(ctx.Path, de)
		if cfg.SecureMode && isDir {
			continue
		}
		out = append(out, Entry{Name: name, IsDir: isDir})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	if !cfg.SecureMode && !isRoot(ctx.Path) {
		return append([]Entry{{Name: "..", IsDir: true}}, out...)
	}
	return out
}

// listItem pairs an entry with its 1-based menu number. The number always
// reflects the entry's position in the unfiltered GetEntries result so
// SelectFile (which re-runs GetEntries) maps a typed number back to the
// same entry whether the listing was filtered or not.
type listItem struct {
	Entry Entry
	Num   int
}

type pagerPhase int

const (
	pagerInactive pagerPhase = iota
	pagerMore                // showing "[M]ore, [S]earch" — waiting for one keystroke
	pagerSearch              // collecting a search term, byte-by-byte
)

// PagerState holds the in-progress listing while the user is being asked
// "[M]ore, [S]earch" or while collecting a search term. Stored on
// ctx.NavState; cleared when the listing finishes (final menu prompt
// shown), so absence implies the listing is fully drawn.
type PagerState struct {
	items []listItem
	next  int
	// firstPageSize is the entry count for the very first page, sized to
	// leave room for the header line + prompt. laterPageSize is for every
	// subsequent page, where the header doesn't repeat — so one more
	// entry fits.
	firstPageSize int
	laterPageSize int
	// totalEntries is the count of entries in the unfiltered directory
	// listing — equal to len(items) for unfiltered listings, and >= it
	// for filtered ones. Used to render the "1-X" range so the user
	// sees the maximum valid number to type even when the filtered list
	// only shows a subset.
	totalEntries int
	filtered     bool
	// fromFinal is set by BeginSearch to flag that the search was
	// triggered from the final menu prompt (no remaining pages); empty
	// Enter cancels back to a fresh ListFiles instead of trying to
	// continue paging.
	fromFinal bool
	searchBuf strings.Builder
	phase     pagerPhase
}

// PagerActive reports whether the navigator is mid-page or collecting a
// search string. The main input loop should route bytes to HandlePagerInput
// in that case instead of treating digits as menu picks.
func PagerActive(ctx *session.Context) bool {
	p, ok := ctx.NavState.(*PagerState)
	return ok && p != nil && p.phase != pagerInactive
}

// ListFiles prints the menu (header + numbered entries + prompt) to the client.
// Entering the listing always discards any previously-buffered transfer body:
// cancelled protocol prompts, finished transfers, and the "empty URL=back"
// path all pass through here, so this is the one place that guarantees a
// pending buffer doesn't leak into the next selection. When the entry
// count exceeds one screenful (per ctx.TermHeight) the listing pauses
// every page at "[M]ore, [S]earch".
func ListFiles(ctx *session.Context, cfg *session.Config) {
	ctx.Mode = session.ModeNavigate
	ctx.RequestedFile = ""
	ctx.RequestedName = ""
	ctx.RequestedBody = nil
	ctx.NavState = nil

	_ = ctx.Writeln(fmt.Sprintf("----- %s -----", ctx.Path))
	all := GetEntries(ctx, cfg)
	startPager(ctx, cfg, buildItems(all), len(all), false)
}

// listFiltered prints a filtered listing — entries from ctx.Path whose
// names contain term (case-insensitively). Numbers shown match the
// unfiltered position so a typed number still resolves correctly via
// SelectFile.
func listFiltered(ctx *session.Context, cfg *session.Config, term string) {
	ctx.Mode = session.ModeNavigate
	ctx.NavState = nil
	_ = ctx.Writeln(fmt.Sprintf("----- \"%s\" in %s -----", term, ctx.Path))

	all := GetEntries(ctx, cfg)
	needle := strings.ToLower(term)
	items := make([]listItem, 0, len(all))
	for i, e := range all {
		if strings.Contains(strings.ToLower(e.Name), needle) {
			items = append(items, listItem{Entry: e, Num: i + 1})
		}
	}
	startPager(ctx, cfg, items, len(all), true)
}

func buildItems(entries []Entry) []listItem {
	out := make([]listItem, len(entries))
	for i, e := range entries {
		out[i] = listItem{Entry: e, Num: i + 1}
	}
	return out
}

// minPageSize keeps pathologically tall-but-narrow terminal sizes from
// driving page size to zero or one — the listing must still make
// progress when the user said e.g. --term-height 2.
const minPageSize = 4

func startPager(ctx *session.Context, cfg *session.Config, items []listItem, totalEntries int, filtered bool) {
	// First page leaves room for header + prompt; later pages have no
	// header to repeat, so one more entry fits.
	firstPage := ctx.TermHeight - 2
	laterPage := ctx.TermHeight - 1
	if firstPage < minPageSize {
		firstPage = minPageSize
	}
	if laterPage < minPageSize {
		laterPage = minPageSize
	}
	p := &PagerState{
		items:         items,
		firstPageSize: firstPage,
		laterPageSize: laterPage,
		totalEntries:  totalEntries,
		filtered:      filtered,
	}
	ctx.NavState = p
	renderNextPage(ctx, cfg, p)
}

// renderNextPage prints the next slice of entries, then either pauses
// with "[M]ore, [S]earch" (more remain) or prints the final menu prompt
// and clears the pager (all entries shown). The first page is one entry
// shorter than later pages because it shares the screen with the listing
// header.
func renderNextPage(ctx *session.Context, cfg *session.Config, p *PagerState) {
	pageSize := p.firstPageSize
	if p.next > 0 {
		pageSize = p.laterPageSize
	}
	end := p.next + pageSize
	if end > len(p.items) {
		end = len(p.items)
	}
	for i := p.next; i < end; i++ {
		it := p.items[i]
		prefix := constants.FilePrefix
		if it.Entry.IsDir {
			prefix = constants.DirectoryPrefix
		}
		_ = ctx.Writeln(fmt.Sprintf("%d %s %s", it.Num, prefix, it.Entry.Name))
	}
	p.next = end
	if p.next < len(p.items) {
		p.phase = pagerMore
		_ = ctx.Write("[M]ore, [S]earch: ")
		return
	}
	finalPrompt(ctx, cfg, p)
}

func finalPrompt(ctx *session.Context, cfg *session.Config, p *PagerState) {
	p.phase = pagerInactive
	ctx.NavState = nil
	urlHint := ""
	if !cfg.NoURL {
		urlHint = "[U]RL, "
	}
	var head string
	switch {
	case len(p.items) == 0:
		head = "(no entries) "
	default:
		// totalEntries reflects the directory's full count, so a filtered
		// listing still tells the user the maximum valid number to type
		// (the displayed numbers preserve original positions).
		head = fmt.Sprintf("1-%d, ", p.totalEntries)
	}
	_ = ctx.Write(fmt.Sprintf("%s%s[S]earch, [R]efresh, e[X]it: ", head, urlHint))
}

// BeginSearch starts a search-term collection from the final menu prompt.
// Reuses the pager's search-byte handling so backspace/echo/Enter behave
// identically whether triggered mid-listing ([M]ore prompt) or after the
// listing has fully drawn. fromFinal=true tells handleSearchByte that an
// empty Enter cancels back to a fresh listing rather than continuing
// pagination.
func BeginSearch(ctx *session.Context, cfg *session.Config) {
	p := &PagerState{phase: pagerSearch, fromFinal: true}
	ctx.NavState = p
	_ = ctx.Writeln("")
	_ = ctx.Write("Search (empty=back): ")
}

// HandlePagerInput consumes bytes while the listing is paused at a
// "[M]ore, [S]earch" prompt or collecting a search term. The main input
// loop calls this whenever PagerActive(ctx) is true so that digits/letters
// destined for the pager don't get misinterpreted as menu picks.
func HandlePagerInput(ctx *session.Context, cfg *session.Config, data []byte) {
	p, ok := ctx.NavState.(*PagerState)
	if !ok || p == nil {
		return
	}
	for _, b := range data {
		switch p.phase {
		case pagerMore:
			handleMoreByte(ctx, cfg, p, b)
		case pagerSearch:
			handleSearchByte(ctx, cfg, p, b)
		default:
			return
		}
		// handleSearchByte may swap in a new pager (filtered listing) or
		// finish the listing entirely. Stop iterating bytes the moment
		// the current pager is no longer the active state, so we don't
		// touch a stale struct or feed input meant for the next prompt.
		if cur, _ := ctx.NavState.(*PagerState); cur != p {
			return
		}
	}
}

func handleMoreByte(ctx *session.Context, cfg *session.Config, p *PagerState, b byte) {
	switch b {
	case 'M', 'm':
		_, _ = ctx.Conn.Write([]byte{b, '\r', '\n'})
		renderNextPage(ctx, cfg, p)
	case 'S', 's':
		_, _ = ctx.Conn.Write([]byte{b, '\r', '\n'})
		p.phase = pagerSearch
		p.searchBuf.Reset()
		_ = ctx.Write("Search (empty=continue): ")
	}
	// Anything else: ignore. Old terminals send stray escape bytes,
	// XON/XOFF, etc., that shouldn't move the listing forward.
}

func handleSearchByte(ctx *session.Context, cfg *session.Config, p *PagerState, b byte) {
	if b == '\r' || b == '\n' {
		term := strings.TrimSpace(p.searchBuf.String())
		p.searchBuf.Reset()
		_ = ctx.Writeln("")
		if term == "" {
			if p.fromFinal {
				// Search came from the final menu prompt — empty input
				// cancels back to a fresh listing of the current path.
				ListFiles(ctx, cfg)
				return
			}
			renderNextPage(ctx, cfg, p)
			return
		}
		listFiltered(ctx, cfg, term)
		return
	}
	if (b == '\b' || b == 0x7f) && p.searchBuf.Len() > 0 {
		s := p.searchBuf.String()
		p.searchBuf.Reset()
		p.searchBuf.WriteString(s[:len(s)-1])
		_, _ = ctx.Conn.Write([]byte{'\b', ' ', '\b'})
		return
	}
	if b >= 0x20 && b < 0x7f {
		p.searchBuf.WriteByte(b)
		_, _ = ctx.Conn.Write([]byte{b})
	}
}

// AnnounceBuffered prints the "Ready to download / Size / MD5" banner once
// a transferable body has been staged in ctx.RequestedBody (either by a
// local pick or a URL download). Shown before the protocol prompt so the
// user can pick a protocol with size in mind.
func AnnounceBuffered(ctx *session.Context) {
	_ = ctx.Writeln(fmt.Sprintf("Ready to download %s", ctx.RequestedName))
	_ = ctx.Writeln(fmt.Sprintf("Size: %d bytes", len(ctx.RequestedBody)))
	_ = ctx.Writeln(fmt.Sprintf("MD5:  %x", md5.Sum(ctx.RequestedBody)))
}

// SelectFile handles the user's numeric choice. Directory: navigate and
// re-list. File: read into memory, stage it on the context, print the
// size/MD5 banner, then hand off to onSelected (which shows the protocol
// prompt). Reading at selection time means transferPrelude never has to
// touch the disk and the user sees the file size before being asked which
// protocol to use — useful when picking XMODEM (small) vs ZMODEM (large).
func SelectFile(ctx *session.Context, n int, cfg *session.Config, onSelected func(*session.Context)) {
	entries := GetEntries(ctx, cfg)
	if n < 1 || n > len(entries) {
		_ = ctx.Writeln(fmt.Sprintf("Invalid selection. Enter a number between 1-%d.", len(entries)))
		ListFiles(ctx, cfg)
		return
	}
	sel := entries[n-1]
	abs, err := GetAbsoluteFilePath(ctx, sel.Name, cfg)
	if err != nil {
		logger.Error(err.Error())
		return
	}
	if sel.IsDir {
		ctx.Path = abs
		logger.Debug(fmt.Sprintf("Navigated to %s", ctx.Path))
		ListFiles(ctx, cfg)
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		logger.Error(fmt.Sprintf("read %s: %v", abs, err))
		_ = ctx.Writeln(fmt.Sprintf("Error reading file: %v", err))
		ListFiles(ctx, cfg)
		return
	}
	ctx.Mode = session.ModeConfirmTransfer
	ctx.RequestedFile = abs
	ctx.RequestedName = sel.Name
	ctx.RequestedBody = data
	AnnounceBuffered(ctx)
	if onSelected != nil {
		onSelected(ctx)
	}
}

func isRoot(path string) bool { return filepath.Dir(path) == path }

// entryIsDir resolves DirEntry.IsDir(), chasing a single level of symlink
// indirection if needed. DirEntry.IsDir reports false for symlinks even if
// they point at directories, which matters for menu rendering.
func entryIsDir(basePath string, de os.DirEntry) bool {
	if de.IsDir() {
		return true
	}
	if de.Type()&os.ModeSymlink == 0 {
		return false
	}
	target, err := filepath.EvalSymlinks(filepath.Join(basePath, de.Name()))
	if err != nil {
		return false
	}
	info, err := os.Stat(target)
	if err != nil {
		return false
	}
	return info.IsDir()
}

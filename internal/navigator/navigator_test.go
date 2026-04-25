package navigator

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solvalou/xfer/internal/session"
)

// newCtx builds a minimal Context rooted at dir. No net.Conn — these tests
// never write to a client, they exercise pure path/listing logic.
func newCtx(dir string) *session.Context {
	return &session.Context{Path: dir}
}

// newConnCtx wires a Context to one end of a net.Pipe and starts a
// background reader on the other end so writes from the navigator don't
// block. Callers can read everything captured via captured.Bytes() and
// inject input bytes by writing to the returned in() helper.
type captured struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *captured) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

func (c *captured) String() string { return string(c.Bytes()) }

func newConnCtx(t *testing.T, dir string, termWidth, termHeight int) (ctx *session.Context, cap *captured, in func([]byte), cleanup func()) {
	t.Helper()
	server, client := net.Pipe()
	cap = &captured{}
	doneR := make(chan struct{})
	go func() {
		defer close(doneR)
		tmp := make([]byte, 256)
		for {
			n, err := client.Read(tmp)
			if n > 0 {
				cap.mu.Lock()
				cap.buf.Write(tmp[:n])
				cap.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	ctx = &session.Context{
		Path:       dir,
		Conn:       server,
		TermWidth:  termWidth,
		TermHeight: termHeight,
	}
	in = func(b []byte) {
		// Writes have to be drained quickly because net.Pipe is synchronous.
		go func() { _, _ = client.Write(b) }()
	}
	cleanup = func() {
		_ = server.Close()
		_ = client.Close()
		<-doneR
	}
	return
}

// waitFor polls cap until predicate is true or the deadline fires.
func waitFor(t *testing.T, cap *captured, predicate func([]byte) bool, why string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if predicate(cap.Bytes()) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waitFor(%s) timed out; got: %q", why, cap.String())
}

// -------------------------------------------------------------
// GetAbsoluteFilePath — path traversal guard
// -------------------------------------------------------------

func TestGetAbsoluteFilePath_PlainFile(t *testing.T) {
	base := t.TempDir()
	ctx := newCtx(base)
	cfg := &session.Config{}

	got, err := GetAbsoluteFilePath(ctx, "hello.txt", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(base, "hello.txt")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestGetAbsoluteFilePath_DotDotAllowedWhenNotSecure(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "child")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := newCtx(sub)
	cfg := &session.Config{SecureMode: false}

	got, err := GetAbsoluteFilePath(ctx, "..", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != filepath.Clean(base) {
		t.Fatalf("got %q, want %q", got, filepath.Clean(base))
	}
}

func TestGetAbsoluteFilePath_DotDotBlockedInSecureMode(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "child")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := newCtx(sub)
	cfg := &session.Config{SecureMode: true}

	_, err := GetAbsoluteFilePath(ctx, "..", cfg)
	if err == nil {
		t.Fatal("expected path traversal error, got nil")
	}
	if !strings.Contains(err.Error(), "Path traversal") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestGetAbsoluteFilePath_ForwardSlashTraversalBlockedEverywhere(t *testing.T) {
	base := t.TempDir()
	for _, secure := range []bool{false, true} {
		t.Run("", func(t *testing.T) {
			ctx := newCtx(base)
			cfg := &session.Config{SecureMode: secure}
			_, err := GetAbsoluteFilePath(ctx, "../etc/passwd", cfg)
			if err == nil {
				t.Fatalf("secure=%v: expected error for ../etc/passwd, got nil", secure)
			}
		})
	}
}

func TestGetAbsoluteFilePath_BackslashTraversalBlockedOnWindows(t *testing.T) {
	// On Windows filepath treats `\` as a separator so `..\boot.ini`
	// actually escapes the base dir; the abs/base prefix check then
	// rejects it. On POSIX `\` is a regular filename byte, so the same
	// input resolves to a file literally named "..\boot.ini" inside
	// base — not a traversal, and not an error.
	if runtime.GOOS != "windows" {
		t.Skip("`..\\` is only a separator on Windows")
	}
	base := t.TempDir()
	ctx := newCtx(base)
	cfg := &session.Config{}
	_, err := GetAbsoluteFilePath(ctx, `..\boot.ini`, cfg)
	if err == nil {
		t.Fatal("expected backslash traversal to be blocked on Windows")
	}
}

func TestGetAbsoluteFilePath_SiblingDirWithSharedPrefix(t *testing.T) {
	// Prefix check must use filepath.Separator, not bare string prefix —
	// otherwise /foobar would be accepted as a child of /foo.
	parent := t.TempDir()
	foo := filepath.Join(parent, "foo")
	foobar := filepath.Join(parent, "foobar", "secret")
	if err := os.MkdirAll(foo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(foobar, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := newCtx(foo)
	cfg := &session.Config{SecureMode: true}

	// "../foobar/secret" contains "../" so the guard runs; the resolved
	// absolute path must not be accepted as descending from foo.
	_, err := GetAbsoluteFilePath(ctx, "../foobar/secret", cfg)
	if err == nil {
		t.Fatal("expected traversal error, got nil (sibling-prefix bug)")
	}
}

// -------------------------------------------------------------
// GetEntries — listing behaviour
// -------------------------------------------------------------

func TestGetEntries_FiltersDotfiles(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"visible.txt", ".hidden", ".git"} {
		if err := os.WriteFile(filepath.Join(base, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries := GetEntries(newCtx(base), &session.Config{})
	for _, e := range entries {
		if strings.HasPrefix(e.Name, ".") && e.Name != ".." {
			t.Fatalf("dotfile leaked into listing: %q", e.Name)
		}
	}
}

func TestGetEntries_CaseInsensitiveSort(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"Zebra.txt", "apple.txt", "Banana.txt"} {
		if err := os.WriteFile(filepath.Join(base, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries := GetEntries(newCtx(base), &session.Config{SecureMode: true})
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	want := []string{"apple.txt", "Banana.txt", "Zebra.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("case-insensitive sort wrong:\n got %v\nwant %v", names, want)
	}
}

func TestGetEntries_SecureModeExcludesDirsAndParent(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "file.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	entries := GetEntries(newCtx(base), &session.Config{SecureMode: true})
	for _, e := range entries {
		if e.IsDir {
			t.Fatalf("secure mode should drop directories, got %q", e.Name)
		}
		if e.Name == ".." {
			t.Fatalf("secure mode should drop parent-dir entry")
		}
	}
	if len(entries) != 1 || entries[0].Name != "file.txt" {
		t.Fatalf("expected [file.txt], got %v", entries)
	}
}

func TestGetEntries_NonSecurePrependsParent(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "here")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "z.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	entries := GetEntries(newCtx(sub), &session.Config{})
	if len(entries) == 0 || entries[0].Name != ".." {
		t.Fatalf("expected .. first, got %v", entries)
	}
}

func TestGetEntries_SymlinkToDirReportedAsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, "realdir")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "aalink") // sorts before "realdir"
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	entries := GetEntries(newCtx(base), &session.Config{})
	var linkEntry *Entry
	for i := range entries {
		if entries[i].Name == "aalink" {
			linkEntry = &entries[i]
		}
	}
	if linkEntry == nil {
		t.Fatal("symlink entry missing from listing")
	}
	if !linkEntry.IsDir {
		t.Fatal("symlink pointing at a directory should be classified as IsDir")
	}
}

// -------------------------------------------------------------
// ListFiles — pagination & search
// -------------------------------------------------------------

// makeFiles creates count empty files named file%03d.txt under dir, plus
// any extra named files passed in.
func makeFiles(t *testing.T, dir string, count int, extra ...string) {
	t.Helper()
	for i := 1; i <= count; i++ {
		name := fmt.Sprintf("file%03d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range extra {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListFiles_NoPaginationWhenAllFit(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, 3)
	ctx, cap, _, cleanup := newConnCtx(t, dir, 80, 24)
	defer cleanup()

	ListFiles(ctx, &session.Config{SecureMode: true})
	// Wait until the final prompt appears.
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("1-")) }, "final prompt")

	got := cap.String()
	if strings.Contains(got, "[M]ore") {
		t.Fatalf("did not expect a more-prompt for a 3-file listing on a 24-line terminal; got:\n%s", got)
	}
	if PagerActive(ctx) {
		t.Fatalf("pager should be inactive after a single-page listing")
	}
}

func TestListFiles_PaginatesAndContinuesOnMore(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, 30) // 30 files, secure mode (no ".." prepended)
	cfg := &session.Config{SecureMode: true}
	ctx, cap, _, cleanup := newConnCtx(t, dir, 80, 10) // pageSize = 10-2 = 8
	defer cleanup()

	ListFiles(ctx, cfg)
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("[M]ore, [S]earch")) }, "first more-prompt")

	first := cap.String()
	// Page 1: file001..file008 should appear. file009 should NOT yet.
	if !strings.Contains(first, "file001.txt") {
		t.Fatalf("page 1 missing first entry; got:\n%s", first)
	}
	if !strings.Contains(first, "file008.txt") {
		t.Fatalf("page 1 missing 8th entry; got:\n%s", first)
	}
	if strings.Contains(first, "file009.txt") {
		t.Fatalf("page 1 leaked into page 2; got:\n%s", first)
	}
	if !PagerActive(ctx) {
		t.Fatalf("pager should be active at more-prompt")
	}

	// Press M — next page should render and we should see another prompt.
	HandlePagerInput(ctx, cfg, []byte("M"))
	waitFor(t, cap, func(b []byte) bool {
		// Wait until file009 has appeared in the cumulative output.
		return bytes.Contains(b, []byte("file009.txt"))
	}, "page 2")
	waitFor(t, cap, func(b []byte) bool {
		// Two M-prompts seen total (one per finished page).
		return bytes.Count(b, []byte("[M]ore, [S]earch")) >= 2
	}, "second more-prompt")

	// Continue M-ing until the listing finishes.
	for i := 0; i < 10 && PagerActive(ctx); i++ {
		HandlePagerInput(ctx, cfg, []byte("M"))
	}
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("1-30")) }, "final prompt")

	got := cap.String()
	if !strings.Contains(got, "file030.txt") {
		t.Fatalf("last entry missing; got:\n%s", got)
	}
	if PagerActive(ctx) {
		t.Fatalf("pager should be cleared after final prompt")
	}
}

func TestListFiles_SearchFiltersAndPreservesOriginalNumbers(t *testing.T) {
	dir := t.TempDir()
	// apple.txt, banana.txt, cherry.txt, grape.txt — sorted positions 1..4
	makeFiles(t, dir, 0, "apple.txt", "banana.txt", "cherry.txt", "grape.txt")
	cfg := &session.Config{SecureMode: true}
	// Tiny screen (10 rows, pageSize=8) so 4 entries still fit on one page.
	ctx, cap, _, cleanup := newConnCtx(t, dir, 80, 10)
	defer cleanup()

	ListFiles(ctx, cfg)
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("1-4")) }, "initial final prompt")

	// Listing isn't paused so HandlePagerInput won't fire — but we want
	// to test the search path that fires from within a page break. Use
	// a smaller terminal to force pagination.
	cleanup()

	ctx, cap, _, cleanup = newConnCtx(t, dir, 80, 6) // pageSize = 4
	defer cleanup()
	ListFiles(ctx, cfg)
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("1-4")) }, "fits on one page even with pageSize=4")

	// 4 entries fit on a 4-entry page so there is no more-prompt; we
	// have to test search when entries > pageSize. Add more files.
	for _, n := range []string{"date.txt", "elderberry.txt", "fig.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cleanup()
	ctx, cap, _, cleanup = newConnCtx(t, dir, 80, 6) // pageSize = 4
	defer cleanup()
	ListFiles(ctx, cfg)
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("[M]ore, [S]earch")) }, "more-prompt with 7 files")

	// Press S — should ask for search term.
	HandlePagerInput(ctx, cfg, []byte("S"))
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("Search (empty=continue)")) }, "search prompt")

	// Type "rry" then Enter — should match cherry.txt (#3) and elderberry.txt (#5).
	HandlePagerInput(ctx, cfg, []byte("rry\r"))
	waitFor(t, cap, func(b []byte) bool {
		// Filtered listing emits "1-7" because the unfiltered directory
		// has 7 entries — the displayed numbers are scattered (3, 5) but
		// the user must still see the maximum valid number to type.
		return bytes.Contains(b, []byte("\"rry\" in")) && bytes.Contains(b, []byte("1-7,"))
	}, "filtered listing")

	got := cap.String()
	// Filtered listing should contain BOTH matched entries; their
	// rendered line should start with the ORIGINAL number (3 and 5
	// after case-insensitive sort: apple, banana, cherry, date,
	// elderberry, fig, grape) so the user can type that number.
	idx := strings.Index(got, "\"rry\" in")
	if idx < 0 {
		t.Fatalf("filter header missing; got:\n%s", got)
	}
	tailLines := strings.Split(got[idx:], "\n")
	wantLines := map[string]bool{"cherry.txt": false, "elderberry.txt": false}
	wantNums := map[string]string{"cherry.txt": "3", "elderberry.txt": "5"}
	for _, line := range tailLines {
		for name := range wantLines {
			if strings.Contains(line, name) {
				wantLines[name] = true
				if !strings.HasPrefix(strings.TrimSpace(line), wantNums[name]+" ") {
					t.Fatalf("filtered line for %q lost its original number %s: %q", name, wantNums[name], line)
				}
			}
		}
	}
	for name, found := range wantLines {
		if !found {
			t.Fatalf("filtered listing missing %q; got:\n%s", name, got)
		}
	}
	// Non-matches must be absent from the filtered section. Slice off
	// everything before the filter header to avoid the original page.
	tail := got[idx:]
	for _, badName := range []string{"apple.txt", "banana.txt", "date.txt", "fig.txt", "grape.txt"} {
		if strings.Contains(tail, badName) {
			t.Fatalf("filter leaked non-match %q; tail:\n%s", badName, tail)
		}
	}
}

func TestListFiles_EmptySearchContinuesPaging(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, 12)
	cfg := &session.Config{SecureMode: true}
	ctx, cap, _, cleanup := newConnCtx(t, dir, 80, 6) // pageSize = 4
	defer cleanup()

	ListFiles(ctx, cfg)
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("[M]ore, [S]earch")) }, "first more-prompt")

	// S then immediate Enter — empty term means "continue".
	HandlePagerInput(ctx, cfg, []byte("S"))
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("Search (empty=continue)")) }, "search prompt")
	HandlePagerInput(ctx, cfg, []byte("\r"))

	// After Enter on empty term, the next page should render — file005..file008 appear.
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("file005.txt")) }, "second page after empty search")
	if !PagerActive(ctx) {
		t.Fatalf("pager should still be active mid-listing")
	}
}

func TestListFiles_SearchCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, 0, "Alpha.TXT", "beta.txt", "GAMMA.txt")
	cfg := &session.Config{SecureMode: true}
	// Three files fit, but force a search by adding more.
	makeFiles(t, dir, 5)
	ctx, cap, _, cleanup := newConnCtx(t, dir, 80, 6) // pageSize=4, 8 entries → paginated
	defer cleanup()

	ListFiles(ctx, cfg)
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("[M]ore, [S]earch")) }, "more-prompt")

	HandlePagerInput(ctx, cfg, []byte("S"))
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("Search (empty=continue)")) }, "search prompt")
	// Uppercase needle, mixed-case haystack — must still match.
	HandlePagerInput(ctx, cfg, []byte("ALPHA\r"))
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("Alpha.TXT")) }, "case-insensitive match")
}

func TestListFiles_SearchNoMatches(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, 12) // forces pagination on an 8-line terminal
	cfg := &session.Config{SecureMode: true}
	ctx, cap, _, cleanup := newConnCtx(t, dir, 80, 6)
	defer cleanup()

	ListFiles(ctx, cfg)
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("[M]ore, [S]earch")) }, "more-prompt")

	HandlePagerInput(ctx, cfg, []byte("S"))
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("Search (empty=continue)")) }, "search prompt")
	HandlePagerInput(ctx, cfg, []byte("nopenope\r"))
	waitFor(t, cap, func(b []byte) bool {
		return bytes.Contains(b, []byte("\"nopenope\" in")) && bytes.Contains(b, []byte("(no entries)"))
	}, "empty filtered listing")

	if PagerActive(ctx) {
		t.Fatalf("pager should be inactive on a zero-result listing")
	}
}

// Improvement #3: pages after the first don't repeat the directory
// header, so the laterPageSize is one more than firstPageSize. This
// test pins down the exact page boundaries so a future reader can see
// the off-by-one fix is intentional.
func TestListFiles_LaterPagesFitOneMoreEntry(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, 25)
	cfg := &session.Config{SecureMode: true}
	// h=10 → first page 8 (10-2), later pages 9 (10-1). 25 entries split
	// as 8 + 9 + 8 over three pages.
	ctx, cap, _, cleanup := newConnCtx(t, dir, 80, 10)
	defer cleanup()

	ListFiles(ctx, cfg)
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("[M]ore, [S]earch")) }, "first more-prompt")

	first := cap.String()
	if !strings.Contains(first, "file008.txt") {
		t.Fatalf("first page must include file008.txt (8 entries); got:\n%s", first)
	}
	if strings.Contains(first, "file009.txt") {
		t.Fatalf("first page must NOT include file009.txt (would mean >8 entries); got:\n%s", first)
	}

	HandlePagerInput(ctx, cfg, []byte("M"))
	waitFor(t, cap, func(b []byte) bool {
		// Page 2 must include file017.txt (8 + 9 = 17).
		return bytes.Contains(b, []byte("file017.txt"))
	}, "second page reaches file017")
	waitFor(t, cap, func(b []byte) bool {
		return bytes.Count(b, []byte("[M]ore, [S]earch")) >= 2
	}, "second more-prompt")

	// Page 2 must NOT yet include file018 — that belongs to page 3.
	if strings.Contains(cap.String(), "file018.txt") {
		t.Fatalf("second page leaked into the third (should hold 9 entries: file009..file017); got:\n%s", cap.String())
	}
}

func TestBeginSearch_FromFinalPromptFiltersAndCancels(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir, 0, "alpha.txt", "beta.txt", "gamma.txt")
	cfg := &session.Config{SecureMode: true}
	ctx, cap, _, cleanup := newConnCtx(t, dir, 80, 24) // all fit, no pagination
	defer cleanup()

	ListFiles(ctx, cfg)
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("[S]earch, [R]efresh")) }, "final menu prompt")

	// Trigger search via BeginSearch (what handleNavigateInput in main.go
	// calls when the user presses S at the final prompt).
	BeginSearch(ctx, cfg)
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("Search (empty=back)")) }, "search prompt from final")

	// Type "alp" Enter — should land in a filtered listing.
	HandlePagerInput(ctx, cfg, []byte("alp\r"))
	waitFor(t, cap, func(b []byte) bool {
		return bytes.Contains(b, []byte("\"alp\" in")) && bytes.Contains(b, []byte("alpha.txt"))
	}, "filtered result")
	if PagerActive(ctx) {
		t.Fatalf("pager should be inactive after a single-page filtered result")
	}

	// Now exercise the empty-cancels branch: search again, press Enter
	// immediately, expect the unfiltered listing to come back.
	BeginSearch(ctx, cfg)
	waitFor(t, cap, func(b []byte) bool {
		return bytes.Count(b, []byte("Search (empty=back)")) >= 2
	}, "second search prompt")
	HandlePagerInput(ctx, cfg, []byte("\r"))
	waitFor(t, cap, func(b []byte) bool {
		// Header for the unfiltered path appears, plus a fresh menu prompt.
		return bytes.Count(b, []byte("----- "+dir)) >= 2 && bytes.Contains(b, []byte("1-3"))
	}, "back to unfiltered listing")
}

func TestListFiles_PageSizeFloor(t *testing.T) {
	// TermHeight=2 would naively give pageSize=0 — must be clamped so a
	// pathologically small terminal still makes progress one page at a time.
	dir := t.TempDir()
	makeFiles(t, dir, 6)
	cfg := &session.Config{SecureMode: true}
	ctx, cap, _, cleanup := newConnCtx(t, dir, 40, 2)
	defer cleanup()

	ListFiles(ctx, cfg)
	waitFor(t, cap, func(b []byte) bool { return bytes.Contains(b, []byte("[M]ore, [S]earch")) }, "more-prompt with tiny terminal")

	// Just confirm at least one entry rendered and the pager paused —
	// the exact floor (4) is implementation detail.
	got := cap.String()
	if !strings.Contains(got, "file001.txt") {
		t.Fatalf("first entry missing on tiny terminal; got:\n%s", got)
	}
}

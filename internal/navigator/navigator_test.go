package navigator

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/solvalou/xfer/internal/session"
)

// newCtx builds a minimal Context rooted at dir. No net.Conn — these tests
// never write to a client, they exercise pure path/listing logic.
func newCtx(dir string) *session.Context {
	return &session.Context{Path: dir}
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

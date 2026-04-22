package navigator

import (
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

// ListFiles prints the menu (header + numbered entries + prompt) to the client.
func ListFiles(ctx *session.Context, cfg *session.Config) {
	_ = ctx.Writeln(fmt.Sprintf("----- %s -----", ctx.Path))

	ctx.Mode = session.ModeNavigate
	entries := GetEntries(ctx, cfg)
	for i, e := range entries {
		prefix := constants.FilePrefix
		if e.IsDir {
			prefix = constants.DirectoryPrefix
		}
		_ = ctx.Writeln(fmt.Sprintf("%d %s %s", i+1, prefix, e.Name))
	}
	_ = ctx.Write(fmt.Sprintf("Enter 1-%d, R=refresh, X=exit: ", len(entries)))
}

// SelectFile handles the user's numeric choice. If it refers to a directory,
// ctx.Path is updated and the menu is re-listed. If it refers to a file,
// ctx.Mode advances to ConfirmTransfer and onSelected is invoked.
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
	ctx.Mode = session.ModeConfirmTransfer
	ctx.RequestedFile = abs
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

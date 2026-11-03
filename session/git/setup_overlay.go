package git

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The setup overlay is the landing half of setup_commands (#6667). The
// daemon-side hook runs against the project checkout BEFORE the worktree
// exists, so anything it wants inside the session tree it stages into a
// payload directory (AF_SETUP_DEST). Once `git worktree add` has placed the
// tree, applySetupOverlay copies the payload in — before post_worktree_commands
// run — so hooks and the agent both see the files the script prepared.
//
// Payload entries merge into existing worktree directories but never overwrite
// an existing file: a staged file that collides with a checked-in path is a
// recorded failure, not a silent clobber of repository content. The copy is
// deliberately simple, but the premise is NOT "the staging dir is trusted
// scratch space": for docker sessions the handoff leaves the payload owned by
// the session's container user — writable by any code that user runs — and a
// local payload sits where anything with the operator's uid could have
// written. The safety lives in the copy itself: it never overwrites, it
// refuses to merge through a non-directory destination, and it reproduces
// symlinks rather than following them — no read ever leaves the staging dir
// through a link the payload planted.
func (g *GitWorktree) applySetupOverlay() {
	dir := strings.TrimSpace(g.setupOverlayDir)
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			g.setupOverlayErr = fmt.Sprintf("setup payload %s is unreadable: %v", dir, err)
		}
		return
	}
	for _, entry := range entries {
		if err := copyOverlayEntry(dir, g.worktreePath, entry); err != nil {
			g.setupOverlayErr = fmt.Sprintf("setup payload %s: %v", dir, err)
			return
		}
	}
}

// copyOverlayEntry copies one top-level payload entry — a directory tree, a
// regular file, or a symlink — into the worktree root.
func copyOverlayEntry(srcDir, destDir string, entry fs.DirEntry) error {
	src := filepath.Join(srcDir, entry.Name())
	dest := filepath.Join(destDir, entry.Name())
	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	switch {
	case info.IsDir():
		// A merge destination that already exists must be a REAL directory:
		// a symlink there (checked into the branch, pointing anywhere on the
		// host) resolves outside the worktree, and Mkdir's ErrExist would let
		// the copy descend through it and write outside the session checkout.
		if st, err := os.Lstat(dest); err == nil {
			if !st.IsDir() {
				return fmt.Errorf("%s already exists in the worktree and is not a directory; a setup payload directory must not replace checked-in content", dest)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", dest, err)
		} else if err := os.Mkdir(dest, info.Mode().Perm()); err != nil {
			return fmt.Errorf("create directory %s: %w", dest, err)
		}
		sub, err := os.ReadDir(src)
		if err != nil {
			return fmt.Errorf("read directory %s: %w", src, err)
		}
		for _, e := range sub {
			if err := copyOverlayEntry(src, dest, e); err != nil {
				return err
			}
		}
		return nil
	case info.Mode().IsRegular():
		return copyOverlayFile(src, dest, info.Mode().Perm())
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("read symlink %s: %w", src, err)
		}
		if err := os.Symlink(target, dest); err != nil {
			return fmt.Errorf("create symlink %s: %w", dest, err)
		}
		return nil
	default:
		return fmt.Errorf("%s: unsupported file type %s", src, info.Mode().Type())
	}
}

// copyOverlayFile writes a payload file exclusively: an existing destination
// is a collision the operator should see, not content af silently replaces.
func copyOverlayFile(src, dest string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists in the worktree; a setup payload file must not replace checked-in content", dest)
		}
		return fmt.Errorf("create %s: %w", dest, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("write %s: %w", dest, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}
	return nil
}

// SetSetupOverlayDir names the payload directory applySetupOverlay copies into
// the freshly created worktree. Empty disables the overlay — the common case,
// since most sessions have no setup_commands and stage nothing.
func (g *GitWorktree) SetSetupOverlayDir(dir string) {
	g.setupOverlayDir = dir
}

// SetupOverlayError reports why the payload copy failed, or "" when it
// applied cleanly (or no payload existed). The instance reads this after the
// worktree create and records it on the row — an overlay failure is
// non-blocking for the same reason the hook failure is.
func (g *GitWorktree) SetupOverlayError() string {
	return g.setupOverlayErr
}

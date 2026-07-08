package git

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sachiniyer/agent-factory/log"
)

// SiblingWorktreePath returns the standard sibling worktree location for a
// session in the repo at repoPath — {repoParent}/{repoName}-{safeTitle} — with a
// numeric suffix appended if that path is already occupied. It mirrors the
// layout NewGitWorktree uses when creating a worktree, so RestoreWorktreeTo can
// move an archived worktree back to where a fresh session's worktree would live
// (#1028). The title is sanitized for filesystem safety and the result is
// validated to sit strictly inside the worktree parent dir (the #461 guard).
func SiblingWorktreePath(repoPath, title string) (string, error) {
	repoRoot, err := findGitRepoRoot(repoPath)
	if err != nil {
		return "", err
	}
	worktreeDir := filepath.Dir(repoRoot)
	repoName := filepath.Base(repoRoot)

	// Sanitize the title into a single safe path segment, matching the
	// safeSessionName handling in NewGitWorktree.
	safe := strings.ReplaceAll(title, "..", "")
	safe = strings.ReplaceAll(safe, "/", "-")
	safe = strings.TrimLeft(safe, "-.")
	if safe == "" {
		safe = "session"
	}

	base := filepath.Join(worktreeDir, repoName+"-"+safe)
	absBase, _ := filepath.Abs(base)
	absDir, _ := filepath.Abs(worktreeDir)
	if !isPathStrictlyInside(absBase, absDir) {
		return "", fmt.Errorf("invalid session title %q: would place worktree outside %s", title, worktreeDir)
	}

	p := base
	for i := 2; ; i++ {
		if _, statErr := os.Stat(p); os.IsNotExist(statErr) {
			break
		} else if statErr != nil {
			return "", fmt.Errorf("cannot check worktree path %q: %w", p, statErr)
		}
		p = fmt.Sprintf("%s-%d", base, i)
	}
	return p, nil
}

// ErrRepoGone is returned by RestoreWorktreeTo when the origin repository this
// worktree is registered against no longer exists (deleted, unmounted, or no
// longer a git repository). A worktree cannot be re-registered without its
// repo, so restore surfaces this as an actionable error and leaves the archived
// worktree intact for the user to salvage manually (#1028).
var ErrRepoGone = errors.New("origin repository is gone")

// worktreeMoveFast is the git-native fast path for relocating a worktree —
// `git worktree move`, which is atomic on a single filesystem and updates the
// two-way registration itself. It is a package var so tests can force the
// manual-move + `git worktree repair` fallback deterministically without a real
// second filesystem. Production never reassigns it.
var worktreeMoveFast = func(g *GitWorktree, src, dest string) error {
	_, err := g.runGitCommand(g.repoPath, "worktree", "move", src, dest)
	return err
}

// worktreeRepair re-links a manually moved worktree's two-way registration
// (`git worktree repair`). A package var for the same test-seam reason as
// worktreeMoveFast: it lets a test force a repair failure AFTER a successful
// byte-move to prove the location is still committed. Production never
// reassigns it.
var worktreeRepair = func(g *GitWorktree, dest string) error {
	_, err := g.runGitCommand(g.repoPath, "worktree", "repair", dest)
	return err
}

// worktreeRepairSubmodules re-points initialized submodules after a raw
// directory move. `git worktree repair` fixes the superproject's .git pointer,
// but submodule .git files can still contain relative gitdir paths computed
// from the old worktree location. `git submodule absorbgitdirs` rewrites those
// pointers without fetching or checking out new content.
var worktreeRepairSubmodules = func(g *GitWorktree, dest string) error {
	_, err := g.runGitCommand(dest, "submodule", "absorbgitdirs")
	return err
}

// MoveWorktree relocates this worktree's directory to dest and keeps git's
// two-way worktree link consistent (the worktree's `.git` file and the repo's
// `.git/worktrees/<name>/gitdir`). It is the archive-side primitive (#1028):
// the caller has already torn down every tmux session over the worktree, so the
// directory is free to move.
//
// Uncommitted changes and the branch are preserved by construction — the
// working directory is moved verbatim, never re-checked-out. On success
// g.worktreePath / g.worktreeDir are updated to point at dest.
func (g *GitWorktree) MoveWorktree(dest string) error {
	return g.relocateWorktreeTo(dest)
}

// RestoreWorktreeTo moves this (archived) worktree back to dest and re-registers
// it against the origin repo — the restore-side primitive (#1028). It first
// verifies the origin repo still exists (ErrRepoGone otherwise), because a
// worktree cannot be repaired/re-registered without its repository; the repair
// runs against wherever the repo now lives, so a repo that itself moved on disk
// since archiving is handled.
func (g *GitWorktree) RestoreWorktreeTo(dest string) error {
	if err := g.ensureRepoPresent(); err != nil {
		return err
	}
	return g.relocateWorktreeTo(dest)
}

// relocateWorktreeTo is the shared move engine behind MoveWorktree and
// RestoreWorktreeTo. Fast path: `git worktree move`. Because the archive root
// ($AF_HOME) is frequently on a different filesystem than the repo, that rename
// can fail with EXDEV; on ANY fast-path failure we fall back to moving the
// directory bytes ourselves (rename, or copy+remove across devices) and running
// `git worktree repair`, which is purpose-built to fix a manually moved
// worktree. `git worktree move` validates and renames before touching its
// config, so on failure the source is normally left intact and the fallback is
// safe; the dest-already-moved check covers the rare partial-move case.
func (g *GitWorktree) relocateWorktreeTo(dest string) error {
	src := g.worktreePath
	if g.externalWorktree {
		return fmt.Errorf("cannot relocate an in-place/external worktree at %s (it is user-owned)", src)
	}
	if src == "" {
		return fmt.Errorf("cannot relocate worktree: source path is empty")
	}
	if dest == "" {
		return fmt.Errorf("cannot relocate worktree: destination path is empty")
	}
	if !pathExists(src) {
		return fmt.Errorf("cannot relocate worktree: source %s does not exist", src)
	}
	if pathExists(dest) {
		return fmt.Errorf("cannot relocate worktree: destination %s already exists", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return fmt.Errorf("failed to create destination parent directory for %s: %w", dest, err)
	}

	if err := worktreeMoveFast(g, src, dest); err != nil {
		log.WarningLog.Printf("git worktree move %s -> %s failed (%v); falling back to manual move + repair", src, dest, err)
		// The fast path may have moved the directory before failing to update
		// its config (rare). Only move bytes ourselves if the dir is still at
		// src; either way, repair fixes the two-way registration.
		if !pathExists(dest) {
			if mErr := moveDirCrossDevice(src, dest); mErr != nil {
				return fmt.Errorf("failed to move worktree %s -> %s: %w", src, dest, mErr)
			}
		}
		// The bytes are now at dest. Commit the new location to the worktree
		// object IMMEDIATELY — before the repair below — so g.worktreePath always
		// points at where the files actually are. If repair then fails, the
		// registration is stale but the location is not: returning here with
		// worktreePath still at the now-removed src would strand the caller
		// pointing at an empty path while the bytes live at dest, and the
		// archive move-failure path (#1028 PR 3) marks the instance Lost and
		// relies on a consistent worktree location.
		g.setWorktreeLocation(dest)
		if rErr := worktreeRepair(g, dest); rErr != nil {
			return fmt.Errorf("moved worktree to %s but failed to repair its git registration: %w", dest, rErr)
		}
		if sErr := worktreeRepairSubmodules(g, dest); sErr != nil {
			return fmt.Errorf("moved worktree to %s but failed to repair submodule gitdirs: %w", dest, sErr)
		}
		return nil
	}

	// Fast path succeeded: git moved the bytes and updated the registration.
	g.setWorktreeLocation(dest)
	return nil
}

// setWorktreeLocation records dest as the worktree's current on-disk location.
func (g *GitWorktree) setWorktreeLocation(dest string) {
	g.worktreePath = dest
	g.worktreeDir = filepath.Dir(dest)
}

// ensureRepoPresent reports ErrRepoGone when the origin repo is missing or no
// longer a git repository. Used by RestoreWorktreeTo so the caller can surface
// the repo-gone case distinctly (leave the archive intact) rather than as a
// generic move failure.
func (g *GitWorktree) ensureRepoPresent() error {
	if g.repoPath == "" {
		return fmt.Errorf("%w: repo path is empty", ErrRepoGone)
	}
	if _, err := os.Stat(g.repoPath); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrRepoGone, g.repoPath, err)
	}
	if _, err := g.runGitCommand(g.repoPath, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("%w: %s is no longer a git repository: %v", ErrRepoGone, g.repoPath, err)
	}
	return nil
}

// moveDirCrossDevice moves src to dest, falling back to a copy+remove when the
// two paths straddle a filesystem boundary (os.Rename returns EXDEV) — the
// common case when the archive root lives on a different device than the repo.
// The copy preserves file contents, modes, and symlinks, so uncommitted changes
// survive verbatim.
func moveDirCrossDevice(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	// Cross-device: copy the tree, then remove the original.
	if err := copyTree(src, dest); err != nil {
		// Best-effort cleanup of a partial copy so a retry sees a clean dest.
		_ = os.RemoveAll(dest)
		return err
	}
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("copied worktree to %s but failed to remove original %s: %w", dest, src, err)
	}
	return nil
}

// copyTree recursively copies the directory rooted at src to dest, preserving
// regular files (contents + permission bits), subdirectories, and symlinks
// (copied as links, never followed). It is only reached on the cross-device
// fallback; a git worktree contains exactly these node kinds (including the
// `.git` pointer file, which `git worktree repair` rewrites afterwards).
func copyTree(src, dest string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return copyFile(path, target, info.Mode().Perm())
		}
	})
}

// copyFile copies a single regular file's contents to dst, creating it with the
// given permission bits.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// pathExists reports whether p exists (best-effort: a stat error other than
// not-exist is treated as "exists" so we never clobber an unreadable path).
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil || !os.IsNotExist(err)
}

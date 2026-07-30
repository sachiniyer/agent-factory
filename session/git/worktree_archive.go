package git

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
	"golang.org/x/sys/unix"
)

// RestoreWorktreePath returns where an archived session's worktree should be
// restored, honoring the configured worktree_root placement (#1540) exactly as
// NewGitWorktree does at creation — routing through the shared
// resolveWorktreePlacement. Sibling mode returns {repoParent}/{repoName}-
// {safeTitle}; subdirectory mode returns {AF_HOME}/worktrees/{branchName}, so a
// subdirectory user gets the worktree back where it belongs instead of stranded
// beside the repo. branchName is the session's persisted branch (used only for
// subdirectory placement). A numeric suffix is appended if the path is occupied,
// and the result is validated to sit strictly inside the worktree parent (#461).
func RestoreWorktreePath(repoPath, title, branchName string) (string, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", fmt.Errorf("failed to load config: %w", err)
	}
	repoRoot, err := findGitRepoRoot(repoPath)
	if err != nil {
		return "", err
	}
	worktreeDir, err := getWorktreeDirectoryForRepoWithConfig(cfg, repoRoot)
	if err != nil {
		return "", err
	}
	return resolveWorktreePlacement(cfg, repoRoot, worktreeDir, title, branchName)
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
// pointers without fetching or checking out new content; the foreach pass makes
// the repair explicit for initialized nested submodules on Git versions whose
// top-level absorb does not recurse.
var worktreeRepairSubmodules = func(g *GitWorktree, dest string) error {
	if _, err := g.runGitCommand(dest, "submodule", "absorbgitdirs"); err != nil {
		return err
	}
	_, err := g.runGitCommand(dest, "submodule", "foreach", "--recursive", "git submodule absorbgitdirs")
	return err
}

// Filesystem operation seams let tests force cross-device and cleanup-failure
// paths deterministically. Production never reassigns them.
var (
	renamePath                  = renamePathNoReplace
	removeAllPath               = os.RemoveAll
	copyTreeBeforeSourceOpen    = func(string) error { return nil }
	moveDirBeforeDestParentOpen = func(string) error { return nil }
	moveDirBeforeDestCommit     = func(string) error { return nil }
	moveDirBeforeSourceCommit   = func(string) error { return nil }
)

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
		var sourceCleanupErr error
		if !pathExists(dest) {
			if mErr := moveDirCrossDevice(src, dest); mErr != nil {
				var copiedErr *copiedWorktreeSourceCleanupError
				if !errors.As(mErr, &copiedErr) {
					return fmt.Errorf("failed to move worktree %s -> %s: %w", src, dest, mErr)
				}
				sourceCleanupErr = mErr
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
			if sourceCleanupErr != nil {
				return fmt.Errorf("copied worktree to %s but failed to remove original %s and failed to repair its git registration: %v: %w", dest, src, rErr, sourceCleanupErr)
			}
			return fmt.Errorf("moved worktree to %s but failed to repair its git registration: %w", dest, rErr)
		}
		if sErr := worktreeRepairSubmodules(g, dest); sErr != nil {
			log.WarningLog.Printf(
				"submodule gitdir repair failed after moving worktree to %s; "+
					"run `%s` "+
					"(or `%s`) "+
					"to fix submodule status; continuing because the worktree move "+
					"and registration repair already succeeded: %v",
				dest,
				shellsuggest.Command("git", "-C", dest, "submodule", "absorbgitdirs"),
				shellsuggest.Command("git", "-C", dest, "submodule", "update", "--init", "--recursive"),
				sErr,
			)
		}
		if sourceCleanupErr != nil {
			// Copy AND git registration both succeeded: the worktree is valid,
			// registered, and usable at dest. Removing the leftover source dir is
			// the only step that failed — a disk-reclamation nuisance, not a move
			// failure. Returning an error here is actively harmful (#2011): it
			// drives the daemon's archive-rollback / restore-retry logic even
			// though a valid worktree already exists at dest, and the retry picks a
			// fresh collision-suffixed dest, copies + registers a SECOND worktree,
			// orphaning the first and corrupting `git worktree list` and branch
			// exclusivity. The old "remove the original manually" advice is worse
			// than useless: instance state may still point at src, so following it
			// breaks recovery. Warn (so the leftover disk stays visible and
			// reclaimable) and return nil.
			log.WarningLog.Printf(
				"worktree copied and registered at %s, but failed to remove the leftover source directory %s; "+
					"the worktree is valid and usable at %s — the leftover is only reclaimable disk, "+
					"remove it by hand with `%s`: %v",
				dest, src, dest,
				shellsuggest.Command("rm", "-rf", src),
				sourceCleanupErr,
			)
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
	if err := renamePath(src, dest); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	// Cross-device: copy into an unguessable sibling, atomically claim the
	// verified source endpoint, then atomically publish the copied directory at
	// dest without replacing anything. These two renames are the commit boundary:
	// until both identities match, the source is restored and never deleted.
	stagingPath, err := privateMovePath(dest, "copy")
	if err != nil {
		return err
	}
	copied, err := copyTreeWithIdentities(src, stagingPath)
	if err != nil {
		cleanupErr := removeAllPath(stagingPath)
		if cleanupErr != nil {
			return fmt.Errorf("failed to copy worktree into private staging directory %s (%v) and failed to clean that staging tree: %w", stagingPath, err, cleanupErr)
		}
		return fmt.Errorf("failed to copy worktree into private staging directory %s: %w", stagingPath, err)
	}

	sourceParentPath := filepath.Dir(src)
	sourceParent, _, err := openDirectoryPathFollowingLinks(sourceParentPath, "source parent")
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	sourceName := filepath.Base(src)
	quarantinePath, err := privateMovePath(src, "source")
	if err != nil {
		return err
	}
	if err := moveDirBeforeSourceCommit(src); err != nil {
		return err
	}
	quarantineName := filepath.Base(quarantinePath)
	if err := renameAtNoReplace(int(sourceParent.Fd()), sourceName, int(sourceParent.Fd()), quarantineName); err != nil {
		return fmt.Errorf("failed to atomically secure source directory %s before cleanup: %w", src, err)
	}
	quarantinedSource, quarantinedInfo, err := openDirectoryAt(sourceParent, quarantineName, quarantinePath, "secured source")
	if err != nil {
		return fmt.Errorf("source directory changed while it was being secured; refusing cleanup: %w", err)
	}
	defer quarantinedSource.Close()
	if !os.SameFile(copied.source, quarantinedInfo) {
		restoreErr := restoreSecuredSource(sourceParent, quarantineName, sourceName)
		if restoreErr != nil {
			return fmt.Errorf("source directory changed while it was copied; replacement was preserved at %s but could not be restored to %s: %w", quarantinePath, src, restoreErr)
		}
		return fmt.Errorf("source directory changed while it was copied; restored the replacement at %s and refused cleanup", src)
	}

	destinationParentPath := filepath.Dir(dest)
	if err := moveDirBeforeDestParentOpen(destinationParentPath); err != nil {
		if restoreErr := restoreSecuredSource(sourceParent, quarantineName, sourceName); restoreErr != nil {
			return fmt.Errorf("destination parent hook failed (%v) and secured source could not be restored: %w", err, restoreErr)
		}
		return err
	}
	destinationParent, _, err := openDirectoryPathFollowingLinks(destinationParentPath, "destination parent")
	if err != nil {
		if restoreErr := restoreSecuredSource(sourceParent, quarantineName, sourceName); restoreErr != nil {
			return fmt.Errorf("failed to reopen destination parent %s (%v) and secured source could not be restored: %w", destinationParentPath, err, restoreErr)
		}
		return err
	}
	defer destinationParent.Close()
	if err := moveDirBeforeDestCommit(stagingPath); err != nil {
		if restoreErr := restoreSecuredSource(sourceParent, quarantineName, sourceName); restoreErr != nil {
			return fmt.Errorf("destination commit hook failed (%v) and secured source could not be restored: %w", err, restoreErr)
		}
		return err
	}
	if err := renameAtNoReplace(
		int(destinationParent.Fd()), filepath.Base(stagingPath),
		int(destinationParent.Fd()), filepath.Base(dest),
	); err != nil {
		if restoreErr := restoreSecuredSource(sourceParent, quarantineName, sourceName); restoreErr != nil {
			return fmt.Errorf("failed to commit destination %s (%v) and secured source could not be restored: %w", dest, err, restoreErr)
		}
		return fmt.Errorf("failed to atomically commit copied worktree at %s without replacement: %w", dest, err)
	}
	currentDestination, err := os.Lstat(dest)
	if err != nil || !os.SameFile(copied.destination, currentDestination) {
		if restoreErr := restoreSecuredSource(sourceParent, quarantineName, sourceName); restoreErr != nil {
			return fmt.Errorf("destination directory changed during commit and secured source could not be restored: %w", restoreErr)
		}
		if err != nil {
			return fmt.Errorf("destination directory changed during commit at %s: %w", dest, err)
		}
		return fmt.Errorf("destination directory changed during commit at %s; source was restored", dest)
	}

	if err := removeAllPath(quarantinePath); err != nil {
		return &copiedWorktreeSourceCleanupError{src: quarantinePath, dest: dest, err: err}
	}
	return nil
}

type copiedWorktreeSourceCleanupError struct {
	src  string
	dest string
	err  error
}

func (e *copiedWorktreeSourceCleanupError) Error() string {
	return fmt.Sprintf("copied worktree to %s but failed to remove original %s: %v", e.dest, e.src, e.err)
}

func (e *copiedWorktreeSourceCleanupError) Unwrap() error {
	return e.err
}

// copyTree recursively copies the directory rooted at src to dest, preserving
// regular files (contents + permission bits), subdirectories, and symlinks
// (copied as links, never followed). Traversal stays anchored to open directory
// descriptors: a worktree process can replace any pathname after it is
// inspected, so every source open is nonblocking and no-follow, and every
// destination node is created exclusively. This is only reached on the
// cross-device fallback.
func copyTree(src, dest string) error {
	_, err := copyTreeWithIdentities(src, dest)
	return err
}

type copiedTreeIdentities struct {
	source      os.FileInfo
	destination os.FileInfo
}

func copyTreeWithIdentities(src, dest string) (copiedTreeIdentities, error) {
	source, sourceInfo, err := openDirectoryPath(src, "source")
	if err != nil {
		return copiedTreeIdentities{}, err
	}
	defer source.Close()
	if err := copyTreeBeforeSourceOpen(src); err != nil {
		return copiedTreeIdentities{}, err
	}

	destParentPath := filepath.Dir(dest)
	destParent, _, err := openDirectoryPathFollowingLinks(destParentPath, "destination parent")
	if err != nil {
		return copiedTreeIdentities{}, err
	}
	defer destParent.Close()
	destName := filepath.Base(dest)
	if err := unix.Mkdirat(int(destParent.Fd()), destName, uint32(sourceInfo.Mode().Perm())); err != nil {
		return copiedTreeIdentities{}, fmt.Errorf("cannot move worktree across filesystems: failed to create destination directory %s exclusively: %w", dest, err)
	}
	destination, destinationInfo, err := openDirectoryAt(destParent, destName, dest, "destination")
	if err != nil {
		return copiedTreeIdentities{}, err
	}
	defer destination.Close()

	if err := copyDirectoryContents(source, destination, src, dest); err != nil {
		return copiedTreeIdentities{}, err
	}
	return copiedTreeIdentities{source: sourceInfo, destination: destinationInfo}, nil
}

func copyDirectoryContents(source, destination *os.File, sourcePath, destinationPath string) error {
	names, err := source.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("cannot move worktree across filesystems: failed to enumerate source directory %s: %w", sourcePath, err)
	}
	sort.Strings(names)
	for _, name := range names {
		childSourcePath := filepath.Join(sourcePath, name)
		childDestinationPath := filepath.Join(destinationPath, name)
		var stat unix.Stat_t
		if err := unix.Fstatat(int(source.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("cannot move worktree across filesystems: failed to inspect source entry %s: %w", childSourcePath, err)
		}
		if err := copyTreeBeforeSourceOpen(childSourcePath); err != nil {
			return err
		}

		switch uint32(stat.Mode) & unix.S_IFMT {
		case unix.S_IFDIR:
			if err := copyDirectoryAt(source, destination, name, childSourcePath, childDestinationPath); err != nil {
				return err
			}
		case unix.S_IFLNK:
			link, err := readLinkAt(source, name, childSourcePath)
			if err != nil {
				return err
			}
			if err := unix.Symlinkat(link, int(destination.Fd()), name); err != nil {
				return fmt.Errorf("cannot move worktree across filesystems: failed to create destination symlink %s exclusively: %w", childDestinationPath, err)
			}
		case unix.S_IFREG:
			if err := copyRegularFileAt(source, destination, name, childSourcePath, childDestinationPath); err != nil {
				return err
			}
		default:
			return unsupportedSourceTypeError(childSourcePath, uint32(stat.Mode))
		}
	}
	return nil
}

func copyDirectoryAt(source, destination *os.File, name, sourcePath, destinationPath string) error {
	sourceChild, sourceInfo, err := openDirectoryAt(source, name, sourcePath, "source")
	if err != nil {
		return err
	}
	defer sourceChild.Close()
	if err := unix.Mkdirat(int(destination.Fd()), name, uint32(sourceInfo.Mode().Perm())); err != nil {
		return fmt.Errorf("cannot move worktree across filesystems: failed to create destination directory %s exclusively: %w", destinationPath, err)
	}
	destinationChild, _, err := openDirectoryAt(destination, name, destinationPath, "destination")
	if err != nil {
		return err
	}
	defer destinationChild.Close()
	return copyDirectoryContents(sourceChild, destinationChild, sourcePath, destinationPath)
}

// copyFile is the path-based test entrypoint for the regular-file copier. The
// production tree walker calls copyRegularFileAt with already-open parents.
func copyFile(src, dst string) error {
	sourceParent, _, err := openDirectoryPath(filepath.Dir(src), "source parent")
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	destinationParent, _, err := openDirectoryPathFollowingLinks(filepath.Dir(dst), "destination parent")
	if err != nil {
		return err
	}
	defer destinationParent.Close()
	return copyRegularFileAt(sourceParent, destinationParent, filepath.Base(src), src, dst)
}

func copyRegularFileAt(source, destination *os.File, name, sourcePath, destinationPath string) error {
	fd, err := unix.Openat(
		int(source.Fd()), name,
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return fmt.Errorf("cannot move worktree across filesystems: failed to open source file %s without following links: %w", sourcePath, err)
	}
	in := os.NewFile(uintptr(fd), sourcePath)
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return unsupportedSourceTypeError(sourcePath, uint32(info.Mode()))
	}
	outFD, err := unix.Openat(
		int(destination.Fd()), filepath.Base(destinationPath),
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		uint32(info.Mode().Perm()),
	)
	if err != nil {
		return fmt.Errorf("cannot move worktree across filesystems: failed to create destination file %s exclusively: %w", destinationPath, err)
	}
	out := os.NewFile(uintptr(outFD), destinationPath)
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func openDirectoryPath(path, role string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot move worktree across filesystems: failed to open %s directory %s safely: %w", role, path, err)
	}
	return openedDirectory(fd, path, role)
}

// openDirectoryPathFollowingLinks is used only for an already-configured
// destination parent. Users may intentionally make worktree_root a symlink to
// another filesystem; O_DIRECTORY and O_NONBLOCK still reject a raced-in FIFO,
// while all writes remain anchored to the returned directory descriptor.
func openDirectoryPathFollowingLinks(path, role string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot move worktree across filesystems: failed to open %s directory %s safely: %w", role, path, err)
	}
	return openedDirectory(fd, path, role)
}

func openDirectoryAt(parent *os.File, name, path, role string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Openat(
		int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot move worktree across filesystems: failed to open %s directory %s safely: %w", role, path, err)
	}
	return openedDirectory(fd, path, role)
}

func openedDirectory(fd int, path, role string) (*os.File, os.FileInfo, error) {
	dir := os.NewFile(uintptr(fd), path)
	info, err := dir.Stat()
	if err != nil {
		_ = dir.Close()
		return nil, nil, err
	}
	if !info.IsDir() {
		_ = dir.Close()
		return nil, nil, fmt.Errorf("cannot move worktree across filesystems: %s path %s is not a directory", role, path)
	}
	return dir, info, nil
}

func readLinkAt(parent *os.File, name, path string) (string, error) {
	for size := 256; size <= 64*1024; size *= 2 {
		buffer := make([]byte, size)
		n, err := unix.Readlinkat(int(parent.Fd()), name, buffer)
		if err != nil {
			return "", fmt.Errorf("cannot move worktree across filesystems: failed to read source symlink %s safely: %w", path, err)
		}
		if n < len(buffer) {
			return string(buffer[:n]), nil
		}
	}
	return "", fmt.Errorf("cannot move worktree across filesystems: source symlink %s target is too long", path)
}

func unsupportedSourceTypeError(path string, mode uint32) error {
	return fmt.Errorf("cannot move worktree across filesystems: unsupported file type at %s (mode %#o)", path, mode&unix.S_IFMT)
}

func renamePathNoReplace(src, dest string) error {
	sourceParent, _, err := openDirectoryPathFollowingLinks(filepath.Dir(src), "source parent")
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	destinationParent, _, err := openDirectoryPathFollowingLinks(filepath.Dir(dest), "destination parent")
	if err != nil {
		return err
	}
	defer destinationParent.Close()
	return renameAtNoReplace(
		int(sourceParent.Fd()), filepath.Base(src),
		int(destinationParent.Fd()), filepath.Base(dest),
	)
}

func privateMovePath(path, purpose string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate private %s path beside %s: %w", purpose, path, err)
	}
	name := fmt.Sprintf(".af-%s-%s", purpose, hex.EncodeToString(random[:]))
	return filepath.Join(filepath.Dir(path), name), nil
}

func restoreSecuredSource(parent *os.File, securedName, sourceName string) error {
	return renameAtNoReplace(int(parent.Fd()), securedName, int(parent.Fd()), sourceName)
}

// pathExists reports whether p exists (best-effort: a stat error other than
// not-exist is treated as "exists" so we never clobber an unreadable path).
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil || !os.IsNotExist(err)
}

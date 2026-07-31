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
	removeDirectoryTree         = removeOpenedDirectory
	copyTreeBeforeSourceOpen    = func(string) error { return nil }
	moveDirBeforeDestParentOpen = func(string) error { return nil }
	moveDirBeforeDestCommit     = func(string) error { return nil }
	moveDirBeforeSourceCommit   = func(string) error { return nil }
	moveDirAfterDestCommit      = func(string) error { return nil }
	renamePathAfterCommit       = func(string) error { return nil }
	removeTreeBeforeEntryClaim  = func(*os.File, string) error { return nil }
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
		var sourceCleanupPath string
		var sourceCleanupPathVerified bool
		if !pathExists(dest) {
			if mErr := moveDirCrossDevice(src, dest); mErr != nil {
				var copiedErr *copiedWorktreeSourceCleanupError
				if !errors.As(mErr, &copiedErr) {
					return fmt.Errorf("failed to move worktree %s -> %s: %w", src, dest, mErr)
				}
				sourceCleanupErr = mErr
				sourceCleanupPath = copiedErr.src
				sourceCleanupPathVerified = copiedErr.cleanupPathVerified
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
				return fmt.Errorf("copied worktree to %s but failed to remove original %s and failed to repair its git registration: %v: %w", dest, sourceCleanupPath, rErr, sourceCleanupErr)
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
			if sourceCleanupPathVerified {
				log.WarningLog.Printf(
					"worktree copied and registered at %s, but failed to remove the leftover source directory %s; "+
						"the worktree is valid and usable at %s — the leftover is only reclaimable disk, "+
						"remove it by hand with `%s`: %v",
					dest, sourceCleanupPath, dest,
					shellsuggest.Command("rm", "-rf", sourceCleanupPath),
					sourceCleanupErr,
				)
			} else {
				log.WarningLog.Printf(
					"worktree copied and registered at %s, but source cleanup could not determine the original tree's current pathname; "+
						"the worktree is valid and usable at %s, but do not delete the stale quarantine name %s because it now identifies different data: %v",
					dest, dest, sourceCleanupPath, sourceCleanupErr,
				)
			}
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
func moveDirCrossDevice(src, dest string) (returnErr error) {
	renameErr := renamePath(src, dest)
	if renameErr == nil {
		return nil
	} else if !errors.Is(renameErr, syscall.EXDEV) {
		return renameErr
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
		return fmt.Errorf("failed to copy worktree into private staging directory %s: %w", stagingPath, err)
	}
	defer copied.close()
	stagingName := filepath.Base(stagingPath)
	published := false
	defer func() {
		if published {
			return
		}
		stagingManifest := destinationCleanupManifest(copied.root)
		if cleanupErr := removeDirectoryTree(
			copied.destinationParent, stagingName, stagingPath, copied.destination, &stagingManifest,
		); cleanupErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("failed to clean private staging tree %s: %w", stagingPath, cleanupErr))
		}
	}()

	sourceParentPath := filepath.Dir(src)
	sourceParent, _, err := openDirectoryPathFollowingLinks(sourceParentPath, "source parent")
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	sourceParentIdentity, err := identityFromFile(sourceParent)
	if err != nil {
		return err
	}
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
	quarantinedIdentity, err := identityAt(sourceParent, quarantineName)
	if err != nil {
		if restoreErr := restoreClaimedSource(sourceParent, quarantineName, sourceName); restoreErr != nil {
			return fmt.Errorf("failed to inspect secured source %s (%v) and could not restore it to %s: %w", quarantinePath, err, src, restoreErr)
		}
		if pathErr := validateDirectoryPathIdentity(sourceParentPath, "source", sourceParentIdentity); pathErr != nil {
			return errors.Join(fmt.Errorf("failed to inspect secured source %s: %w", quarantinePath, err), pathErr)
		}
		return fmt.Errorf("failed to inspect secured source %s; restored it to %s: %w", quarantinePath, src, err)
	}
	if !copied.sourceIdentity.same(quarantinedIdentity) {
		restoreErr := restoreClaimedSource(sourceParent, quarantineName, sourceName)
		if restoreErr != nil {
			return fmt.Errorf("source directory changed while it was copied; replacement was preserved at %s but could not be restored to %s: %w", quarantinePath, src, restoreErr)
		}
		if pathErr := validateNamedPathIdentity(
			sourceParentPath, sourceName, "source", sourceParentIdentity, quarantinedIdentity,
		); pathErr != nil {
			return errors.Join(errors.New("source directory changed while it was copied"), pathErr)
		}
		return fmt.Errorf("source directory changed while it was copied; restored the replacement at %s and refused cleanup", src)
	}
	restoreSource := func(cause error) error {
		restoreErr := restoreSecuredSource(sourceParent, quarantineName, sourceName, copied.source)
		if restoreErr != nil {
			return fmt.Errorf("%v; secured source at %s could not be restored to %s: %w", cause, quarantinePath, src, restoreErr)
		}
		if pathErr := validateNamedPathIdentity(
			sourceParentPath, sourceName, "source", sourceParentIdentity, copied.sourceIdentity,
		); pathErr != nil {
			return errors.Join(cause, pathErr)
		}
		return cause
	}

	destinationParentPath := filepath.Dir(dest)
	if err := moveDirBeforeDestParentOpen(destinationParentPath); err != nil {
		return restoreSource(err)
	}
	currentDestinationParent, _, err := openDirectoryPathFollowingLinks(destinationParentPath, "destination parent")
	if err != nil {
		return restoreSource(err)
	}
	currentDestinationParentIdentity, err := identityFromFile(currentDestinationParent)
	currentDestinationParent.Close()
	if err != nil || !copied.destinationParentIdentity.same(currentDestinationParentIdentity) {
		if err == nil {
			err = fmt.Errorf("destination parent changed while the worktree was copied")
		}
		return restoreSource(err)
	}
	if err := moveDirBeforeDestCommit(stagingPath); err != nil {
		return restoreSource(err)
	}
	if err := copied.validateSource(quarantinePath); err != nil {
		return restoreSource(fmt.Errorf("source tree changed after copy: %w", err))
	}
	if err := copied.validateDestination(stagingPath); err != nil {
		return restoreSource(fmt.Errorf("destination tree changed after copy: %w", err))
	}
	if err := renameAtNoReplace(
		int(copied.destinationParent.Fd()), stagingName,
		int(copied.destinationParent.Fd()), filepath.Base(dest),
	); err != nil {
		return restoreSource(fmt.Errorf("failed to atomically commit copied worktree at %s without replacement: %w", dest, err))
	}
	published = true
	commitErr := errors.Join(moveDirAfterDestCommit(dest), validatePublishedDestination(dest, copied))
	if commitErr != nil {
		destinationManifest := destinationCleanupManifest(copied.root)
		cleanupErr := removeDirectoryTree(
			copied.destinationParent, filepath.Base(dest), dest, copied.destination, &destinationManifest,
		)
		if cleanupErr != nil {
			commitErr = errors.Join(commitErr, fmt.Errorf("failed to remove unverified destination %s: %w", dest, cleanupErr))
		}
		return restoreSource(commitErr)
	}

	if err := removeDirectoryTree(sourceParent, quarantineName, quarantinePath, copied.source, &copied.root); err != nil {
		var unverified *unverifiedCleanupPathError
		cleanupPathVerified := !errors.As(err, &unverified)
		if pathErr := validateDirectoryPathIdentity(sourceParentPath, "source", sourceParentIdentity); pathErr != nil {
			err = errors.Join(err, pathErr)
			cleanupPathVerified = false
		}
		return &copiedWorktreeSourceCleanupError{
			src:                 quarantinePath,
			dest:                dest,
			err:                 err,
			cleanupPathVerified: cleanupPathVerified,
		}
	}
	return nil
}

type copiedWorktreeSourceCleanupError struct {
	src                 string
	dest                string
	err                 error
	cleanupPathVerified bool
}

func (e *copiedWorktreeSourceCleanupError) Error() string {
	if !e.cleanupPathVerified {
		return fmt.Sprintf("copied worktree to %s but could not determine the original source's current pathname near %s: %v", e.dest, e.src, e.err)
	}
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
	copied, err := copyTreeWithIdentities(src, dest)
	if copied != nil {
		copied.close()
	}
	return err
}

type copiedTreeIdentities struct {
	source                    *os.File
	sourceIdentity            pathIdentity
	destinationParent         *os.File
	destinationParentIdentity pathIdentity
	destination               *os.File
	destinationIdentity       pathIdentity
	root                      copiedDirectory
}

type copiedDirectory struct {
	entries []copiedEntry
}

type copiedEntry struct {
	name        string
	source      pathIdentity
	destination pathIdentity
	directory   *copiedDirectory
}

type copiedDirectoryRoute struct {
	components []copiedEntry
	directory  *copiedDirectory
}

type pathIdentity struct {
	device   uint64
	inode    uint64
	fileType uint32
}

func (identity pathIdentity) same(other pathIdentity) bool {
	return identity == other
}

func (copied *copiedTreeIdentities) close() {
	_ = copied.destination.Close()
	_ = copied.destinationParent.Close()
	_ = copied.source.Close()
}

func (copied *copiedTreeIdentities) validateSource(path string) error {
	current, err := identityFromFile(copied.source)
	if err != nil {
		return err
	}
	if !copied.sourceIdentity.same(current) {
		return fmt.Errorf("source root identity changed at %s", path)
	}
	return validateCopiedTree(copied.source, copied.root, true, path)
}

func (copied *copiedTreeIdentities) validateDestination(path string) error {
	current, err := identityFromFile(copied.destination)
	if err != nil {
		return err
	}
	if !copied.destinationIdentity.same(current) {
		return fmt.Errorf("destination root identity changed at %s", path)
	}
	return validateCopiedTree(copied.destination, copied.root, false, path)
}

func openDirectoryRoute(
	root *os.File,
	rootPath string,
	components []copiedEntry,
	source bool,
) (*os.File, error) {
	role := "destination"
	if source {
		role = "source"
	}
	current, _, err := openDirectoryAt(root, ".", rootPath, role)
	if err != nil {
		return nil, err
	}
	currentPath := rootPath
	for _, component := range components {
		currentPath = filepath.Join(currentPath, component.name)
		next, _, err := openDirectoryAt(current, component.name, currentPath, role)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		current = next
		identity, err := identityFromFile(current)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		expected := component.destination
		if source {
			expected = component.source
		}
		if !expected.same(identity) {
			_ = current.Close()
			return nil, fmt.Errorf("%s directory identity changed at %s", role, currentPath)
		}
	}
	return current, nil
}

func directoryRoutePath(rootPath string, components []copiedEntry) string {
	path := rootPath
	for _, component := range components {
		path = filepath.Join(path, component.name)
	}
	return path
}

func copyTreeWithIdentities(src, dest string) (*copiedTreeIdentities, error) {
	source, sourceInfo, err := openDirectoryPath(src, "source")
	if err != nil {
		return nil, err
	}
	if err := copyTreeBeforeSourceOpen(src); err != nil {
		_ = source.Close()
		return nil, err
	}
	sourceIdentity, err := identityFromFile(source)
	if err != nil {
		_ = source.Close()
		return nil, err
	}

	destParentPath := filepath.Dir(dest)
	destParent, _, err := openDirectoryPathFollowingLinks(destParentPath, "destination parent")
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	destinationParentIdentity, err := identityFromFile(destParent)
	if err != nil {
		_ = destParent.Close()
		_ = source.Close()
		return nil, err
	}
	destName := filepath.Base(dest)
	if err := unix.Mkdirat(int(destParent.Fd()), destName, uint32(sourceInfo.Mode().Perm())); err != nil {
		_ = destParent.Close()
		_ = source.Close()
		return nil, fmt.Errorf("cannot move worktree across filesystems: failed to create destination directory %s exclusively: %w", dest, err)
	}
	destination, _, err := openDirectoryAt(destParent, destName, dest, "destination")
	if err != nil {
		cleanupErr := removeEmptyDirectoryAt(destParent, destName, dest)
		_ = destParent.Close()
		_ = source.Close()
		return nil, errors.Join(err, cleanupErr)
	}
	destinationIdentity, err := identityFromFile(destination)
	if err != nil {
		cleanupErr := removeEmptyDirectoryAt(destParent, destName, dest)
		_ = destination.Close()
		_ = destParent.Close()
		_ = source.Close()
		return nil, errors.Join(err, cleanupErr)
	}
	copied := &copiedTreeIdentities{
		source:                    source,
		sourceIdentity:            sourceIdentity,
		destinationParent:         destParent,
		destinationParentIdentity: destinationParentIdentity,
		destination:               destination,
		destinationIdentity:       destinationIdentity,
	}

	copied.root, err = copyDirectoryContents(source, destination, src, dest)
	if err != nil {
		cleanupErr := removeOpenedDirectory(destParent, destName, dest, destination, nil)
		copied.close()
		if cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("failed to clean partial destination tree %s: %w", dest, cleanupErr))
		}
		return nil, err
	}
	return copied, nil
}

func copyDirectoryContents(source, destination *os.File, sourcePath, destinationPath string) (copiedDirectory, error) {
	root := copiedDirectory{}
	queue := []copiedDirectoryRoute{{directory: &root}}
	for len(queue) > 0 {
		job := queue[0]
		queue = queue[1:]
		sourceDirectory, err := openDirectoryRoute(source, sourcePath, job.components, true)
		if err != nil {
			return copiedDirectory{}, err
		}
		destinationDirectory, err := openDirectoryRoute(destination, destinationPath, job.components, false)
		if err != nil {
			_ = sourceDirectory.Close()
			return copiedDirectory{}, err
		}
		children, err := copyDirectoryLevel(
			sourceDirectory,
			destinationDirectory,
			directoryRoutePath(sourcePath, job.components),
			directoryRoutePath(destinationPath, job.components),
			job,
		)
		_ = destinationDirectory.Close()
		_ = sourceDirectory.Close()
		if err != nil {
			return copiedDirectory{}, err
		}
		queue = append(queue, children...)
	}
	return root, nil
}

func copyDirectoryLevel(
	source, destination *os.File,
	sourcePath, destinationPath string,
	job copiedDirectoryRoute,
) ([]copiedDirectoryRoute, error) {
	names, err := source.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("cannot move worktree across filesystems: failed to enumerate source directory %s: %w", sourcePath, err)
	}
	sort.Strings(names)
	job.directory.entries = make([]copiedEntry, 0, len(names))
	children := make([]copiedDirectoryRoute, 0)
	for _, name := range names {
		childSourcePath := filepath.Join(sourcePath, name)
		childDestinationPath := filepath.Join(destinationPath, name)
		if err := copyTreeBeforeSourceOpen(childSourcePath); err != nil {
			return nil, err
		}
		stat, err := statAt(source, name)
		if err != nil {
			return nil, fmt.Errorf("cannot move worktree across filesystems: failed to inspect source entry %s: %w", childSourcePath, err)
		}
		inspected := identityFromStat(stat)

		var entry copiedEntry
		switch inspected.fileType {
		case unix.S_IFDIR:
			entry, err = copyDirectoryEntry(source, destination, name, childSourcePath, childDestinationPath, inspected)
		case unix.S_IFLNK:
			entry, err = copySymlinkEntry(source, destination, name, childSourcePath, childDestinationPath, inspected)
		case unix.S_IFREG:
			entry, err = copyRegularFileAtWithIdentity(source, destination, name, childSourcePath, childDestinationPath, &inspected)
		default:
			err = unsupportedSourceTypeError(childSourcePath, uint32(stat.Mode))
		}
		if err != nil {
			return nil, err
		}
		job.directory.entries = append(job.directory.entries, entry)
		if entry.directory != nil {
			components := append([]copiedEntry(nil), job.components...)
			components = append(components, entry)
			children = append(children, copiedDirectoryRoute{components: components, directory: entry.directory})
		}
	}
	return children, nil
}

func copyDirectoryEntry(
	source, destination *os.File,
	name, sourcePath, destinationPath string,
	inspected pathIdentity,
) (copiedEntry, error) {
	sourceChild, sourceInfo, err := openDirectoryAt(source, name, sourcePath, "source")
	if err != nil {
		return copiedEntry{}, err
	}
	defer sourceChild.Close()
	sourceIdentity, err := identityFromFile(sourceChild)
	if err != nil {
		return copiedEntry{}, err
	}
	if !inspected.same(sourceIdentity) {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: source directory %s changed before it was opened", sourcePath)
	}
	if err := unix.Mkdirat(int(destination.Fd()), name, uint32(sourceInfo.Mode().Perm())); err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to create destination directory %s exclusively: %w", destinationPath, err)
	}
	destinationChild, _, err := openDirectoryAt(destination, name, destinationPath, "destination")
	if err != nil {
		return copiedEntry{}, err
	}
	defer destinationChild.Close()
	destinationIdentity, err := identityFromFile(destinationChild)
	if err != nil {
		return copiedEntry{}, err
	}
	children := copiedDirectory{}
	return copiedEntry{
		name:        name,
		source:      sourceIdentity,
		destination: destinationIdentity,
		directory:   &children,
	}, nil
}

func copySymlinkEntry(
	source, destination *os.File,
	name, sourcePath, destinationPath string,
	inspected pathIdentity,
) (copiedEntry, error) {
	link, err := readLinkAt(source, name, sourcePath)
	if err != nil {
		return copiedEntry{}, err
	}
	current, err := identityAt(source, name)
	if err != nil || !inspected.same(current) {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: source symlink %s changed while it was copied", sourcePath)
	}
	if err := unix.Symlinkat(link, int(destination.Fd()), name); err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to create destination symlink %s exclusively: %w", destinationPath, err)
	}
	destinationIdentity, err := identityAt(destination, name)
	if err != nil {
		return copiedEntry{}, err
	}
	return copiedEntry{name: name, source: inspected, destination: destinationIdentity}, nil
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
	_, err := copyRegularFileAtWithIdentity(source, destination, name, sourcePath, destinationPath, nil)
	return err
}

func copyRegularFileAtWithIdentity(
	source, destination *os.File,
	name, sourcePath, destinationPath string,
	inspected *pathIdentity,
) (copiedEntry, error) {
	fd, err := unix.Openat(
		int(source.Fd()), name,
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to open source file %s without following links: %w", sourcePath, err)
	}
	in := os.NewFile(uintptr(fd), sourcePath)
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return copiedEntry{}, err
	}
	if !info.Mode().IsRegular() {
		return copiedEntry{}, unsupportedSourceTypeError(sourcePath, uint32(info.Mode()))
	}
	sourceIdentity, err := identityFromFile(in)
	if err != nil {
		return copiedEntry{}, err
	}
	if inspected != nil && !inspected.same(sourceIdentity) {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: source file %s changed before it was opened", sourcePath)
	}
	outFD, err := unix.Openat(
		int(destination.Fd()), filepath.Base(destinationPath),
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		uint32(info.Mode().Perm()),
	)
	if err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to create destination file %s exclusively: %w", destinationPath, err)
	}
	out := os.NewFile(uintptr(outFD), destinationPath)
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return copiedEntry{}, err
	}
	destinationIdentity, err := identityFromFile(out)
	if err != nil {
		_ = out.Close()
		return copiedEntry{}, err
	}
	if err := out.Close(); err != nil {
		return copiedEntry{}, err
	}
	return copiedEntry{name: name, source: sourceIdentity, destination: destinationIdentity}, nil
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

func privateMovePath(path, purpose string) (string, error) {
	name, err := privateMoveName(purpose)
	if err != nil {
		return "", fmt.Errorf("generate private %s path beside %s: %w", purpose, path, err)
	}
	return filepath.Join(filepath.Dir(path), name), nil
}

func privateMoveName(purpose string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf(".af-%s-%s", purpose, hex.EncodeToString(random[:])), nil
}

func restoreClaimedSource(parent *os.File, securedName, sourceName string) error {
	return renameAtNoReplace(int(parent.Fd()), securedName, int(parent.Fd()), sourceName)
}

func restoreSecuredSource(parent *os.File, securedName, sourceName string, source *os.File) error {
	expected, err := identityFromFile(source)
	if err != nil {
		return err
	}
	current, err := identityAt(parent, securedName)
	if err != nil || !expected.same(current) {
		return fmt.Errorf("secured source name no longer identifies the opened source")
	}
	if err := restoreClaimedSource(parent, securedName, sourceName); err != nil {
		return err
	}
	restored, err := identityAt(parent, sourceName)
	if err != nil || !expected.same(restored) {
		return fmt.Errorf("restored source name does not identify the opened source")
	}
	return nil
}

// pathExists reports whether p exists (best-effort: a stat error other than
// not-exist is treated as "exists" so we never clobber an unreadable path).
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil || !os.IsNotExist(err)
}

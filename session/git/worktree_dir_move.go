// The secure directory-move layer the relocation paths share: the EXDEV
// copy+claim+publish engine (moveDirCrossDevice*), the fd-relative open helpers
// that never follow a raced symlink, the unguessable private-move names, and
// the source-restore renames. Nothing here knows about sessions — only paths,
// identities, and the two-rename commit boundary.
package git

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// moveDirCrossDevice moves src to dest, falling back to a copy+remove when the
// two paths straddle a filesystem boundary (os.Rename returns EXDEV) — the
// common case when the archive root lives on a different device than the repo.
// The copy preserves file contents, modes, and symlinks, so uncommitted changes
// survive verbatim.
// operation names the caller's action, purely for error text. It is stamped onto
// an unreadableSourceError BEFORE that error is wrapped: fmt.Errorf formats and
// CACHES the inner error's text at construction, so stamping after the wrap
// changes the struct and not the message the user sees (#3087 review).
func moveDirCrossDevice(src, dest, operation string) error {
	return moveDirCrossDeviceRecordingIdentity(src, dest, operation, refuseUnreadable, nil, nil, nil)
}

type relocationPublicationCheckpoint func(ArchiveReport, pathIdentity, func() error) error

// moveDirCrossDeviceRecordingIdentity is the relocation path's variant. The
// caller initializes committedIdentity with the claimed source identity. A
// same-filesystem rename preserves it; a verified cross-device publication
// replaces it with the copied root identity at the exact no-replace commit.
func moveDirCrossDeviceRecordingIdentity(
	src, dest, operation string,
	policy unreadablePolicy,
	committedIdentity *pathIdentity,
	archiveReport *ArchiveReport,
	checkpoint relocationPublicationCheckpoint,
) (returnErr error) {
	renamePublish := func() error { return renamePath(src, dest) }
	var renameErr error
	if checkpoint != nil {
		if committedIdentity == nil {
			return fmt.Errorf("relocation publication checkpoint requires a committed identity")
		}
		renameErr = checkpoint(ArchiveReport{}, *committedIdentity, renamePublish)
	} else {
		renameErr = renamePublish()
	}
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
	copied, err := copyTreeWithPolicy(src, stagingPath, policy)
	if err != nil {
		// Stamp first, wrap second. The order is the whole point.
		var unreadable *unreadableSourceError
		if errors.As(err, &unreadable) {
			unreadable.operation = operation
		}
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
	quarantinedIdentity, err := moveDirInspectClaimedSource(sourceParent, quarantineName)
	if err != nil {
		// Restore only what still identifies the source this process opened. A
		// racer can strand the claimed entry and drop a replacement at the
		// quarantine name inside this window; an unchecked rename would publish
		// that replacement at src and report it as the restored source, while
		// the real tree stayed stranded under the racer's name.
		if restoreErr := restoreSecuredSource(sourceParent, quarantineName, sourceName, copied.source); restoreErr != nil {
			return fmt.Errorf("failed to inspect secured source %s (%v) and could not restore it to %s: %w", quarantinePath, err, src, restoreErr)
		}
		if pathErr := validateNamedPathIdentity(
			sourceParentPath, sourceName, "source", sourceParentIdentity, copied.sourceIdentity,
		); pathErr != nil {
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
	var report ArchiveReport
	if len(copied.skipped) > 0 {
		if archiveReport == nil {
			return restoreSource(fmt.Errorf("copier skipped unreadable files without an archive report channel"))
		}
		report = ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
			newArchiveRetainedTree(quarantinePath, quarantinedIdentity, copied.skipped),
		}}
	}
	publishDestination := func() error {
		if err := renameAtNoReplace(
			int(copied.destinationParent.Fd()), stagingName,
			int(copied.destinationParent.Fd()), filepath.Base(dest),
		); err != nil {
			return fmt.Errorf("failed to atomically commit copied worktree at %s without replacement: %w", dest, err)
		}
		published = true
		return errors.Join(moveDirAfterDestCommit(dest), validatePublishedDestination(dest, copied))
	}
	var commitErr error
	if checkpoint != nil {
		commitErr = checkpoint(report, copied.destinationIdentity, publishDestination)
	} else {
		commitErr = publishDestination()
	}
	if commitErr != nil {
		if published {
			destinationManifest := destinationCleanupManifest(copied.root)
			cleanupErr := removeDirectoryTree(
				copied.destinationParent, filepath.Base(dest), dest, copied.destination, &destinationManifest,
			)
			if cleanupErr != nil {
				commitErr = errors.Join(commitErr, fmt.Errorf("failed to remove unverified destination %s: %w", dest, cleanupErr))
			}
		}
		return restoreSource(commitErr)
	}
	if committedIdentity != nil {
		*committedIdentity = copied.destinationIdentity
	}
	if !report.Empty() {
		// Keep the COMPLETE secured source rather than deleting bytes the archive
		// never copied. This is intentionally archive-only: the report pointer is
		// supplied only by relocateWorktreeTo's explicit archive role. The hidden
		// source is inert (git registration now points at dest) but recoverable, and
		// its exact location travels in the durable session report.
		*archiveReport = report
		return nil
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

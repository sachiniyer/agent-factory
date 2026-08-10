package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// probeRepoGoneOrigin applies restore's repository-validity rule under the
// caller's teardown-safe deadline. Git owns the path lookup too: an os.Stat
// before a context-aware command could outlive the deadline on a stalled
// filesystem, leaving one abandoned goroutine per retry. A missing or answered
// non-Git directory is gone; an unreadable path or timed-out Git process is
// unknown and must fail closed.
func probeRepoGoneOrigin(ctx context.Context, worktree *GitWorktree) error {
	if worktree.repoPath == "" {
		return fmt.Errorf("%w: repo path is empty", ErrRepoGone)
	}
	if _, err := worktree.runGitCommandContext(ctx, worktree.repoPath, "rev-parse", "--git-dir"); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if definitiveMissingRepository(err) {
			return fmt.Errorf("%w: %s: %v", ErrRepoGone, worktree.repoPath, err)
		}
		if definitiveNonGitRepository(worktree.repoPath, err) {
			return fmt.Errorf("%w: %s is no longer a git repository: %v", ErrRepoGone, worktree.repoPath, err)
		}
		return err
	}
	return nil
}

// definitiveMissingRepository recognizes only Git's C-locale failure to enter
// the requested root. Permission, safe-directory, command-start and every other
// operational error remain unknown and cannot authorize deletion.
func definitiveMissingRepository(probeErr error) bool {
	var exitErr *exec.ExitError
	if !errors.As(probeErr, &exitErr) {
		return false
	}
	diagnostic := string(exitErr.Stderr)
	return strings.Contains(diagnostic, "cannot change to") &&
		strings.Contains(diagnostic, "No such file or directory")
}

// definitiveNonGitRepository accepts only Git's stable outside-repository
// answer with no repository metadata at the recorded root. Command-start,
// permission, corrupt-metadata, safe-directory, and other execution failures
// remain unknown and therefore cannot authorize deletion.
func definitiveNonGitRepository(repoPath string, probeErr error) bool {
	var exitErr *exec.ExitError
	if !errors.As(probeErr, &exitErr) ||
		!strings.Contains(string(exitErr.Stderr), "not a git repository (or any of the parent directories)") ||
		os.Getenv("GIT_DIR") != "" {
		return false
	}
	_, metadataErr := os.Lstat(filepath.Join(repoPath, ".git"))
	return errors.Is(metadataErr, os.ErrNotExist)
}

func boundedRepoGoneOriginProbe(worktree *GitWorktree) error {
	ctx, cancel := context.WithTimeout(context.Background(), relocationIdentityTimeout)
	defer cancel()
	err := repoGoneOriginProbe(ctx, worktree)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf(
			"timed out after %s while checking origin repository %s: %w",
			relocationIdentityTimeout, worktree.repoPath, ctxErr,
		)
	}
	return err
}

// CheckRepoPresentForRelocation applies the same bounded, fail-closed
// repository-validity rule used by cleanup admission. Nil means a valid origin,
// ErrRepoGone means missing or conclusively non-Git, and every other error is an
// unknown operational result which must retain the archive.
func CheckRepoPresentForRelocation(repoPath string) error {
	return boundedRepoGoneOriginProbe(&GitWorktree{repoPath: repoPath})
}

// removeClaimedRepoGoneDirectory removes only the directory identified by the
// recovery claim. The parent and root are opened before the recursive walk, so a
// same-UID rename cannot redirect the walk to a replacement pathname. Every
// descendant and the final root entry use the copy mover's claim-rename-verify-
// unlink primitive; a changed entry is retained rather than reinterpreted.
func removeClaimedRepoGoneDirectory(path string, expected pathIdentity) error {
	parentPath := filepath.Dir(path)
	name := filepath.Base(path)
	parent, _, err := openDirectoryPathFollowingLinks(parentPath, "repo-gone cleanup parent")
	if err != nil {
		return err
	}
	defer parent.Close()

	directory, _, err := openDirectoryAt(parent, name, path, "repo-gone cleanup root")
	if err != nil {
		return err
	}
	defer directory.Close()
	openedIdentity, err := identityFromFile(directory)
	if err != nil {
		return fmt.Errorf("inspect opened repo-gone cleanup root %s: %w", path, err)
	}
	namedIdentity, err := identityAt(parent, name)
	if err != nil || !expected.same(openedIdentity) || !expected.same(namedIdentity) {
		return unverifiedCleanupError("refusing to remove repo-gone worktree %s because its claimed identity changed", path)
	}

	repoGoneBeforeWriterReap(path)
	reapClaimedWorktreeWriters(path, expected)
	repoGoneBeforeRecursiveDelete(path)
	manifest, err := snapshotCopiedTree(directory, path)
	if err != nil {
		return recheckCleanupRoot(parent, name, path, expected, err)
	}
	if err := removeCopiedTree(directory, path, manifest, true); err != nil {
		return recheckCleanupRoot(parent, name, path, expected, err)
	}
	return removeCopiedEntry(parent, parentPath, copiedEntry{
		name: name, source: expected, directory: &copiedDirectory{},
	}, true)
}

func reapClaimedWorktreeWriters(path string, expected pathIdentity) {
	repoGoneReapMatching(path, func(pid int) bool {
		cwd, _, ok := repoGoneOpenWorkingDir(pid)
		if !ok {
			return false
		}
		defer cwd.Close()
		return openedDirectoryAtOrUnder(cwd, expected)
	})
}

func openedDirectoryAtOrUnder(directory *os.File, expected pathIdentity) bool {
	current, _, err := openDirectoryAt(directory, ".", "", "process working")
	if err != nil {
		return false
	}
	for {
		identity, err := identityFromFile(current)
		if err != nil {
			_ = current.Close()
			return false
		}
		if expected.same(identity) {
			_ = current.Close()
			return true
		}
		parent, _, err := openDirectoryAt(current, "..", "", "process working parent")
		if err != nil {
			_ = current.Close()
			return false
		}
		parentIdentity, err := identityFromFile(parent)
		_ = current.Close()
		if err != nil {
			_ = parent.Close()
			return false
		}
		if identity.same(parentIdentity) {
			_ = parent.Close()
			return false
		}
		current = parent
	}
}

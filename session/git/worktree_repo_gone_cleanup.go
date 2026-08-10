package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// probeRepoGoneOrigin applies restore's repository-validity rule under a
// teardown-safe Git deadline. A missing or answered non-Git directory is gone;
// an unreadable path or timed-out Git process is unknown and must fail closed.
func probeRepoGoneOrigin(worktree *GitWorktree) error {
	if worktree.repoPath == "" {
		return fmt.Errorf("%w: repo path is empty", ErrRepoGone)
	}
	if _, err := os.Stat(worktree.repoPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s: %v", ErrRepoGone, worktree.repoPath, err)
		}
		return err
	}
	if _, err := worktree.runGitLocalCommand(worktree.repoPath, "rev-parse", "--git-dir"); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: %s is no longer a git repository: %v", ErrRepoGone, worktree.repoPath, err)
	}
	return nil
}

func boundedRepoGoneOriginProbe(worktree *GitWorktree) error {
	result := make(chan error, 1)
	go func() { result <- repoGoneOriginProbe(worktree) }()
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		return fmt.Errorf(
			"timed out after %s while checking origin repository %s: %w",
			relocationIdentityTimeout, worktree.repoPath, context.DeadlineExceeded,
		)
	}
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

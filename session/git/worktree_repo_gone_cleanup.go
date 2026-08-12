package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var repoGoneCleanupRetainedTrees = func(worktree *GitWorktree) error {
	return worktree.cleanupRetainedArchiveTrees()
}

// removeClaimedRepoGoneDirectory removes only the directory identified by the
// recovery claim. The parent and root are opened before the recursive walk, so a
// same-UID rename cannot redirect the walk to a replacement pathname. Every
// descendant and the final root entry use the copy mover's claim-rename-verify-
// unlink primitive; a changed entry is retained rather than reinterpreted.
func removeClaimedRepoGoneDirectory(
	path string,
	expected pathIdentity,
	expectedGeneration string,
	beforeRootRemoval func(string) error,
) error {
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
	generation, err := cleanupGenerationFromFile(directory)
	if err != nil || generation != expectedGeneration || expectedGeneration == "" {
		return unverifiedCleanupError("refusing to remove repo-gone worktree %s because its durable generation changed", path)
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
	return removeCopiedEntryRestoringNameWithCheckpoint(parent, parentPath, copiedEntry{
		name: name, source: expected, directory: &copiedDirectory{},
	}, true, beforeRootRemoval)
}

// removeFinalizingRepoGoneRoot consumes only the empty root marker described by
// cleanup_finalizing. The descriptor walk had already removed and verified all
// children before that state was persisted. If the name is now absent, the
// original empty marker is gone. A replacement, changed generation, or surviving
// exact marker which has been repopulated remains an unresolved cleanup obligation.
func removeFinalizingRepoGoneRoot(
	path string,
	expected pathIdentity,
	expectedGeneration string,
	beforeRootRemoval func(string) error,
) error {
	parentPath := filepath.Dir(path)
	name := filepath.Base(path)
	parent, _, err := openDirectoryPathFollowingLinks(parentPath, "repo-gone finalization parent")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer parent.Close()

	identity, err := identityAt(parent, name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !expected.same(identity) {
		return unverifiedCleanupError("finalizing root %s no longer has its claimed identity", path)
	}
	directory, _, err := openDirectoryAt(parent, name, path, "repo-gone finalization root")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	generation, generationErr := cleanupGenerationFromFile(directory)
	if generationErr != nil {
		_ = directory.Close()
		return generationErr
	}
	if generation != expectedGeneration || expectedGeneration == "" {
		_ = directory.Close()
		return unverifiedCleanupError("finalizing root %s no longer has its claimed generation", path)
	}
	names, err := directoryNames(directory, path)
	_ = directory.Close()
	if err != nil {
		return err
	}
	if len(names) != 0 {
		return unverifiedCleanupError(
			"refusing to finish repo-gone cleanup because finalizing root %s was repopulated", path,
		)
	}
	return removeCopiedEntryRestoringNameWithCheckpoint(parent, parentPath, copiedEntry{
		name: name, source: expected, directory: &copiedDirectory{},
	}, true, beforeRootRemoval)
}

// CleanupClaimedRepoGone removes an archived worktree using the cleanup-only
// claim authorized before the kill's durable commit. The claim is checked after
// pane teardown, writers are reaped before deletion, and its identity stays
// claimed through an identity-anchored recursive delete. The origin is
// deliberately not probed again here: its absence was the pre-commit admission
// condition, while a returned origin is a separate directory that must not be
// able to strand an already committed kill.
func (g *GitWorktree) CleanupClaimedRepoGone(claim RelocationClaim) (CleanupState, error) {
	state, err, _ := g.cleanupClaimedRepoGone(claim)
	return state, err
}

// CleanupClaimedRepoGoneWithLateResult is the ghost-cleanup form. On a caller
// deadline, lateResult reports the descriptor worker's eventual result so the
// daemon can reconcile the persisted row instead of leaving a completed delete
// fenced forever. A nil channel means no worker outlived the call.
func (g *GitWorktree) CleanupClaimedRepoGoneWithLateResult(
	claim RelocationClaim,
) (CleanupState, error, <-chan error) {
	return g.cleanupClaimedRepoGone(claim)
}

func (g *GitWorktree) cleanupClaimedRepoGone(claim RelocationClaim) (CleanupState, error, <-chan error) {
	completed := false
	defer func() {
		if !completed {
			g.PreserveRelocationClaim(claim)
		}
	}()
	checkpoint := g.repoGoneFinalizationCheckpointSnapshot()
	if claim.cleanupFinalizing {
		if !claim.cleanupRootGone {
			if err := removeFinalizingRepoGoneRoot(
				claim.Path, claim.identity, claim.cleanupGeneration,
				func(securedPath string) error {
					return g.checkpointRepoGoneFinalization(claim, securedPath, checkpoint)
				},
			); err != nil {
				return CleanupStateUnknown, fmt.Errorf(
					"finish repo-gone cleanup root %s: %w", claim.Path, err,
				), nil
			}
			if err := g.completeRemovedRelocationClaim(claim); err != nil {
				return CleanupStateUnknown, err, nil
			}
		} else if err := g.completeRepoGoneFinalization(claim); err != nil {
			return CleanupStateUnknown, err, nil
		}
		completed = true
		return CleanupSettled, nil, nil
	}

	if err := g.RevalidateRelocationClaim(claim); err != nil {
		return CleanupStateUnknown, fmt.Errorf("cannot authorize repo-gone cleanup: %w", err), nil
	}
	if err := g.cancelAndWaitHooks(); err != nil {
		return CleanupStateUnknown, fmt.Errorf(
			"cannot remove repo-gone worktree while post-worktree hooks may still be writing: %w", err,
		), nil
	}
	deleteDone := make(chan error, 1)
	go func() {
		// Current archives may carry complete retained source trees for files the
		// published copy could not read. Consume those handles and the primary
		// archive inside one deadline-bound transaction.
		err := repoGoneCleanupRetainedTrees(g)
		if err != nil {
			deleteDone <- fmt.Errorf("remove retained archive trees before repo-gone cleanup: %w", err)
			return
		}
		if err := g.RevalidateRelocationClaim(claim); err != nil {
			deleteDone <- fmt.Errorf("repo-gone cleanup identity changed before reaping writers: %w", err)
			return
		}
		err = repoGoneRemoveDirectory(claim.Path, claim.identity, claim.cleanupGeneration, func(securedPath string) error {
			return g.checkpointRepoGoneFinalization(claim, securedPath, checkpoint)
		})
		if err == nil {
			err = g.completeRemovedRelocationClaim(claim)
		}
		deleteDone <- err
	}()
	timer := time.NewTimer(repoGoneCleanupTimeout)
	defer timer.Stop()
	select {
	case err := <-deleteDone:
		if err != nil {
			revalidationErr := g.RevalidateRelocationClaim(claim)
			return CleanupStateUnknown, errors.Join(
				fmt.Errorf("failed to remove repo-gone worktree %s: %w", claim.Path, err),
				revalidationErr,
			), nil
		}
	case <-timer.C:
		// The worker may be stuck in an uninterruptible filesystem syscall and
		// cannot safely be killed. Its opened handles identify only the claimed
		// tree, so it may finish in the background; the caller still returns and
		// the durable record remains the retry/manual-recovery handle.
		g.markCleanupStalledWithClaim(&claim)
		completed = true
		return CleanupStateUnknown, fmt.Errorf(
			"repo-gone recursive deletion of %s did not finish within %s: %w",
			claim.Path, repoGoneCleanupTimeout, context.DeadlineExceeded,
		), deleteDone
	}
	completed = true
	return CleanupSettled, nil, nil
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

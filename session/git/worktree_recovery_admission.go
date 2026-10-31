package git

import (
	"errors"
	"fmt"
	"os"
)

// finalizingCleanupCandidateAdmission reports whether the named path is still
// the empty finalizing cleanup root this record checkpointed, and any error
// proving it is no longer that exact root. A conclusive absence (ENOENT) is
// not an error here: the original empty marker is gone, which is completion.
func finalizingCleanupCandidateAdmission(path string, recovery *RelocationRecovery) (bool, error) {
	identity, err := boundedRelocationPathIdentity(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect finalizing cleanup candidate %s: %w", path, err)
	}
	if !recovery.identity().same(identity) {
		return false, nil
	}
	inspection, err := boundedFinalizingCleanupIdentity(path)
	if err != nil {
		return true, fmt.Errorf("inspect exact finalizing cleanup root %s: %w", path, err)
	}
	if !recovery.identity().same(inspection.identity) || inspection.generation != recovery.CleanupGeneration {
		return true, fmt.Errorf("exact finalizing cleanup root %s changed during admission", path)
	}
	if !inspection.empty {
		return true, fmt.Errorf("finalizing cleanup root %s was repopulated before the kill commit", path)
	}
	return true, nil
}

// ValidateRelocationCleanupAdmission rechecks a cleanup-ready record. Kill calls
// this before its tombstone and pane teardown; the cleanup itself consumes a
// fresh claim and validates once more immediately before deleting the directory.
//
// A cleanup_ready record whose single authorized directory is conclusively
// absent (os.ErrNotExist, no alternate candidate) is discharged: there is
// nothing left for the authorization to delete, no alternate remains to
// arbitrate, and the device/inode identity can never be re-established, so
// retaining the record would leave the row permanently undeletable — every
// later consumer wraps the same ENOENT in ErrRelocateStateUnknown and refuses
// (#5102). The kill admission caller persists the discharged record-free state
// through its tombstone, and the record-free absent-worktree teardown then
// settles the missing path the way it already does for a record-free archive.
func (g *GitWorktree) ValidateRelocationCleanupAdmission() error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.relocationRecovery == nil ||
		(g.relocationRecovery.State != RelocationRecoveryCleanupReady &&
			g.relocationRecovery.State != RelocationRecoveryCleanupFinalizing) {
		return errors.Join(fmt.Errorf("worktree has no cleanup-ready relocation identity"), ErrRelocateStateUnknown)
	}
	recovery := g.relocationRecovery
	if recovery.State == RelocationRecoveryCleanupFinalizing {
		primaryMatches, primaryErr := finalizingCleanupCandidateAdmission(g.worktreePath, recovery)
		if primaryMatches {
			if primaryErr != nil {
				return errors.Join(primaryErr, ErrRelocateStateUnknown)
			}
			return nil
		}
		if recovery.AlternatePath != "" {
			alternateMatches, alternateErr := finalizingCleanupCandidateAdmission(recovery.AlternatePath, recovery)
			if alternateMatches {
				if alternateErr != nil {
					return errors.Join(alternateErr, ErrRelocateStateUnknown)
				}
				return nil
			}
			if alternateErr != nil {
				return errors.Join(alternateErr, ErrRelocateStateUnknown)
			}
		}
		if primaryErr != nil {
			return errors.Join(primaryErr, ErrRelocateStateUnknown)
		}
		return nil
	}
	identity, err := boundedRelocationPathIdentity(g.worktreePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && recovery.AlternatePath == "" {
			// The authorization names exactly one directory, that directory is
			// conclusively gone, and no alternate candidate remains to arbitrate.
			// Discharge the record so the row is not left permanently undeletable:
			// an identity-qualified cleanup_ready record over a proven-absent path
			// had no discharge path anywhere — every consumer wrapped the same
			// ENOENT in ErrRelocateStateUnknown and refused, and the in-memory
			// claim_stale downgrade this gate used to make was never persisted by
			// any caller (#5102). A non-ENOENT probe failure (a timeout or an
			// unreadable mount) is unknown rather than absent, so it falls through
			// and keeps the authorization retryable below.
			g.relocationRecovery = nil
			return nil
		}
		// A failed read is not evidence of a changed identity. Keep the exact
		// cleanup authorization retryable unless the path is conclusively absent.
		if errors.Is(err, os.ErrNotExist) {
			recovery.State = RelocationRecoveryClaimStale
		}
		return errors.Join(fmt.Errorf(
			"cannot validate worktree path %s for cleanup: %w",
			g.worktreePath, err,
		), ErrRelocateStateUnknown)
	}
	if !recovery.identity().same(identity) {
		recovery.State = RelocationRecoveryClaimStale
		return errors.Join(fmt.Errorf(
			"worktree path %s no longer identifies the directory authorized for cleanup",
			g.worktreePath,
		), ErrRelocateStateUnknown)
	}
	if generationErr := requireCleanupPathIdentity(
		g.worktreePath, recovery.identity(), recovery.CleanupGeneration,
	); generationErr != nil {
		recovery.State = RelocationRecoveryClaimStale
		return errors.Join(fmt.Errorf(
			"worktree path %s no longer carries its durable cleanup generation: %w",
			g.worktreePath, generationErr,
		), ErrRelocateStateUnknown)
	}
	if g.repoPath == "" {
		return errors.Join(fmt.Errorf("origin repo path is empty"), ErrRelocateStateUnknown)
	}
	if err := boundedRepoGoneOriginProbe(g); err == nil {
		return fmt.Errorf(
			"origin repo %s exists again; restore the archived session before retrying kill",
			g.repoPath,
		)
	} else if !errors.Is(err, ErrRepoGone) && !os.IsNotExist(err) {
		return errors.Join(fmt.Errorf(
			"cannot establish that origin repo %s is still gone: %w",
			g.repoPath, err,
		), ErrRelocateStateUnknown)
	}
	return nil
}

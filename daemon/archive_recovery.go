package daemon

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// Test seam at the boundary between the daemon's early repo-gone guard and the
// git layer's authoritative pre-move check. A repository can disappear in this
// interval in production; tests use the seam to make that race deterministic.
var beforeRestoreWorktreeUse = func() {}

// Test seam immediately before restore derives a repo-dependent destination.
// Production never reassigns it.
var beforeRestoreWorktreePath = func() {}

func (m *Manager) guardRepoGoneRestore(
	repoID, title, repoPath string,
	instance *session.Instance,
	claim sessiongit.RelocationClaim,
) (bool, error) {
	if err := sessiongit.CheckRepoPresentForRelocation(repoPath); err == nil {
		return false, nil
	} else if !errors.Is(err, sessiongit.ErrRepoGone) {
		probeErr := fmt.Errorf("cannot establish origin repo state for %s for session %q: %w", repoPath, title, err)
		instance.PreserveWorktreeRelocationClaimAsUnresolved(claim)
		if persistErr := m.persistInstanceErr(repoID, instance); persistErr != nil {
			return false, errors.Join(probeErr, fmt.Errorf(
				"could not persist the unresolved archived worktree identity: %w", persistErr,
			))
		}
		return false, probeErr
	}
	if err := m.prepareRepoGoneCleanup(repoID, title, repoPath, instance, claim); err != nil {
		return false, err
	}
	return true, fmt.Errorf(
		"cannot restore session %q: its origin repo %s is gone; the archived worktree is intact at %s — recover it manually with git",
		title, repoPath, instance.GetWorktreePath(),
	)
}

// persistRestorePathFailure closes the interval between a successful origin
// guard and repo-dependent destination derivation. The guard's point-in-time
// answer cannot let a later failure return a record-free destructive default.
func (m *Manager) persistRestorePathFailure(
	repoID, title string,
	instance *session.Instance,
	claim sessiongit.RelocationClaim,
	pathCause error,
) error {
	pathErr := fmt.Errorf("cannot determine restore location for %q: %w", title, pathCause)
	instance.PreserveWorktreeRelocationClaimAsUnresolved(claim)
	if persistErr := m.persistInstanceErr(repoID, instance); persistErr != nil {
		return errors.Join(pathErr, fmt.Errorf(
			"could not persist the unresolved archived worktree identity: %w", persistErr,
		))
	}
	return pathErr
}

func (m *Manager) persistUnresolvedRestoreFailure(
	repoID, title string,
	instance *session.Instance,
	claim sessiongit.RelocationClaim,
	restoreErr error,
) error {
	wrapped := fmt.Errorf("failed to restore worktree for %q: %w", title, restoreErr)
	instance.PreserveWorktreeRelocationClaimAsUnresolved(claim)
	if persistErr := m.persistInstanceErr(repoID, instance); persistErr != nil {
		return errors.Join(wrapped, fmt.Errorf(
			"could not persist its unresolved archived worktree identity: %w", persistErr,
		))
	}
	return wrapped
}

// claimRestoreRelocation resolves the archived worktree and durably records any
// failed bounded probe before the restore handler returns.
func (m *Manager) claimRestoreRelocation(
	repoID, title string, instance *session.Instance,
) (sessiongit.RelocationClaim, error) {
	claim, err := instance.ClaimWorktreeRelocationForRetry()
	if err == nil {
		return claim, nil
	}
	if errors.Is(err, sessiongit.ErrRelocateStateUnknown) {
		if persistErr := m.persistInstanceErr(repoID, instance); persistErr != nil {
			return sessiongit.RelocationClaim{}, fmt.Errorf(
				"cannot resolve restore source for session %q and could not persist its recovery record (%v): %w",
				title, persistErr, err,
			)
		}
	}
	return sessiongit.RelocationClaim{}, fmt.Errorf(
		"cannot resolve archived worktree for session %q: %w", title, err,
	)
}

// prepareRepoGoneCleanup is the non-moving completion path. Establish the
// selected directory identity once more, replace its active ownership with a
// cleanup-only durable record, and persist both facts so a later kill must
// revalidate the same directory before deleting it.
func (m *Manager) prepareRepoGoneCleanup(
	repoID, title, repoPath string,
	instance *session.Instance,
	claim sessiongit.RelocationClaim,
) error {
	// Stage a non-destructive fence before installing cleanup authority. If the
	// later cleanup_ready write fails, restart sees claim_stale rather than the
	// record-free archive that this transaction began with.
	instance.PreserveWorktreeRelocationClaimAsUnresolved(claim)
	return m.persistAndInstallRepoGoneCleanup(repoID, title, repoPath, instance)
}

// persistAndInstallRepoGoneCleanup commits the two-phase cleanup transition.
// Its caller has already removed destructive authority by staging claim_stale;
// only after that fence is durable may cleanup_ready be installed and written.
func (m *Manager) persistAndInstallRepoGoneCleanup(
	repoID, title, repoPath string,
	instance *session.Instance,
) error {
	if persistErr := m.persistInstanceErr(repoID, instance); persistErr != nil {
		return fmt.Errorf(
			"cannot restore session %q because its origin repo %s is gone, and could not persist an unresolved archived worktree identity: %w",
			title, repoPath, persistErr,
		)
	}
	stagedClaim, err := instance.ClaimWorktreeRelocationForRetry()
	if err != nil {
		return fmt.Errorf(
			"cannot restore session %q because its origin repo is gone and the staged archived worktree identity could not be reclaimed: %w",
			title, err,
		)
	}
	if err := instance.PrepareWorktreeRelocationClaimForCleanup(stagedClaim); err != nil {
		prepareErr := fmt.Errorf(
			"cannot restore session %q because its origin repo is gone and the archived worktree identity could not be established: %w",
			title, err,
		)
		// Preparation replaces a consumed point-in-time claim with durable stale
		// evidence when the cleanup generation cannot be established. Persist that
		// fail-closed transition before returning; otherwise the previous on-disk
		// record could still be interpreted as an admissible retry after restart.
		if persistErr := m.persistInstanceErr(repoID, instance); persistErr != nil {
			return errors.Join(prepareErr, fmt.Errorf(
				"could not persist the unresolved archived worktree identity: %w", persistErr,
			))
		}
		return prepareErr
	}
	if err := m.persistInstanceErr(repoID, instance); err != nil {
		// The in-memory cleanup-ready record remains fail-closed. The previous
		// on-disk recovery record is conservative too.
		return fmt.Errorf(
			"cannot restore session %q because its origin repo %s is gone, and could not persist the resolved archived worktree identity: %w",
			title, repoPath, err,
		)
	}
	return nil
}

// persistRepoGoneAtRestoreUse handles the authoritative repo check immediately
// before the worktree move. The git layer has materialized claim_stale but no
// destructive authority; persist that fence before installing cleanup_ready.
func (m *Manager) persistRepoGoneAtRestoreUse(
	repoID, title, repoPath string, instance *session.Instance, restoreErr error,
) error {
	if err := m.persistAndInstallRepoGoneCleanup(repoID, title, repoPath, instance); err != nil {
		return errors.Join(err, restoreErr)
	}
	return fmt.Errorf(
		"cannot restore session %q: its origin repo is gone; the archived worktree is intact at %s — recover it manually with git: %w",
		title, instance.GetWorktreePath(), restoreErr,
	)
}

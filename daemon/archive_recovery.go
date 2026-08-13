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
	if err := m.prepareRepoGoneCleanup("restore", repoID, title, repoPath, instance, claim); err != nil {
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
// revalidate the same directory before deleting it. operation names the caller
// ("restore" or "kill") in every failure message.
func (m *Manager) prepareRepoGoneCleanup(
	operation, repoID, title, repoPath string,
	instance *session.Instance,
	claim sessiongit.RelocationClaim,
) error {
	// Stage a non-destructive fence before installing cleanup authority. If the
	// later cleanup_ready write fails, restart sees claim_stale rather than the
	// record-free archive that this transaction began with.
	instance.PreserveWorktreeRelocationClaimAsUnresolved(claim)
	return m.persistAndInstallRepoGoneCleanup(operation, repoID, title, repoPath, instance)
}

// persistAndInstallRepoGoneCleanup commits the two-phase cleanup transition.
// Its caller has already removed destructive authority by staging claim_stale;
// only after that fence is durable may cleanup_ready be installed and written.
func (m *Manager) persistAndInstallRepoGoneCleanup(
	operation, repoID, title, repoPath string,
	instance *session.Instance,
) error {
	if persistErr := m.persistInstanceErr(repoID, instance); persistErr != nil {
		return fmt.Errorf(
			"cannot %s session %q because its origin repo %s is gone, and could not persist an unresolved archived worktree identity: %w",
			operation, title, repoPath, persistErr,
		)
	}
	stagedClaim, err := instance.ClaimWorktreeRelocationForRetry()
	if err != nil {
		return fmt.Errorf(
			"cannot %s session %q because its origin repo is gone and the staged archived worktree identity could not be reclaimed: %w",
			operation, title, err,
		)
	}
	if err := instance.PrepareWorktreeRelocationClaimForCleanup(stagedClaim); err != nil {
		prepareErr := fmt.Errorf(
			"cannot %s session %q because its origin repo is gone and the archived worktree identity could not be established: %w",
			operation, title, err,
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
			"cannot %s session %q because its origin repo %s is gone, and could not persist the resolved archived worktree identity: %w",
			operation, title, repoPath, err,
		)
	}
	return nil
}

// prepareDirectRepoGoneKillCleanup is the admission producer for a kill that
// never went through a failed restore (#3176). The failed-restore route leaves
// a durable cleanup authorization behind for the destruction admission to
// validate; a direct kill of an archived session starts record-free, and the
// admission's "is a relocation unresolved?" question reads that absence as
// permission. Ordinary teardown then runs git cleanup against an origin that
// may be gone, its answered failures settle the kill, the row is deleted, and
// the archived directory — the only remaining handle to the user's work — is
// orphaned. Ask the positive question here, before the kill commits to
// anything: resolve the archived directory identity, probe the origin, and on a
// conclusive repo-gone answer — with the archive's own worktree pointer as
// creation-time evidence — capture and persist the same identity-qualified
// authorization a failed restore would have left, so teardown consumes the
// exact archived directory through the claimed repo-gone transaction. A probe
// that cannot answer refuses the kill; a present origin admits the ordinary
// kill untouched. A stalled identity fence from an earlier attempt is durable
// and reclaimed here on retry, so a transient stall never strands the session.
func (m *Manager) prepareDirectRepoGoneKillCleanup(
	repoID, title string, instance *session.Instance,
) error {
	if instance.GetLiveness() != session.LiveArchived || instance.IsExternalWorktree() ||
		instance.Capabilities().Workspace == session.WorkspaceRemote {
		return nil
	}
	archivedPath := instance.GetWorktreePath()
	if archivedPath == "" {
		// No local worktree means no archived directory to authorize; the
		// ordinary admission owns whatever remains. GetGitWorktree is not
		// usable here — it refuses while started is false, which an archived
		// instance always is.
		return nil
	}
	recovery, unresolved := instance.WorktreeRelocationRecovery()
	if unresolved && recovery.State != sessiongit.RelocationRecoveryStalled {
		// Any other lifecycle is owned by the destruction admission below: it
		// validates cleanup-ready records and refuses the rest. A stalled probe
		// record is this gate's own residue (or restore's), carries no
		// authority in either direction, and is exactly what a retry claim may
		// re-resolve — so fall through and reclaim it rather than leaving the
		// session unkillable in this process (#3278 review).
		return nil
	}
	repoPath := instance.GetRepoPath()
	if repoPath == "" {
		return fmt.Errorf(
			"cannot kill archived session %q: it has no repo path on record to establish its origin state; nothing was changed",
			title,
		)
	}
	claim, claimErr := instance.ClaimWorktreeRelocationForRetry()
	if claimErr != nil {
		wrapped := fmt.Errorf(
			"the archived worktree identity for session %q could not be resolved for cleanup authorization: %w",
			title, claimErr,
		)
		if errors.Is(claimErr, sessiongit.ErrRelocateStateUnknown) {
			// The failed bounded probe installed a stalled fence in memory.
			// Persist it so a crash cannot forget the evidence and a later
			// kill can reclaim it through the branch above (#3278 review).
			if persistErr := m.persistInstanceErr(repoID, instance); persistErr != nil {
				return errors.Join(wrapped, fmt.Errorf(
					"could not persist the stalled archived worktree identity: %w", persistErr,
				))
			}
		}
		return wrapped
	}
	probeErr := sessiongit.CheckRepoPresentForRelocation(repoPath)
	switch {
	case probeErr == nil:
		if !unresolved {
			// The claim came from record-free state and owns nothing; the
			// ordinary kill proceeds untouched.
			return nil
		}
		// The claim consumed the stalled record with a fresh identity answer.
		// Settle and persist that resolution so the ordinary kill below is
		// admitted instead of refused over a stall the origin has outlived.
		if settleErr := instance.SettleWorktreeRelocationClaim(claim); settleErr != nil {
			return fmt.Errorf(
				"origin repo %s answered present but the stalled archived worktree identity for session %q could not be settled: %w",
				repoPath, title, settleErr,
			)
		}
		if persistErr := m.persistInstanceErr(repoID, instance); persistErr != nil {
			return fmt.Errorf(
				"origin repo %s answered present but the settled archived worktree identity for session %q could not be persisted — retry the kill: %w",
				repoPath, title, persistErr,
			)
		}
		return nil
	case !errors.Is(probeErr, sessiongit.ErrRepoGone):
		instance.PreserveWorktreeRelocationClaimForRetry(claim)
		return fmt.Errorf(
			"cannot establish origin repo state for %s before killing archived session %q — retry once the origin can be probed: %w",
			repoPath, title, probeErr,
		)
	}
	// The origin is conclusively gone. Require the archive's own creation-time
	// evidence — its linked-worktree pointer — before authorizing deletion of
	// the current pathname occupant (#3278 review): a directory that replaced
	// the archive at the same path does not carry it.
	if pointerErr := sessiongit.VerifyArchivedWorktreePointer(archivedPath); pointerErr != nil {
		instance.PreserveWorktreeRelocationClaimForRetry(claim)
		return fmt.Errorf(
			"refusing to authorize repo-gone cleanup for session %q: %v — inspect %s manually before killing it",
			title, pointerErr, archivedPath,
		)
	}
	return m.prepareRepoGoneCleanup("kill", repoID, title, repoPath, instance, claim)
}

// persistRepoGoneAtRestoreUse handles the authoritative repo check immediately
// before the worktree move. The git layer has materialized claim_stale but no
// destructive authority; persist that fence before installing cleanup_ready.
func (m *Manager) persistRepoGoneAtRestoreUse(
	repoID, title, repoPath string, instance *session.Instance, restoreErr error,
) error {
	if err := m.persistAndInstallRepoGoneCleanup("restore", repoID, title, repoPath, instance); err != nil {
		return errors.Join(err, restoreErr)
	}
	return fmt.Errorf(
		"cannot restore session %q: its origin repo is gone; the archived worktree is intact at %s — recover it manually with git: %w",
		title, instance.GetWorktreePath(), restoreErr,
	)
}

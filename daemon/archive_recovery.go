package daemon

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

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

// settleRepoGoneRelocation is the non-moving completion path. Establish the
// selected directory identity once more, clear its active ownership, and persist
// both facts so a repo-gone archive remains eligible for an explicit kill.
func (m *Manager) settleRepoGoneRelocation(
	repoID, title, repoPath string,
	instance *session.Instance,
	claim sessiongit.RelocationClaim,
) error {
	if err := instance.SettleWorktreeRelocationClaimForRetry(claim); err != nil {
		return fmt.Errorf(
			"cannot restore session %q because its origin repo is gone and the archived worktree identity could not be established: %w",
			title, err,
		)
	}
	if err := m.persistInstanceErr(repoID, instance); err != nil {
		// Keep in-memory admission fail-closed if the resolved lifecycle could not
		// be written. The previous on-disk record remains conservative.
		instance.PreserveWorktreeRelocationClaimForRetry(claim)
		return fmt.Errorf(
			"cannot restore session %q because its origin repo %s is gone, and could not persist the resolved archived worktree identity: %w",
			title, repoPath, err,
		)
	}
	return nil
}

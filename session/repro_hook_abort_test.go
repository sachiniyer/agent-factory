package session

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArchiveTeardown_HookAbortPreservesConsumedRecoveryClaim is the hook-abort
// analog of TestArchiveTeardown_PaneAbortRestoresConsumedRecoveryClaim: it proves
// that when beforeMove returns ErrHookTeardownUnconfirmed (a safety refusal to
// relocate, added in #3650), a recovery-owned claim consumed from a pre-existing
// MoveUnknown record is rematerialized via PreserveWorktreeRelocationClaimForRetry
// — so a same-process archive/kill retry can resolve it instead of wedging for
// the life of the daemon process.
//
// The pane-abort sibling proves this property when closeTab reports a possibly-live
// pane (the gate fires before handleWorktree runs). This test closes that gap for
// the OTHER early return in handleWorktree: panes are confirmed dead, the flow
// reaches the hook boundary, and the hook refuses with ErrHookTeardownUnconfirmed.
//
// Before the fix, *m.claimHandled was set to true at the TOP of the m.claim block,
// before this early return. The caller then skipped PreserveWorktreeRelocationClaimForRetry,
// leaking the in-memory activeRelocationClaim. A same-process retry's
// ClaimRelocationSource then refused with "claim already in use" / ErrRelocateStateUnknown
// (worktree_recovery.go:327-330), and kill refused because RelocationSnapshot
// reported an unresolved ClaimStale derived from the leaked active claim. Both
// paths stayed wedged until a daemon restart rehydrated the persisted record.
//
// The fix moves *m.claimHandled = true to immediately before ArchiveWorktreeWithClaim
// (the point where the claim is actually settled/consumed), so every earlier return
// — including this hook-abort — leaves claimHandled == false and the caller
// re-preserves the claim exactly as the contract comment at teardown.go:821-825
// already promised.
func TestArchiveTeardown_HookAbortPreservesConsumedRecoveryClaim(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "worktree")
	require.NoError(t, os.Mkdir(source, 0o755))
	info, err := os.Stat(source)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	gw, err := git.NewGitWorktreeFromStorage(
		root, source, "hook-abort", "af/hook-abort", "", false, true,
	)
	require.NoError(t, err)
	require.NoError(t, gw.RestoreRelocationRecovery(git.RelocationRecovery{
		State:         git.RelocationRecoveryMoveUnknown,
		AlternatePath: filepath.Join(root, "old-candidate"),
		IdentityKnown: true,
		Device:        uint64(stat.Dev),
		Inode:         uint64(stat.Ino),
		FileType:      uint32(stat.Mode & syscall.S_IFMT),
	}))
	// Consume the durable record: recoveryOwned == true, g.activeRelocationClaim
	// set, g.relocationRecovery == nil.
	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err)

	// Panes are confirmed dead so the gate passes and handleWorktree runs,
	// reaching the hook boundary where ErrHookTeardownUnconfirmed fires.
	previousClose := archiveCloseTab
	archiveCloseTab = func(_ *tmux.TmuxSession, _, _ string) (teardownState, bool, error) {
		return stateKnown, false, nil
	}
	t.Cleanup(func() { archiveCloseTab = previousClose })

	inst := instanceWithTmuxTab(t, &tmux.TmuxSession{})
	inst.gitWorktree = gw
	beforeMove := func() error { return ErrHookTeardownUnconfirmed }

	_, archiveErr := inst.ArchiveTeardownWithClaim(t.TempDir(), claim, beforeMove, false)
	require.ErrorIs(t, archiveErr, ErrHookTeardownUnconfirmed,
		"the hook-abort safety refusal must surface as the archive error")

	// The consumed recovery claim must be rematerialized: with the fix,
	// PreserveWorktreeRelocationClaimForRetry ran (claimHandled was false) and
	// PreserveRelocationClaim reinstalled a ClaimStale record from the claim,
	// then released the active claim.
	recovery, retained := gw.GetRelocationRecovery()
	require.True(t, retained,
		"a recovery-owned claim consumed from a MoveUnknown record must be "+
			"rematerialized when the hook aborts before the move — absence would "+
			"let a later retry read 'no recovery' as permission to act freely")
	assert.True(t, recovery.IdentityKnown, "the rematerialized record must carry identity")
	assert.Equal(t, filepath.Join(root, "old-candidate"), recovery.AlternatePath,
		"the rematerialized record must preserve the original alternate candidate")

	// THE WEDGE: with the bug, g.activeRelocationClaim leaked (claimHandled was
	// set before the hook-abort early return), so the next ClaimRelocationSource
	// refuses with "claim already in use" / ErrRelocateStateUnknown. With the fix,
	// the active claim was released by PreserveRelocationClaim and the
	// rematerialized ClaimStale record resolves normally.
	retryClaim, retryErr := gw.ClaimRelocationSource()
	require.NoError(t, retryErr,
		"a same-process retry must not wedge on a leaked active relocation claim — "+
			"the active claim must have been released so the rematerialized record resolves")
	assert.Equal(t, source, retryClaim.Path,
		"the retry must resolve the primary candidate whose identity still matches")

	// Kill is wedged for the same reason when the active claim leaks: kill admission
	// (ValidateWorktreeDestructionAdmission) reads RelocationSnapshot, which projects
	// an active claim back as an unresolved ClaimStale. After the fix's retry resolved
	// the record, the retry claim is active; settle it to clear the relocation, then
	// confirm kill admission passes — proving the wedge is gone end-to-end.
	require.NoError(t, gw.SettleRelocationClaim(retryClaim))
	_, _, unresolved := gw.RelocationSnapshot()
	require.False(t, unresolved, "settling the retry claim must clear the relocation so kill can proceed")
}

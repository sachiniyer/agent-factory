package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArchiveTeardown_RevalidationFailureSelfHeals_OutOfBand is the symmetric
// CONTROL for the other early return in handleWorktree — the revalidation-failure
// return (teardown.go:831-832), which sits at the same site the fix moved
// *m.claimHandled out of. It proves the fix is scoped to the hook-abort path only:
// the revalidation-failure path self-heals internally (recordStaleClaimLocked
// installs a ClaimStale record AND releases the active claim), so the flag
// placement cannot change its recovery state.
//
// This test PASSES on BOTH the unfixed code (claimHandled == true, preserve skipped)
// AND the fixed code (claimHandled == false, preserve runs but takes the
// no-op branch because g.relocationRecovery is already set). Were the fix to
// regress this path — by double-recording, re-arming an already-released claim, or
// overwriting the heal-installed ClaimStale — this test would catch it.
//
// The composition mirrors the hook-abort repro: install + consume a MoveUnknown
// record (recoveryOwned claim, active claim set, recovery record cleared), then
// mutate the worktree path identity out-of-band so RevalidateRelocationClaim
// fails at the use boundary. The self-heal must leave the worktree in a
// recoverable state, and a same-process retry must NOT wedge with the hook-abort
// signature ("claim already in use").
func TestArchiveTeardown_RevalidationFailureSelfHeals_OutOfBand(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "worktree")
	require.NoError(t, os.Mkdir(source, 0o755))
	info, err := os.Stat(source)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	gw, err := git.NewGitWorktreeFromStorage(
		root, source, "reval-fail", "af/reval-fail", "", false, true,
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
	claim, err := gw.ClaimRelocationSource()
	require.NoError(t, err)

	// Mutate the worktree identity out-of-band: rename the original directory
	// aside (keeping its inode allocated — RemoveAll+Mkdir can reuse the same
	// inode on filesystems like overlayfs, which would let revalidation match the
	// recorded identity and skip the heal) and create a fresh replacement so it
	// gets a new inode. RevalidateRelocationClaim will see the new identity, find
	// it does not match the claim's recorded identity, and self-heal via
	// recordStaleClaimLocked (installs ClaimStale, releases the active claim).
	mutatedAside := filepath.Join(root, "worktree-mutated-aside")
	require.NoError(t, os.Rename(source, mutatedAside))
	require.NoError(t, os.Mkdir(source, 0o755))

	// Panes confirmed dead so the flow reaches RevalidateRelocationClaim. No
	// beforeMove hook is needed: the revalidation failure fires first.
	previousClose := archiveCloseTab
	archiveCloseTab = func(_ *tmux.TmuxSession, _, _ string) (teardownState, bool, error) {
		return stateKnown, false, nil
	}
	t.Cleanup(func() { archiveCloseTab = previousClose })

	inst := instanceWithTmuxTab(t, &tmux.TmuxSession{})
	inst.gitWorktree = gw

	_, archiveErr := inst.ArchiveTeardownWithClaim(t.TempDir(), claim, nil, false)
	require.ErrorIs(t, archiveErr, git.ErrRelocateStateUnknown,
		"the revalidation failure must surface as an unestablished relocation")

	// The self-heal must have installed a ClaimStale record (recordStaleClaimLocked)
	// and released the active claim, regardless of where claimHandled sits.
	recovery, retained := gw.GetRelocationRecovery()
	require.True(t, retained,
		"the revalidation failure must self-heal into a durable recovery record")
	assert.Equal(t, git.RelocationRecoveryClaimStale, recovery.State,
		"the heal must install a ClaimStale record so the worktree stays recoverable")
	assert.True(t, recovery.IdentityKnown, "the heal must preserve the recorded identity")

	// The same-process retry must NOT wedge with the hook-abort signature. With
	// the bug OR the fix, the active claim was released by the self-heal, so
	// ClaimRelocationSource does not refuse with "already in use". It fails only to
	// RESOLVE the deliberately-mutated worktree (neither candidate matches the
	// recorded identity), which is the expected pre- and post-fix behavior.
	retryClaim, retryErr := gw.ClaimRelocationSource()
	if retryErr == nil {
		// If resolution somehow succeeded (alternate matched), the claim is active.
		// Settle it to confirm no leak — this branch is not expected with a mutated
		// primary and a nonexistent alternate, but is tolerated defensively.
		require.NoError(t, gw.SettleRelocationClaim(retryClaim))
	} else {
		require.True(t, errors.Is(retryErr, git.ErrRelocateStateUnknown),
			"the retry failure must be an unestablished relocation, not 'already in use'")
		require.False(t, strings.Contains(retryErr.Error(), "already in use"),
			"the retry must not wedge on a leaked active claim (the hook-abort signature); "+
				"the self-heal must have released it — the failure must be a resolve failure only")
	}
}

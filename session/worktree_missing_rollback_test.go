package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
)

// A binary that predates the worktree-missing flag ignores it, so a flagged row
// must reach it through the relocation-recovery record it already honors: one
// that can never resolve, over the row's inert ownership projection (#5102).
func TestForStorage_MissingRowProjectsARollbackFence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt")
	inst := worktreeMissingInstance(t, path)
	inst.SetWorktreeMissing(WorktreeMissingDeletedReason(path))

	stored := inst.ToInstanceData().ForStorage()
	recovery := stored.Worktree.RelocationRecovery
	require.NotNil(t, recovery, "a missing row must carry a recovery fence for older readers")
	assert.Equal(t, git.RelocationRecoveryClaimStale, recovery.State)
	assert.True(t, recovery.IdentityKnown)
	assert.True(t, stored.StartupStateUnknown, "the pre-recovery release loads it inert")
	assert.True(t, stored.Worktree.ExternalWorktree, "and treats the checkout as user-owned")
	require.NotNil(t, stored.Worktree.BranchCreatedByUs)
	assert.False(t, *stored.Worktree.BranchCreatedByUs, "and cannot delete its branch")
	assert.True(t, stored.Worktree.Missing, "current readers still get the flag directly")

	// The previous release's own resolver refuses the record whether the path
	// is absent or a directory reappeared there.
	for _, present := range []bool{false, true} {
		if present {
			require.NoError(t, os.MkdirAll(path, 0o755))
		}
		gw, err := git.NewGitWorktreeFromStorage(filepath.Dir(path), path, "wt-missing", "af/wt-missing", "", false, true)
		require.NoError(t, err)
		require.NoError(t, gw.RestoreRelocationRecovery(git.RelocationRecovery{
			State: recovery.State, IdentityKnown: recovery.IdentityKnown,
			Device: recovery.Device, Inode: recovery.Inode, FileType: recovery.FileType,
		}), "an older reader must accept the record")
		_, claimErr := gw.ClaimRelocationSource()
		require.Error(t, claimErr, "present=%v", present)
		assert.True(t, errors.Is(claimErr, git.ErrRelocateStateUnknown), "present=%v: %v", present, claimErr)
		assert.True(t, gw.HasUnresolvedRelocation(), "present=%v: the record must keep fencing", present)
	}
}

// Current readers turn the projection back into the flag with the row's real
// ownership — including after an older binary rewrote the row and dropped the
// flag and its reason, keeping only the recovery record it understands.
func TestRestoreRelocationRecoveryOriginals_RecoversTheMissingFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt")
	inst := worktreeMissingInstance(t, path)
	reason := WorktreeMissingDeletedReason(path)
	inst.SetWorktreeMissing(reason)
	payload, err := json.Marshal(inst.ToInstanceData().ForStorage())
	require.NoError(t, err)

	t.Run("current writer", func(t *testing.T) {
		var data InstanceData
		require.NoError(t, json.Unmarshal(payload, &data))
		restored, err := data.RestoreRelocationRecoveryOriginals()
		require.NoError(t, err)
		assert.Nil(t, restored.Worktree.RelocationRecovery, "the projection is not a relocation record")
		assert.True(t, restored.Worktree.Missing)
		assert.Equal(t, reason, restored.Worktree.MissingReason)
		assert.False(t, restored.Worktree.ExternalWorktree)
		require.NotNil(t, restored.Worktree.BranchCreatedByUs)
		assert.True(t, *restored.Worktree.BranchCreatedByUs)
		assert.False(t, restored.StartupStateUnknown)
	})

	t.Run("rewritten by the previous release", func(t *testing.T) {
		var previous v10244InstanceData
		require.NoError(t, json.Unmarshal(payload, &previous))
		rewritten, err := json.Marshal(previous)
		require.NoError(t, err)
		require.NotContains(t, string(rewritten), `"missing"`, "premise: the older writer drops the flag")

		var data InstanceData
		require.NoError(t, json.Unmarshal(rewritten, &data))
		restored, err := data.RestoreRelocationRecoveryOriginals()
		require.NoError(t, err)
		assert.Nil(t, restored.Worktree.RelocationRecovery)
		assert.True(t, restored.Worktree.Missing, "the record's shape alone must bring the flag back")
		assert.Equal(t, WorktreeMissingDeletedReason(path), restored.Worktree.MissingReason)
		assert.False(t, restored.Worktree.ExternalWorktree)
	})

	t.Run("loaded instance", func(t *testing.T) {
		var data InstanceData
		require.NoError(t, json.Unmarshal(payload, &data))
		loaded, err := FromInstanceData(data)
		require.NoError(t, err)
		missing, _ := loaded.WorktreeMissing()
		assert.True(t, missing)
		gw, err := loaded.GetGitWorktree()
		require.NoError(t, err)
		assert.False(t, gw.HasUnresolvedRelocation(), "the projection must not load as a relocation fence")
	})
}

// A real recovery record on a missing row is left alone: it already fences the
// row for every reader, and is not the projection's shape.
func TestForStorage_MissingRowKeepsARealRecoveryRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt")
	inst := worktreeMissingInstance(t, path)
	inst.SetWorktreeMissing(WorktreeMissingDeletedReason(path))
	_ = inst.gitWorktree.FenceUnverifiedWorktree(errors.New("test"))

	stored := inst.ToInstanceData().ForStorage()
	require.NotNil(t, stored.Worktree.RelocationRecovery)
	assert.Equal(t, git.RelocationRecoveryStalled, stored.Worktree.RelocationRecovery.State)
	restored, err := stored.RestoreRelocationRecoveryOriginals()
	require.NoError(t, err)
	require.NotNil(t, restored.Worktree.RelocationRecovery, "a real stall must survive the round trip")
	assert.True(t, restored.Worktree.Missing)
}

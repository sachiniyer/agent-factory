package session

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archiveRetainedTreeForFenceTest is a non-empty retained tree, which is what
// makes ArchiveReport.Empty() false and so routes a row into the recapture
// branch of ForStorage. The path/identity fields are inert here: no test in
// this file touches the filesystem, only the in-memory projections.
func archiveRetainedTreeForFenceTest() git.ArchiveRetainedTree {
	return git.ArchiveRetainedTree{
		Path:          "/worktrees/.af-source-0123456789abcdef0123456789abcdef",
		IdentityKnown: true, Device: 1, Inode: 2, FileType: 0o040000,
		Skipped: []git.ArchiveSkippedEntry{{
			Path: "private/credential", Reason: git.ArchiveSkipPermissionDenied,
		}},
	}
}

// nonEmptyArchiveReport builds a non-empty ArchiveReport with a cleared
// RollbackFence — the exact shape the ghost-cleanup persist path produces
// (RestoreArchiveRollbackFence clears the originals, then the path sets
// RollbackFence = nil to force the recapture branch in ForStorage).
func nonEmptyArchiveReport() *git.ArchiveReport {
	return &git.ArchiveReport{
		RetainedTrees: []git.ArchiveRetainedTree{archiveRetainedTreeForFenceTest()},
		// RollbackFence deliberately nil: the ghost path clears it so the
		// recapture branch in ForStorage fires.
	}
}

// ghostSwapArchiveReport is the disk-reload fixture the bug fires on: a
// non-empty ArchiveReport whose RollbackFence was cleared, a pending account
// swap, and the row's true StartupStateUnknown = false. archiveReportSource is
// left nil because a private field never deserializes off disk, which is exactly
// what routes the row onto the buggy recapture path in ForStorage.
func ghostSwapArchiveReport() InstanceData {
	branchCreatedByUs := true
	return InstanceData{
		ID:                  "ghost-swap-id",
		Title:               "ghost-swap",
		Program:             "claude",
		Status:              Archived,
		Liveness:            LiveArchived,
		StartupStateUnknown: false,
		Worktree: GitWorktreeData{
			ExternalWorktree:  false,
			BranchCreatedByUs: &branchCreatedByUs,
		},
		PendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal",
			ReplacementPanesStarted: true,
			MissionDeliveryStatus:   PromptCouldNotConfirm,
		},
		ArchiveReport: nonEmptyArchiveReport(),
	}
}

// ghostHandoffArchiveReport is the equivalent trigger via the pending-handoff
// projection instead of the pending-account-swap projection: an ambiguous
// pending mission makes projectPendingHandoffForPreviousRelease overwrite
// StartupStateUnknown = true before the recapture. The fix must precede BOTH
// projectors; this fixture exercises the second one.
func ghostHandoffArchiveReport() InstanceData {
	branchCreatedByUs := true
	return InstanceData{
		ID:                  "ghost-handoff-id",
		Title:               "ghost-handoff",
		Program:             "claude",
		Status:              Archived,
		Liveness:            LiveArchived,
		StartupStateUnknown: false,
		Worktree: GitWorktreeData{
			ExternalWorktree:  false,
			BranchCreatedByUs: &branchCreatedByUs,
		},
		PendingHandoffMission: "continue the inherited work",
		HandoffDeliveryStatus: PromptCouldNotConfirm,
		ArchiveReport:         nonEmptyArchiveReport(),
	}
}

// TestForStorageArchiveRollbackFenceCapturesPreProjectionStartupState is the
// core regression for the recapture-after-projection bug. On the disk-reload
// path, ForStorage must snapshot the row's true pre-projection
// StartupStateUnknown into ArchiveReport.RollbackFence.OriginalStartupStateUnknown,
// not the value projectPendingAccountSwapForPreviousRelease overwrote it with.
func TestForStorageArchiveRollbackFenceCapturesPreProjectionStartupState(t *testing.T) {
	stored := ghostSwapArchiveReport().ForStorage()

	require.NotNil(t, stored.ArchiveReport)
	require.NotNil(t, stored.ArchiveReport.RollbackFence,
		"the recapture branch must install a fence when the ghost path cleared it")

	// The on-disk safety projection is unchanged by the fix: the row is still
	// persisted inert for older binaries.
	assert.True(t, stored.StartupStateUnknown,
		"the row's own StartupStateUnknown must still project to true for older readers")
	assert.True(t, stored.Worktree.ExternalWorktree)
	require.NotNil(t, stored.Worktree.BranchCreatedByUs)
	assert.False(t, *stored.Worktree.BranchCreatedByUs)

	// The fence must carry the row's TRUE pre-projection ownership, not the
	// projected values. Before the fix the recapture ran after
	// projectPendingAccountSwapForPreviousRelease set StartupStateUnknown = true,
	// so the fence recorded the projected true as the "original".
	fence := stored.ArchiveReport.RollbackFence
	assert.False(t, fence.OriginalStartupStateUnknown,
		"fence must capture the true original StartupStateUnknown (false), not the projected true")
	assert.False(t, fence.OriginalExternalWorktree,
		"fence must capture the true original ExternalWorktree")
	require.NotNil(t, fence.OriginalBranchCreatedByUs)
	assert.True(t, *fence.OriginalBranchCreatedByUs,
		"fence must capture the true original BranchCreatedByUs")

	// The account-swap fence preserves the true original independently; the two
	// fences must agree after the fix. Before the fix they disagreed: the
	// archive-rollback fence carried true while the swap fence carried false.
	require.NotNil(t, stored.PendingAccountSwap)
	require.NotNil(t, stored.PendingAccountSwap.OriginalStartupStateUnknown)
	assert.False(t, *stored.PendingAccountSwap.OriginalStartupStateUnknown,
		"the swap fence stashes the true original")
	assert.Equal(t,
		fence.OriginalStartupStateUnknown,
		*stored.PendingAccountSwap.OriginalStartupStateUnknown,
		"the archive-rollback and account-swap rollback fences must agree on the original startup state")
}

// TestForStorageArchiveRollbackFenceCapturesPreProjectionStartupStateHandoffPath
// exercises the equivalent trigger via projectPendingHandoffForPreviousRelease.
// The fix must precede both projectors; an ambiguous pending mission must not
// poison the archive rollback fence either.
func TestForStorageArchiveRollbackFenceCapturesPreProjectionStartupStateHandoffPath(t *testing.T) {
	stored := ghostHandoffArchiveReport().ForStorage()

	require.NotNil(t, stored.ArchiveReport)
	require.NotNil(t, stored.ArchiveReport.RollbackFence)
	assert.True(t, stored.StartupStateUnknown,
		"the row's own StartupStateUnknown must still project to true for older readers")
	assert.False(t, stored.ArchiveReport.RollbackFence.OriginalStartupStateUnknown,
		"the handoff projection must not poison the archive fence's original startup state")

	// The handoff fence also carries the true original and must agree.
	require.NotNil(t, stored.HandoffOriginalStartupStateUnknown)
	assert.False(t, *stored.HandoffOriginalStartupStateUnknown,
		"the handoff fence stashes the true original")
	assert.Equal(t,
		stored.ArchiveReport.RollbackFence.OriginalStartupStateUnknown,
		*stored.HandoffOriginalStartupStateUnknown,
		"the archive-rollback and handoff rollback fences must agree on the original startup state")
}

// TestForStorageKillFencePropagatesTrueOriginalStartupState asserts the fix
// also corrects the synthetic relocation-recovery kill fence, whose
// OriginalStartupStateUnknown is copied from ArchiveReport.RollbackFence by
// archiveReportKillFence. Before the fix it inherited the projected true.
func TestForStorageKillFencePropagatesTrueOriginalStartupState(t *testing.T) {
	stored := ghostSwapArchiveReport().ForStorage()

	require.NotNil(t, stored.Worktree.RelocationRecovery,
		"a non-empty ArchiveReport installs a relocation-recovery kill fence")
	recovery := stored.Worktree.RelocationRecovery
	require.NotNil(t, recovery.OriginalStartupStateUnknown)
	assert.False(t, *recovery.OriginalStartupStateUnknown,
		"the kill fence must inherit the true original startup state, not the projected value")
	require.NotNil(t, recovery.OriginalExternalWorktree)
	assert.False(t, *recovery.OriginalExternalWorktree)
	require.NotNil(t, recovery.OriginalBranchCreatedByUs)
	assert.True(t, *recovery.OriginalBranchCreatedByUs)
}

// TestRestoreArchiveRollbackFenceRecoversTrueStartupStateOnGhostPath asserts the
// fence's stated purpose: RestoreArchiveRollbackFence — called by the
// ghost-cleanup paths WITHOUT RestoreAccountSwapRollbackFence — must recover
// the row's true StartupStateUnknown. Before the fix it restored the poisoned
// true, defeating the fence on the only path that could have used it.
func TestRestoreArchiveRollbackFenceRecoversTrueStartupStateOnGhostPath(t *testing.T) {
	stored := ghostSwapArchiveReport().ForStorage()

	restored := stored.RestoreArchiveRollbackFence()
	assert.False(t, restored.StartupStateUnknown,
		"RestoreArchiveRollbackFence alone must recover the true original on the ghost path")
	assert.False(t, restored.Worktree.ExternalWorktree)
	require.NotNil(t, restored.Worktree.BranchCreatedByUs)
	assert.True(t, *restored.Worktree.BranchCreatedByUs)
}

// TestNoSwapAmbiguousHandoffFenceRecoveredOnlyByArchiveFence pins the shape that
// made the bug latent-and-dangerous: with no PendingAccountSwap on the row,
// RestoreAccountSwapRollbackFence is a no-op, so the ONLY correction of
// StartupStateUnknown is RestoreArchiveRollbackFence. Before the fix that
// restore wrote the poisoned true back; after the fix it writes the true false.
// The ambiguous handoff is what triggers a projection without a swap, so the
// archive fence is the row's sole carrier of the original.
func TestNoSwapAmbiguousHandoffFenceRecoveredOnlyByArchiveFence(t *testing.T) {
	stored := ghostHandoffArchiveReport().ForStorage()
	require.False(t, stored.ArchiveReport.RollbackFence.OriginalStartupStateUnknown,
		"the archive fence is the sole carrier of the original with no swap")

	// Swap restore is a no-op with no swap, so it must NOT correct the value.
	swapRestored := stored.RestoreAccountSwapRollbackFence()
	assert.True(t, swapRestored.StartupStateUnknown,
		"with no swap, swap restore is a no-op and the on-disk safety projection survives")

	// Only the archive fence can recover the original on a no-swap row.
	archiveRestored := stored.RestoreArchiveRollbackFence()
	assert.False(t, archiveRestored.StartupStateUnknown,
		"without a swap fence, only the archive fence can recover the original — it must be correct")
}

// TestForStoragePreservesExistingRollbackFenceOnDiskPath guards the other
// branch of the recapture: when the disk-reload row already carries a
// RollbackFence (the ghost path did NOT clear it), the recapture must leave it
// untouched. The fix's pre-projection snapshot is only consumed when the fence
// was cleared, so an existing fence survives verbatim.
func TestForStoragePreservesExistingRollbackFenceOnDiskPath(t *testing.T) {
	original := true
	existing := &git.ArchiveRollbackFence{
		OriginalStartupStateUnknown: false,
		OriginalExternalWorktree:    true,
		OriginalBranchCreatedByUs:   &original,
		RelocationRecoveryProjected: true,
	}
	data := ghostSwapArchiveReport()
	data.ArchiveReport = &git.ArchiveReport{
		RetainedTrees: []git.ArchiveRetainedTree{archiveRetainedTreeForFenceTest()},
		RollbackFence: existing,
	}

	stored := data.ForStorage()
	require.NotNil(t, stored.ArchiveReport)
	require.NotNil(t, stored.ArchiveReport.RollbackFence)
	assert.Equal(t, false, stored.ArchiveReport.RollbackFence.OriginalStartupStateUnknown,
		"an existing fence must be preserved, not recaptured")
	assert.True(t, stored.ArchiveReport.RollbackFence.OriginalExternalWorktree)
	require.NotNil(t, stored.ArchiveReport.RollbackFence.OriginalBranchCreatedByUs)
	assert.True(t, *stored.ArchiveReport.RollbackFence.OriginalBranchCreatedByUs)
}

// TestForStorageRecaptureIdempotentAcrossGhostRepersist confirms a correctly
// stored row stays correct across the self-perpetuating ghost re-persist cycle:
// reload (RestoreArchiveRollbackFence) -> clear the fence -> ForStorage. This is
// the cycle a ghost row repeats for the rest of its life; the fix must keep the
// fence correct on every iteration, not just the first.
func TestForStorageRecaptureIdempotentAcrossGhostRepersist(t *testing.T) {
	stored := ghostSwapArchiveReport().ForStorage()
	require.False(t, stored.ArchiveReport.RollbackFence.OriginalStartupStateUnknown,
		"first persist must produce a correct fence")

	// Simulate the ghost-cleanup reload: restore the originals, then clear the
	// fence exactly as ghostCleanupWorktree / projectGhostPersistenceSnapshot do.
	reloaded := stored.RestoreArchiveRollbackFence()
	assert.False(t, reloaded.StartupStateUnknown,
		"the correct fence restores the true original on reload")
	report := reloaded.ArchiveReport.Clone()
	report.RollbackFence = nil
	reloaded.ArchiveReport = &report

	repersisted := reloaded.ForStorage()
	require.NotNil(t, repersisted.ArchiveReport.RollbackFence)
	assert.False(t, repersisted.ArchiveReport.RollbackFence.OriginalStartupStateUnknown,
		"a correctly-stored row must stay correct across the ghost re-persist cycle")
	// The two fences must still agree after a second persist.
	require.NotNil(t, repersisted.PendingAccountSwap)
	require.NotNil(t, repersisted.PendingAccountSwap.OriginalStartupStateUnknown)
	assert.Equal(t,
		repersisted.ArchiveReport.RollbackFence.OriginalStartupStateUnknown,
		*repersisted.PendingAccountSwap.OriginalStartupStateUnknown)
}

// TestForStorageArchiveRollbackFenceSurvivesJSONRoundTrip confirms the corrected
// fence is durable on the wire: the bug is about persisted rows, so the
// OriginalStartupStateUnknown the fix captures must survive marshal/unmarshal
// and not silently round-trip back to the projected value.
func TestForStorageArchiveRollbackFenceSurvivesJSONRoundTrip(t *testing.T) {
	stored := ghostSwapArchiveReport().ForStorage()
	require.False(t, stored.ArchiveReport.RollbackFence.OriginalStartupStateUnknown)

	payload, err := json.Marshal(stored)
	require.NoError(t, err)
	var reloaded InstanceData
	require.NoError(t, json.Unmarshal(payload, &reloaded))
	require.NotNil(t, reloaded.ArchiveReport)
	require.NotNil(t, reloaded.ArchiveReport.RollbackFence)
	assert.False(t, reloaded.ArchiveReport.RollbackFence.OriginalStartupStateUnknown,
		"the corrected fence must be durable across JSON marshal/unmarshal")
	// An empty ArchiveReport is never produced from a non-empty one.
	require.False(t, reloaded.ArchiveReport.Empty())
}

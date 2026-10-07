package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPersistInstanceData_GhostSwapArchiveFenceCarriesTrueOriginal is the daemon-side
// end-to-end regression for the archive-rollback-fence recapture bug. It feeds the
// ghost-cleanup persist shape (a disk-reloaded row: PendingAccountSwap + non-empty
// ArchiveReport with a CLEARED RollbackFence, the exact shape
// ghostCleanupWorktree/projectGhostPersistenceSnapshot produce) through the real
// persistInstanceData -> ForStorage disk-write path, then reads the row back from
// disk and asserts the fence carries the row's true pre-projection StartupStateUnknown.
//
// Before the fix, ForStorage recaptured the fence AFTER projectPendingAccountSwap
// overwrote StartupStateUnknown=true, so the persisted fence recorded the projected
// true as the "original"; reading it back through RestoreArchiveRollbackFence (the
// only restore a ghost path applies) restored the poisoned true.
func TestPersistInstanceData_GhostSwapArchiveFenceCarriesTrueOriginal(t *testing.T) {
	manager, repoID, _ := newStatusTestManager(t)

	branchCreatedByUs := true
	// ghostRow is the disk-reload shape the ghost-cleanup path re-persists: a row
	// carrying a non-empty ArchiveReport whose RollbackFence has been CLEARED
	// (exactly what ghostCleanupWorktree / projectGhostPersistenceSnapshot do to
	// force the recapture branch in ForStorage), plus a pending account swap whose
	// projection overwrites StartupStateUnknown before the recapture.
	ghostRow := func() session.InstanceData {
		return session.InstanceData{
			ID:                  "ghost-swap-e2e-id",
			Title:               "ghost-swap-e2e",
			Program:             "claude",
			Status:              session.Archived,
			Liveness:            session.LiveArchived,
			StartupStateUnknown: false,
			Worktree: session.GitWorktreeData{
				ExternalWorktree:  false,
				BranchCreatedByUs: &branchCreatedByUs,
			},
			PendingAccountSwap: &session.AccountSwapData{
				Manual: true, From: "work", To: "personal",
				ReplacementPanesStarted: true,
				MissionDeliveryStatus:   session.PromptCouldNotConfirm,
			},
			ArchiveReport: &git.ArchiveReport{
				RetainedTrees: []git.ArchiveRetainedTree{{
					Path:          "/worktrees/.af-source-0123456789abcdef0123456789abcdef",
					IdentityKnown: true, Device: 1, Inode: 2, FileType: 0o040000,
					Skipped: []git.ArchiveSkippedEntry{{
						Path: "private/credential", Reason: git.ArchiveSkipPermissionDenied,
					}},
				}},
				// RollbackFence deliberately nil: the ghost path clears it so the
				// recapture branch in ForStorage fires.
			},
		}
	}

	// Seed the store so persistInstanceData has a matching id row to update.
	require.NoError(t, appendInstanceData(repoID, ghostRow()))

	// Re-persist the cleared-fence ghost row — the daemon ghost-cleanup persist
	// path. persistInstanceData routes it through ForStorage with
	// archiveReportSource == nil (the buggy recapture path).
	require.NoError(t, persistInstanceData(repoID, ghostRow()))

	stored := persistedInstanceByTitle(t, repoID, "ghost-swap-e2e")
	require.NotNil(t, stored.ArchiveReport)
	require.NotNil(t, stored.ArchiveReport.RollbackFence,
		"the recapture branch must install a fence for the cleared-fence ghost row")
	// The fix's guarantee at the daemon persistence layer:
	assert.False(t, stored.ArchiveReport.RollbackFence.OriginalStartupStateUnknown,
		"persistInstanceData must persist the true pre-projection StartupStateUnknown, not the projected true")
	// The on-disk safety projection is unchanged.
	assert.True(t, stored.StartupStateUnknown, "older readers must still load the row inert")
	// The two fences must agree after the daemon persist.
	require.NotNil(t, stored.PendingAccountSwap)
	require.NotNil(t, stored.PendingAccountSwap.OriginalStartupStateUnknown)
	assert.Equal(t,
		stored.ArchiveReport.RollbackFence.OriginalStartupStateUnknown,
		*stored.PendingAccountSwap.OriginalStartupStateUnknown,
		"archive-rollback and account-swap rollback fences must agree after daemon persist")

	// RestoreArchiveRollbackFence alone (the only restore a ghost path applies) must
	// recover the true original — defeating the bug at the read boundary.
	restored := stored.RestoreArchiveRollbackFence()
	assert.False(t, restored.StartupStateUnknown,
		"the ghost read path must recover the true original from the corrected fence")

	_ = manager
}

package daemon

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// A restore whose new worktree location never reached disk must not report
// success (#2880).
//
// RestoreWorktreeTo moves the bytes and then calls setWorktreeLocation, which
// updates the path IN MEMORY. The write that follows is the only thing that
// carries it across a restart, and it used persistInstance — log and continue. A
// lost write plus an unclean exit therefore reloads the session Archived, bound
// to an archive directory that no longer exists, while the user's work sits at
// the restore path with nothing durable pointing at it. The next restore fails
// relocate's source-exists guard, and no poll repairs the divergence:
// persistPollChange writes on a liveness/reset change and this row's liveness is
// already settled.
//
// This is the same contract the cut-off-relocate branch in the same function
// already keeps (TestRestoreArchived_RelocateCutOffReportsAFailedPersist). A
// relocate that fully SUCCEEDED makes the new location more certain, not less.
func TestRestoreArchived_SuccessfulRelocateReportsAFailedPersist(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()
	require.Equal(t, archivedPath, recordFor(t, repoID, "worker").Worktree.WorktreePath,
		"precondition: the archive location is what is persisted going in")

	restored, perr := sessiongit.RestoreWorktreePath(repoPath, "worker", inst.GetBranch())
	require.NoError(t, perr)

	// Fail exactly the restore commit: the write that first carries the RESTORED
	// location, which is the one taken BEFORE the respawn. Everything before it —
	// the archive above, and the relocate's own git work — is real, so this cannot
	// be confused with a restore that never got that far.
	diskFull := errors.New("no space left on device")
	var mu sync.Mutex
	fired := false
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title != "worker" || data.Worktree.WorktreePath != restored {
			return nil
		}
		mu.Lock()
		fired = true
		mu.Unlock()
		return diskFull
	}

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})

	// Without this, the assertion below fails on the unfixed code whether or not
	// the seam ever matched — so a later change to what gets written (ForStorage,
	// a different commit point) could make the whole test vacuous once it is green.
	mu.Lock()
	injected := fired
	mu.Unlock()
	require.True(t, injected,
		"the restore commit was never attempted with the restored path, so this test exercised nothing")

	require.Error(t, err,
		"a restore whose new location was never written reported success: disk still says Archived at "+
			"an archive path that no longer exists, so a restart strands the worktree and every later "+
			"restore fails the source-exists guard")
	assert.ErrorContains(t, err, restored,
		"the error must name where the worktree actually is, so the operator can re-register it")
	assert.ErrorContains(t, err, diskFull.Error(),
		"the error must name the write failure, not just the outcome")

	// The premise: the bytes really did move, which is what makes a lost pointer
	// stranding rather than a harmless retry.
	assert.True(t, exists(restored), "premise: the relocate placed the worktree at the restore path")
	assert.False(t, exists(archivedPath), "premise: it is no longer at the archive path")

	// And the record still names the old location — the exact state a restarted
	// daemon would load. Nothing here can fix that; the error is the only signal
	// the operator gets, which is why it must not be swallowed.
	assert.Equal(t, archivedPath, recordFor(t, repoID, "worker").Worktree.WorktreePath,
		"premise: the failed write left the stale archive path on disk")
}

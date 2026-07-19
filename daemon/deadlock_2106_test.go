package daemon

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReserveCreate_ReusesArchivedName_DoubleFailureNoSelfDeadlock is the #2106
// regression.
//
// reserveCreate holds m.mu across its whole body and calls
// renameArchivedForReuseLocked under it. That function's double-failure recovery
// branch — the durable title rewrite fails AND the worktree rollback that should
// undo it also fails — used to call m.persistInstance, which reaches
// persistInstanceErr -> startLockForRepo -> m.mu.Lock(). Go's sync.Mutex is not
// reentrant, so the goroutine blocked on the manager lock it was already holding:
// the create never returned and every other daemon operation waiting on m.mu
// stalled behind it. A total daemon hang needing a process restart.
//
// Both failures are forced deterministically through the reuseArchivedRenamePersist
// seam, which fires in exactly the window between the two worktree moves:
//
//  1. the real first move has already relocated the archived worktree
//     origDest -> newDest, vacating origDest;
//  2. the seam re-occupies origDest and returns an error, so the durable rewrite
//     "fails";
//  3. the rollback move newDest -> origDest then fails on relocateWorktreeTo's
//     "destination already exists" guard (a plain stat, not a permission trick,
//     so it behaves identically for root and in a container).
//
// That lands the create on the recovery branch on the real production path
// (reserveCreate -> renameArchivedForReuseLocked), which is the point: calling the
// locking helper under m.mu in isolation would only re-prove that Go mutexes are
// not reentrant, not that this code path reaches it.
//
// The exercised call runs in a goroutine behind a select/timeout so a pre-fix run
// FAILS with a clear message instead of hanging the suite until the test binary
// panics.
func TestReserveCreate_ReusesArchivedName_DoubleFailureNoSelfDeadlock(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// Branch freed: this repro must REACH the rename to force its double failure,
	// and #2127's guard refuses before the rename whenever the archived session
	// still holds the branch the create derives. Guarding is not what this test
	// pins — the lock ordering inside the recovery branch is.
	archived, _ := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")

	// Where the archived worktree lives before the rename; the rollback move
	// targets exactly this path.
	origDest, err := archivedWorktreePath(repoID, "foo")
	require.NoError(t, err)
	newDest, err := archivedWorktreePath(repoID, "foo (archived)")
	require.NoError(t, err)
	require.True(t, exists(origDest), "the seeded archived worktree must start at the pre-rename path")

	errForcedPersist := fmt.Errorf("forced durable-rename failure (#2106 repro)")
	seamFired := false
	blockerPlanted := false
	prev := reuseArchivedRenamePersist
	reuseArchivedRenamePersist = func(rid, oldTitle string, newData session.InstanceData) error {
		seamFired = true
		// The first move has vacated origDest by now; re-occupying it is what makes
		// the rollback's dest-already-exists guard trip.
		if mkErr := os.MkdirAll(origDest, 0o755); mkErr == nil {
			blockerPlanted = true
		}
		return errForcedPersist
	}
	t.Cleanup(func() { reuseArchivedRenamePersist = prev })

	type outcome struct {
		err     error
		release func()
	}
	done := make(chan outcome, 1)
	go func() {
		_, _, release, _, rerr := manager.reserveCreate(CreateSessionRequest{
			RepoPath: repoPath,
			Title:    "foo",
			Program:  "claude",
		})
		done <- outcome{err: rerr, release: release}
	}()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock (#2106): reserveCreate never returned — the archived-name-reuse " +
			"double-failure recovery branch persists while holding m.mu, and persistInstance " +
			"re-acquires m.mu via startLockForRepo (sync.Mutex is not reentrant), hanging the daemon")
	}
	if got.release != nil {
		got.release()
	}

	// The harness must actually have produced the double failure; a repro that
	// silently stopped reaching the branch would pass for the wrong reason.
	require.True(t, seamFired, "the durable-rename seam never fired; the repro did not reach the rename path")
	require.True(t, blockerPlanted, "could not re-occupy the pre-rename archive path; the rollback would not have failed")
	require.True(t, exists(newDest), "the archived worktree must still be at the post-rename path — the rollback was forced to fail")

	// The create aborts, surfacing BOTH failures so the operator can recover the
	// archive by hand.
	require.Error(t, got.err, "the create must abort when the rename can neither be persisted nor rolled back")
	assert.ErrorIs(t, got.err, errForcedPersist, "the persist failure must be wrapped, not swallowed")
	assert.Contains(t, got.err.Error(), "may need manual recovery",
		"the error must tell the operator the archive was left needing recovery")

	// The archived row stays re-keyed under its new title: the bytes live at
	// newDest, so the manager map must agree with the filesystem.
	assert.Equal(t, "foo (archived)", archived.Title)
	manager.mu.Lock()
	_, oldKeyed := manager.instances[daemonInstanceKey(repoID, "foo")]
	_, newKeyed := manager.instances[daemonInstanceKey(repoID, "foo (archived)")]
	manager.mu.Unlock()
	assert.False(t, oldKeyed, "the archived row must not remain keyed under the un-freed name")
	assert.True(t, newKeyed, "the archived row must stay addressable under the name its worktree now uses")

	// m.mu must be usable afterwards. reserveCreate takes and releases it, so a
	// second create completing at all is the direct proof the manager lock was not
	// left wedged by the recovery branch.
	second := make(chan error, 1)
	go func() {
		_, _, release, _, rerr := manager.reserveCreate(CreateSessionRequest{
			RepoPath:  repoPath,
			TitleBase: "after-recovery",
			Program:   "claude",
		})
		if release != nil {
			release()
		}
		second <- rerr
	}()
	select {
	case rerr := <-second:
		assert.NoError(t, rerr, "a create after the recovery branch must still succeed")
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock (#2106): the manager lock was left held by the double-failure recovery branch — " +
			"a subsequent create can never acquire m.mu")
	}
}

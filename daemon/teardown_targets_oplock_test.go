package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/teardownreq"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// #7760: an archived-restore serialized ahead of a by-id kill relocates the
// worktree to RestoreWorktreePath before the kill's teardown, and the blocked
// requester's cwd follows the moved directory to the new path. The kill's
// writer reaper runs on THAT path, so the requester must be excused for it —
// a registered set derived only at handler entry names the pre-move archive
// path, and the kill reaps its own caller before its reply lands.
//
// The real interleaving this covers puts the move one step earlier than the
// staging below: the restore holds the session's op-lock AND its killsInFlight
// claim (claimRestoreOperation) while the kill's handler snapshots the entry
// targets at the archive path, and only when the restore unwinds —
// releaseRestoreOperation before opLock.Unlock, by the LIFO defer order in
// restoreArchivedInstance — does the kill claim and queue on the lock, then
// reap at the restored path. This test produces the same kill-side state by
// moving the tree BETWEEN the kill's claim and its lock acquisition — a point
// a real restore can never reach, because claimRestoreOperation refuses while
// killsInFlight is held. Either way the property under test is identical: at
// lock acquisition the registered set names a path the move already left.
func TestKillSession_RequesterSetFollowsTheOpLockClaim(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "restored-ahead")
	archivedPath, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "restored-ahead", RepoID: repoID})
	require.NoError(t, err)

	requester := proctree.Process{PID: 424253, StartID: 24}
	server := &controlServer{manager: manager, requester: &requester}

	// Hold the session's operation lock so the kill queues behind a peer —
	// the serialization a restore already has with a by-id kill of the same
	// session.
	key := daemonInstanceKey(repoID, "restored-ahead")
	releasePeer := holdOpLock(t, manager, key)

	killDone := make(chan error, 1)
	go func() {
		var resp KillSessionResponse
		killDone <- server.killSession(context.Background(),
			&KillSessionRequest{ID: inst.ID, RepoID: repoID}, &resp)
	}()
	// The kill resolves and claims killsInFlight BEFORE it waits on the
	// op-lock, so the claim's presence proves the handler-entry snapshot and
	// the by-id resolve both already ran — the window a serialized-ahead
	// operation can still relocate into.
	require.Eventually(t, func() bool { return killGuardHeld(manager, key) },
		30*time.Second, 5*time.Millisecond)

	// The serialized-ahead operation: the restore's move commits on the same
	// instance while the kill is queued — the kill-side state the real
	// interleaving leaves (see the doc comment), reached here by moving the
	// tree directly because a real restore could not claim past the kill's
	// killsInFlight registration.
	restoreDest, err := sessiongit.RestoreWorktreePath(repoPath, "restored-ahead", inst.GetBranch())
	require.NoError(t, err)
	require.NoError(t, inst.RestoreArchivedWorktree(restoreDest))

	// Read the registration from inside the kill's tombstone write — under
	// the op-lock, before the first reaper can fire. On the pre-fix head the
	// restored path is absent: the only set ever registered was the entry
	// snapshot's archive path.
	var excusedAtRestored, excusedAtArchive bool
	prev := killTombstonePersist
	killTombstonePersist = func(rid string, d session.InstanceData) error {
		excusedAtRestored = teardownreq.Is(requester, teardownreq.WorktreeTarget(restoreDest))
		excusedAtArchive = teardownreq.Is(requester, teardownreq.WorktreeTarget(archivedPath))
		return nil
	}
	t.Cleanup(func() { killTombstonePersist = prev })

	releasePeer()
	require.NoError(t, <-killDone)
	assert.True(t, excusedAtRestored,
		"the kill reaps at the restored path the caller's cwd followed — the requester must be excused there before the first reap")
	assert.True(t, excusedAtArchive,
		"the entry registration must survive — it is what excuses the requester from the serialized-ahead op's own source reap during the wait")
	assert.False(t, teardownreq.Is(requester, teardownreq.WorktreeTarget(restoreDest)),
		"the exemption ends with the reply")
}

package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// #3715: the archive verb's half of the window #3713 closed for restore.
//
// archiveSession claimed killsInFlight and THEN waited up to opLockTimeout (30s)
// for the session's operation lock, with the archive fence raised much later —
// after the re-verify, the remote-route branch, the destination derivation and
// the relocation claim. canKillFor reads the op axis alone, so for that whole
// stretch the row sat at {live, OpNone} and went on advertising a Kill that
// killSessionRequestedBy refused at the admission gate with "kill already in
// progress for session X" — an operation the user never started.
//
// The remedy is the one #3713 established: take the op-lock first so the wait
// holds nothing, then claim, then raise the fence adjacent to that claim. What
// archive needs on top of it is the post-lock re-read: its pre-lock guards (not
// archived, op == None) used to be held across the wait BY the claim, and once
// the claim moves behind the lock a peer can archive the row while this call is
// queued. Without re-checking them under the lock the reorder would let a second
// archive run on an already-archived session.

// parkArchiveAtItsOpLock stops the next archive at the head of its op-lock wait.
// In the PRE-FIX ordering that point sits AFTER the killsInFlight claim, so what
// the assertions read there is what a client saw for the whole wait.
func parkArchiveAtItsOpLock(t *testing.T) (parked <-chan struct{}, resume func()) {
	t.Helper()
	at := make(chan struct{})
	go_ := make(chan struct{})
	var entered bool
	prev := beforeArchiveOperationLock
	beforeArchiveOperationLock = func() {
		if !entered {
			entered = true
			close(at)
			<-go_
		}
	}
	var released bool
	resume = func() {
		if !released {
			released = true
			close(go_)
		}
	}
	t.Cleanup(func() {
		resume()
		beforeArchiveOperationLock = prev
	})
	return at, resume
}

func awaitParkedArchive(t *testing.T, parked <-chan struct{}) {
	t.Helper()
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("the archive never reached its operation-lock wait")
	}
}

// TestArchiveSession_OpLockWaitLeavesKillAdmitted is #3715 where it reproduces:
// an archive queued behind a peer that holds the operation lock, with a real Kill
// pressed at that instant.
func TestArchiveSession_OpLockWaitLeavesKillAdmitted(t *testing.T) {
	withOpLockTimeout(t, 500*time.Millisecond)

	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "archiving")
	key := daemonInstanceKey(repoID, "archiving")
	require.True(t, inst.CanKill(), "setup: a settled live row advertises Kill before any archive claims it")
	releasePeer := holdOpLock(t, manager, key)

	parked, resume := parkArchiveAtItsOpLock(t)
	archiveDone := make(chan error, 1)
	go func() {
		_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "archiving", RepoID: repoID})
		archiveDone <- err
	}()
	awaitParkedArchive(t, parked)

	assertRowAdvertisesKill(t, manager, inst, key, "while an archive waits for the operation lock")
	assertKillIsAdmittedButBoundedByThePeer(t, manager, repoID, "archiving")

	resume()
	select {
	case err := <-archiveDone:
		require.Error(t, err, "the peer never released the lock, so the archive cannot have run")
	case <-time.After(30 * time.Second):
		t.Fatal("the archive never returned")
	}
	releasePeer()
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "a refused archive must not leave the row busy")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness(), "a refused archive must not have shelved the session")
}

// TestArchiveSession_NoClaimIsHeldThroughTheOpLockWait watches the GENUINE wait
// and pins the other half: the wait is still a WAIT. An archive that queues
// behind a peer must still run once the peer lets go.
func TestArchiveSession_NoClaimIsHeldThroughTheOpLockWait(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "queued-archive")
	key := daemonInstanceKey(repoID, "queued-archive")
	releasePeer := holdOpLock(t, manager, key)

	parked, resume := parkArchiveAtItsOpLock(t)
	done := make(chan error, 1)
	go func() {
		_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "queued-archive", RepoID: repoID})
		done <- err
	}()
	awaitParkedArchive(t, parked)
	resume()

	// The peer holds the lock for the whole window, so the archive cannot get past
	// it and every sample lands inside the wait.
	require.Never(t, func() bool { return killGuardHeld(manager, key) }, 250*time.Millisecond, 5*time.Millisecond,
		"the archive held an exclusive-operation claim while merely QUEUED behind a peer: every Kill, "+
			"restore, prompt and rename against the session is refused for the length of that wait, and the "+
			"row advertises Kill throughout (#3715)")
	require.True(t, inst.CanKill(), "the queued archive must leave the row's advertised Kill honest")

	releasePeer()
	select {
	case err := <-done:
		require.NoError(t, err, "an archive that waits for a peer must still run once the peer releases")
	case <-time.After(30 * time.Second):
		t.Fatal("the queued archive never returned")
	}
	assert.Equal(t, session.LiveArchived, inst.GetLiveness(), "the archive that waited must have shelved the session")
	assert.False(t, exists(srcPath), "the worktree must have been relocated out of its live path")
}

// TestArchiveSession_TwoArchivesRacingTheOpLockArchiveExactlyOnce is the guard
// the reorder makes load-bearing, driven by two REAL archives rather than a
// simulated residue.
//
// The pre-lock "already archived" and "op in flight" checks used to be held
// across the wait BY the claim: a second archive was refused at the claim before
// it ever waited. With the claim behind the lock both callers queue on the lock,
// one wins, and the loser must re-read those guards under the lock — otherwise it
// runs a SECOND archive over an already-shelved session (which the transition
// layer rejects outright: BeginArchive is not legal from Archived).
//
// PRE-FIX the loser is refused at the claim instead, with "an operation is
// already in progress" — a message about an operation that has not started,
// which is the #3715 false affordance seen from the second caller.
func TestArchiveSession_TwoArchivesRacingTheOpLockArchiveExactlyOnce(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "raced-archive")
	key := daemonInstanceKey(repoID, "raced-archive")
	releasePeer := holdOpLock(t, manager, key)

	parked, resume := parkArchiveAtItsOpLock(t)
	first := make(chan error, 1)
	go func() {
		_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "raced-archive", RepoID: repoID})
		first <- err
	}()
	awaitParkedArchive(t, parked)
	resume() // genuinely queued on the peer's lock from here

	// A second, equally real archive of the same session, queued behind the same
	// lock. The hook fires only for the first caller, so this one does not park.
	second := make(chan error, 1)
	go func() {
		_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "raced-archive", RepoID: repoID})
		second <- err
	}()
	require.Never(t, func() bool { return killGuardHeld(manager, key) }, 150*time.Millisecond, 5*time.Millisecond,
		"a queued archive claimed the session before it held the lock, so its peer is refused for an "+
			"operation that has not started (#3715)")

	releasePeer()
	var errs []error
	for i := 0; i < 2; i++ {
		select {
		case err := <-first:
			errs = append(errs, err)
			first = make(chan error, 1)
		case err := <-second:
			errs = append(errs, err)
			second = make(chan error, 1)
		case <-time.After(30 * time.Second):
			t.Fatal("an archive never returned after the peer released its operation lock")
		}
	}

	var succeeded, refused int
	for _, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		refused++
		require.ErrorIs(t, err, ErrAlreadyArchived,
			"the losing archive must be refused by the guards re-read under the lock, with the idempotent "+
				"already-archived sentinel DeleteProject relies on — got: %v", err)
	}
	require.Equal(t, 1, succeeded, "exactly one of the two racing archives may run")
	require.Equal(t, 1, refused, "the other must be refused, not run a second archive")

	assert.Equal(t, session.LiveArchived, inst.GetLiveness(), "the row must be archived exactly once")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "neither archive may leave the row busy")
	assert.False(t, exists(srcPath), "the worktree must have been relocated exactly once")
}

// TestArchiveSession_FenceIsUpBeforeTheLocalRouteTouchesAnything pins the second
// stretch of #3715's window: the raise must sit adjacent to the claim, not after
// the destination derivation and the relocation claim. Those run before any
// teardown, but they are not free — and while the fence is down the row is still
// advertising a Kill the admission gate would refuse.
func TestArchiveSession_FenceIsUpBeforeTheLocalRouteTouchesAnything(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "fenced-archive")

	var opAtPath session.InFlightOp
	var canKillAtPath, projectedAtPath bool
	prev := beforeArchiveWorktreePath
	beforeArchiveWorktreePath = func() {
		opAtPath = inst.GetInFlightOp()
		canKillAtPath = inst.CanKill()
		projectedAtPath = inst.ToInstanceData().CanKill
	}
	t.Cleanup(func() { beforeArchiveWorktreePath = prev })

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "fenced-archive", RepoID: repoID})
	require.NoError(t, err)

	require.Equal(t, session.OpArchiving, opAtPath,
		"the archive fence was still down when the local route began deriving its destination: the claim "+
			"already refuses Kill at the admission gate, so the row advertises a control that can only "+
			"fail (#3715)")
	require.False(t, canKillAtPath, "Instance.CanKill() was true at the head of the local archive route")
	require.False(t, projectedAtPath, "the projected can_kill was true at the head of the local archive route")
}

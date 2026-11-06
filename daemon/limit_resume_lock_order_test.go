package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// The documented lock order (startLockForRepo) is op lock -> repository start
// lock -> worktree admission -> m.mu, and CreateSession follows it: it holds the
// start lock and then WAITS on admission. A limit resume used to take admission
// at its entry and then reach for the start lock inside — publishSessionSnapshot,
// every persistSettlement, and the completion checkpoint all take it — so a
// resume overlapping a same-repo create was one unbounded wait from a deadlock;
// only the create's bounded admission wait kept it a stall (#6880, the #6197
// inversion class reaching the resume paths). The restore paths fixed the same
// defect by holding admission only across the rebuild choke point and writing
// under it through the admission-safe primitives; a limit resume now keeps the
// same shape: admission is held only around the respawn, and every persist that
// fires inside it uses the under-admission writers.
//
// These tests pin both halves. The probe test asserts no durable row write ever
// runs with admission and the start lock both held — on master every
// persistSettlement in the resume body violates it — and that the respawn's
// live-boundary write still runs under admission (a fix that simply stopped
// taking admission would pass the first half for the wrong reason). The overlap
// test is the create interleave: while a resume is blocked on the repo start
// lock, a create's bounded admission wait must still succeed.

// TestResumeFromLimit_PersistsWithoutTheStartLockUnderAdmission drives the
// manual resume RPC through its respawn arm (alive=false: the agent's tmux
// exited while blocked) under the lock-order probe. Every write the resume
// performs while holding worktree admission must avoid the repo start lock; the
// live-boundary settlement inside the respawn is what proves the admitted span
// is still real.
func TestResumeFromLimit_PersistsWithoutTheStartLockUnderAdmission(t *testing.T) {
	manager, repoID, inst, backend := newAutoResumeManager(t, "", false, "finish the migration", time.Now().Add(time.Hour))

	probe := installLockOrderProbe(t, manager, repoID)
	outcome, err := manager.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID})
	require.NoError(t, err)
	require.Equal(t, resumePerformed, outcome)
	_, respawnCalls, _ := backend.snapshot()
	require.Equal(t, 1, respawnCalls, "the limit resume must reach its respawn arm for this test to exercise admission")
	probe.require(t, true)
}

// TestResumeLimitedSessions_PersistsWithoutTheStartLockUnderAdmission is the
// same probe oracle through the auto-resume scheduler's entry: its
// non-blocking admission must follow the same one global order.
func TestResumeLimitedSessions_PersistsWithoutTheStartLockUnderAdmission(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, repoID, inst, backend := newAutoResumeManager(t, "", false, "resume work", base)

	probe := installLockOrderProbe(t, manager, repoID)
	advance(limitResumeGrace + time.Second)
	manager.ResumeLimitedSessions()

	_, respawnCalls, _ := backend.snapshot()
	require.Equal(t, 1, respawnCalls, "the scheduler's limit resume must reach its respawn arm for this test to exercise admission")
	require.False(t, inst.LimitReached(), "precondition: the scheduled resume completed")
	probe.require(t, true)
}

// TestResumeFromLimit_OverlappingACreateAtItsStartLock is the overlap oracle: a
// create that already holds the repository start lock and is about to wait on
// worktree admission, against a resume of a limit-blocked session.
//
// The resume raises its op fence (BeginLimitResume) before its first
// start-lock use, so the fence being up proves the resume is inside its
// operation and — under the unfixed ordering — holding worktree admission
// while blocked on the start lock. On master the admission wait below then
// times out, which is exactly the create's failure mode in the field: it owns
// the start lock and cannot get admission. Every wait is under a deadline so
// the regression reports a timeout rather than hanging the package.
func TestResumeFromLimit_OverlappingACreateAtItsStartLock(t *testing.T) {
	manager, repoID, inst, backend := newAutoResumeManager(t, "", false, "finish the migration", time.Now().Add(time.Hour))

	// The create, past its start lock.
	startLock := manager.startLockForRepo(repoID)
	startLock.Lock()
	startHeld := true
	defer func() {
		if startHeld {
			startLock.Unlock()
		}
	}()

	type result struct {
		outcome resumeFromLimitOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := manager.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repoID})
		done <- result{outcome: outcome, err: err}
	}()

	const deadline = 20 * time.Second
	requireEventually(t, "the resume to raise its operation fence", func() bool {
		return inst.GetInFlightOp() == session.OpRespawning
	})

	// The create's next step. The resume must not be holding worktree
	// admission while it waits on the start lock the create already owns —
	// admission may be held only across the rebuild itself, which the resume
	// has not reached while it is parked on the start lock.
	admission := manager.worktreeAdmissionLockForRepo(repoID)
	acquired, _ := lockWithin(admission, deadline)
	require.True(t, acquired, "a same-repo create could not take worktree admission within %v: "+
		"the resume holds admission while waiting on the repo start lock the create holds — the inversion (#6880)", deadline)
	admission.Unlock()

	startLock.Unlock()
	startHeld = false
	select {
	case r := <-done:
		require.NoError(t, r.err)
		require.Equal(t, resumePerformed, r.outcome)
		_, respawnCalls, _ := backend.snapshot()
		require.Equal(t, 1, respawnCalls, "the resume must have completed through its respawn arm")
	case <-time.After(deadline):
		t.Fatalf("the resume did not finish within %v of the start lock being released", deadline)
	}
}

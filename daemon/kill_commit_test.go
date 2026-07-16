package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// installFailKillTombstoned builds a session whose teardown always fails, then
// drives one KillSession over it — which durably tombstones the record and, since
// the teardown could not complete, leaves the record in place. That is exactly the
// state finishUserKill is meant to converge from.
func installFailKillTombstoned(t *testing.T, title string) (*Manager, string, *session.Instance) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	restore := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		return failKillBackend{readyFakeBackend{fake}}, nil
	})
	t.Cleanup(restore)

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    title,
		RepoPath: repoPath,
		Program:  "claude",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := manager.KillSession(KillSessionRequest{Title: title, RepoID: repo.ID}); err == nil {
		t.Fatal("expected KillSession to surface the failing teardown")
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, title)]
	manager.mu.Unlock()
	if inst == nil {
		t.Fatal("the instance must stay tracked after a teardown that could not complete")
	}
	return manager, repo.ID, inst
}

// The #1917 follow-up locks, daemon side. Shared principle with the session-side
// gate tests: a bound that TRIPS must stop the destructive step. These pin the
// two places a tripped bound must change control flow rather than be logged.

// TestKillSession_UnrecordableKill_AbortsBeforeTeardown: the tombstone is the
// kill's commit point, so a kill that cannot be RECORDED must not be PERFORMED.
//
// Bounding UpdateRepoInstances gave this write a new, realistic failure mode
// (another af process holding the instances flock), where before it could only
// fail on a disk fault. Tearing down anyway leaves no durable record that the
// user asked for the kill — so a daemon that then dies before the record delete
// reloads a non-tombstoned row whose tmux is gone, classifies it Lost, and
// RESTORES it: a session the user explicitly killed comes back, pointed at a
// worktree the teardown already deleted.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: persistKillTombstone returned nothing,
// KillSession logged the failure and tore the session down regardless (kills=1).
func TestKillSession_UnrecordableKill_AbortsBeforeTeardown(t *testing.T) {
	backend := &raceBackend{}
	manager, repoID, inst := installRaceBackend(t, backend, "unrecordable")

	// Force exactly the write that records the kill intent to fail, and nothing
	// else — the same isolation archivePersist gives the archive commit.
	prev := killTombstonePersist
	killTombstonePersist = func(string, session.InstanceData) error {
		return fmt.Errorf("instances lock is held by another process")
	}
	t.Cleanup(func() { killTombstonePersist = prev })

	_, err := manager.KillSession(KillSessionRequest{Title: "unrecordable", RepoID: repoID})
	if err == nil {
		t.Fatal("KillSession reported success though it could not record the kill intent")
	}
	if !strings.Contains(err.Error(), "retry") {
		t.Fatalf("the error must tell the user this is retryable: %v", err)
	}

	// NOTHING may have been destroyed: this is the last point at which the kill is
	// still free, and the whole value of aborting is that the retry is a true retry.
	if kills, _ := backend.counts(); kills != 0 {
		t.Fatalf("the session was torn down despite its kill never being recorded (kills=%d); "+
			"a crash before the record delete would then RESTORE a killed session (#1917)", kills)
	}

	// And no in-memory tombstone may linger: refreshInstanceStatus routes a
	// UserKilled instance to finishUserKill, which would complete on the next poll
	// exactly the kill this call just refused — defeating the abort.
	if inst.UserKilled() {
		t.Fatal("an aborted kill left an in-memory kill tombstone; the poll's finishUserKill " +
			"would tear the session down anyway, making the abort meaningless")
	}

	// The record must still be a live, killable session.
	rec := recordFor(t, repoID, "unrecordable")
	if rec == nil {
		t.Fatal("the record vanished after an aborted kill")
	}
	if rec.UserKilled {
		t.Fatal("an aborted kill left a durable tombstone on disk")
	}

	// The retry must work once the write can land again.
	killTombstonePersist = prev
	if _, err := manager.KillSession(KillSessionRequest{Title: "unrecordable", RepoID: repoID}); err != nil {
		t.Fatalf("retry after the write recovered must succeed, got: %v", err)
	}
}

// TestFinishUserKill_UnsafeTeardown_KeepsTheRecordForRetry: the tombstone finisher
// is the retry path the bounded teardown depends on, so it must not do the very
// thing the bound exists to prevent.
//
// Instance.Kill already swallows every failure tmux or git ANSWERED with, so an
// error reaching here means the teardown could not complete SAFELY — a pane whose
// liveness is unknown, or a worktree whose removal was cut off mid-delete. In both
// cases the workspace is still on disk. Deleting the record anyway orphans it and
// takes away the user's only handle to retry through.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: finishUserKill logged the Kill error and
// deleted the record regardless.
func TestFinishUserKill_UnsafeTeardown_KeepsTheRecordForRetry(t *testing.T) {
	manager, repoID, inst := installFailKillTombstoned(t, "orphan-risk")

	manager.finishUserKill(repoID, inst)

	rec := recordFor(t, repoID, "orphan-risk")
	if rec == nil {
		t.Fatal("finishUserKill deleted the record even though the teardown could not complete " +
			"safely: the worktree is still registered on disk and the user has just lost the only " +
			"handle to retry the kill through (#1917)")
	}
	if !rec.UserKilled {
		t.Fatal("the retained record must keep its tombstone, or the next poll restores it")
	}
	manager.mu.Lock()
	_, tracked := manager.instances[daemonInstanceKey(repoID, "orphan-risk")]
	manager.mu.Unlock()
	if !tracked {
		t.Fatal("the instance was dropped from the manager, so no later poll can retry the kill")
	}
}

// TestRestoreLostSessions_SurvivedSettleThenDied_StartsFreshEpisode: the settle
// deadline is load-bearing, not decoration.
//
// A runtime that OUTLIVED confirmBy was confirmed alive by definition, even if no
// sweep happened to observe it non-Lost first — the sweep only runs while the row
// reads non-Lost, so a death just after the window can beat it. Treating that as
// "died before confirmation" saddles a genuinely new loss episode with the
// previous one's failure history and backs it off instead of restoring it
// promptly.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: diedBeforeConfirm read st.awaitingConfirm
// without consulting st.confirmBy, so ANY Lost row with a pending confirmation
// inherited the old history (failures=1, backed off) no matter how long its
// runtime had actually survived.
func TestRestoreLostSessions_SurvivedSettleThenDied_StartsFreshEpisode(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "long-lived", backend, true, session.Lost)
	zeroRestoreBackoff(t)

	key := daemonInstanceKey(repoID, "long-lived")
	manager.RestoreLostSessions() // recovers; arms the confirmation window
	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("recover calls = %d, want 1", got)
	}

	// The runtime survived well past its settle window, then died much later.
	manager.mu.Lock()
	st := manager.lostRestoreStates[key]
	if st == nil || !st.awaitingConfirm {
		manager.mu.Unlock()
		t.Fatal("expected a restore awaiting confirmation")
	}
	st.confirmBy = time.Now().Add(-time.Hour)
	manager.mu.Unlock()
	inst.SetStatusForTest(session.Lost)

	manager.RestoreLostSessions()

	manager.mu.Lock()
	st = manager.lostRestoreStates[key]
	failures := -1
	if st != nil {
		failures = st.consecutiveFailures
	}
	manager.mu.Unlock()

	if failures > 0 {
		t.Fatalf("a runtime that outlived its settle window was charged %d failure(s) from the "+
			"PREVIOUS episode; it was confirmed alive, so this loss must start a fresh episode "+
			"and be restored promptly rather than backed off (#1910)", failures)
	}
	if got := backend.recoverCalls(); got != 2 {
		t.Fatalf("recover calls = %d, want 2: a fresh loss episode must attempt a restore, not back off", got)
	}
}

// TestRefreshInstanceStatus_TombstonedAfterTeardown_StillFinishesTheKill is
// review finding (5): the promise of an automatic retry has to be one the code
// keeps.
//
// When the record delete loses a bounded race for the instances lock, the kill
// returns an error saying it "will be retried automatically" — but Instance.Kill
// has already set started=false, and refreshInstanceStatus returned at its
// !Started() check BEFORE it looked for the tombstone. So finishUserKill never
// ran, the tombstone sat unprocessed for the daemon's whole life, and the session
// stayed on screen: the #1917 symptom, reached by a new route.
//
// This survived the first round because the DISK record still says started=true
// (the tombstone is written before teardown), so a daemon RESTART reloaded it and
// finished the kill — which is exactly the "only a restart can reap it" behavior
// this PR exists to remove.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the record is never deleted (finishUserKill
// is unreachable for a torn-down instance).
func TestRefreshInstanceStatus_TombstonedAfterTeardown_StillFinishesTheKill(t *testing.T) {
	backend := &raceBackend{}
	manager, repoID, inst := installRaceBackend(t, backend, "stranded")

	// The exact post-teardown state a lock-timed-out kill leaves behind: the
	// teardown ran (started=false) and the kill intent is durable.
	if err := manager.persistKillTombstone(repoID, inst, nil); err != nil {
		t.Fatalf("persistKillTombstone: %v", err)
	}
	inst.SetStartedForTest(false)
	if rec := recordFor(t, repoID, "stranded"); rec == nil || !rec.UserKilled {
		t.Fatal("setup: the record must carry a durable tombstone")
	}

	// One poll, with the contention now gone.
	manager.refreshInstanceStatus(repoID, inst)

	if rec := recordFor(t, repoID, "stranded"); rec != nil {
		t.Fatal("the poll never finished the tombstoned kill, so the session stays on screen " +
			"undeletable until the daemon restarts — the very outcome this PR removes, and the " +
			"error told the user it would be retried automatically (#1917 review)")
	}
	manager.mu.Lock()
	_, tracked := manager.instances[daemonInstanceKey(repoID, "stranded")]
	manager.mu.Unlock()
	if tracked {
		t.Fatal("the finished kill left the instance tracked in the manager")
	}
}

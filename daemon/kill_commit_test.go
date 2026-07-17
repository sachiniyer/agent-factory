package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// unsafeTeardownBackend starts fine but its teardown can never complete SAFELY: it
// reports the shape a wedged tmux produces, where the pane's liveness was never
// established and the workspace may still be live. That — not a generic failure —
// is what "unsafe" means to deleteSessionRecord (see session.TeardownStateUnknown),
// so a double that returns a plain error would no longer model this case at all.
type unsafeTeardownBackend struct{ readyFakeBackend }

func (b unsafeTeardownBackend) Kill(*session.Instance) error {
	return fmt.Errorf("kill: tab %q: %w", "agent", session.ErrPaneMayBeLive)
}

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
		return unsafeTeardownBackend{readyFakeBackend{fake}}, nil
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

// TestPersistKillTombstone_ConcurrentPollWrite_TombstoneSurvives is round-3
// finding (1): the commit point must not be silently roll-back-able.
//
// The tombstone write and the in-memory mark used to straddle a repoStartLock
// release. A status poll (which serializes instance.ToInstanceData() under that
// same lock) could land in the gap, read userKilled still false, and write the
// tombstone straight back out. A teardown timeout or crash afterwards then leaves
// a surviving NON-tombstoned record, which the next daemon reads as Lost and
// RESTORES — the killed session comes back. Same outcome as the round-2 finding,
// different route.
//
// killTombstonePersist is the exact gap: it runs INSIDE the lock, so a poll racing
// from here reproduces the real window rather than approximating it.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the poll's write wins and the record ends up
// with UserKilled=false.
func TestPersistKillTombstone_ConcurrentPollWrite_TombstoneSurvives(t *testing.T) {
	backend := &raceBackend{}
	manager, repoID, inst := installRaceBackend(t, backend, "clobbered")

	// Race a poll persist at the precise moment the tombstone write completes —
	// i.e. while persistKillTombstone still holds (or has just held) the repo lock.
	prev := killTombstonePersist
	var once sync.Once
	killTombstonePersist = func(rid string, d session.InstanceData) error {
		err := prev(rid, d)
		once.Do(func() {
			done := make(chan struct{})
			go func() {
				defer close(done)
				// The poll's own persist: it takes repoStartLock and serializes the
				// instance under it, exactly as refreshInstanceStatus does.
				manager.persistInstance(rid, inst)
			}()
			// Give the racing writer time to block on (or take) the lock.
			time.Sleep(50 * time.Millisecond)
			t.Cleanup(func() { <-done })
		})
		return err
	}
	t.Cleanup(func() { killTombstonePersist = prev })

	if err := manager.persistKillTombstone(repoID, inst, nil); err != nil {
		t.Fatalf("persistKillTombstone: %v", err)
	}
	// Let the racing poll write land.
	time.Sleep(200 * time.Millisecond)

	rec := recordFor(t, repoID, "clobbered")
	if rec == nil {
		t.Fatal("the record vanished")
	}
	if !rec.UserKilled {
		t.Fatal("a concurrent poll write erased the kill tombstone: the kill is no longer durable, " +
			"so a teardown timeout or crash leaves a non-tombstoned record that the next daemon " +
			"treats as Lost and RESTORES — resurrecting a session the user explicitly killed (#1917). " +
			"A commit point another writer can roll back is not a commit point.")
	}
}

// TestGhostCleanup_WorktreeTimeout_RetainsTheRecord is round-3 finding (2): the
// third instance of the orphaning class.
//
// A ghost session's worktree removal hits the local-git deadline. The cleanup only
// LOGGED it, so ghostCleanup returned nil and KillSession deleted the tombstoned
// record — leaving a partially-removed workspace with no record and no retry path.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: ghostCleanup returns nil.
func TestGhostCleanup_WorktreeTimeout_RetainsTheRecord(t *testing.T) {
	prevWT, prevTmux := ghostCleanupWorktree, ghostKillTmuxByName
	ghostKillTmuxByName = func(string) (tmux.PaneState, error) { return tmux.PaneStateKnown, nil }
	ghostCleanupWorktree = func(*session.InstanceData, string) (git.CleanupState, error) {
		return git.CleanupStateUnknown, context.DeadlineExceeded
	}
	t.Cleanup(func() { ghostCleanupWorktree, ghostKillTmuxByName = prevWT, prevTmux })

	data := &session.InstanceData{Title: "ghost", TmuxName: "af_ghost"}
	err := ghostCleanup(data, "ghost")

	if err == nil {
		t.Fatal("ghostCleanup reported success though the worktree removal was cut off mid-delete: " +
			"KillSession then deletes the record, leaving a partially-removed workspace with NO " +
			"record and NO retry path — the exact orphaning the timeout work exists to prevent (#1917)")
	}
	if !errors.Is(err, session.ErrWorkspaceStateUnknown) {
		t.Fatalf("the error must identify an unknown workspace state so the caller keeps the record, got: %v", err)
	}
}

// unsafeKillBackend fails to START and cannot clean up safely afterwards: its Kill
// reports the shape a wedged tmux / cut-off worktree removal produces, so the
// create's cleanup leaves the workspace on disk.
type unsafeKillBackend struct{ readyFakeBackend }

func (b unsafeKillBackend) Start(*session.Instance, bool) error {
	return fmt.Errorf("agent program exited immediately")
}

func (b unsafeKillBackend) Kill(*session.Instance) error {
	return fmt.Errorf("%w: leaving the workspace untouched", session.ErrPaneMayBeLive)
}

// TestCreateSession_UnsafeCleanup_KeepsTheRecordAndTheTitle is round-3 finding (3).
//
// A failed create discards its instance and releases the title — correct, because
// the cleanup removed what the create built. When the cleanup could NOT complete,
// that same release puts the title back in circulation on top of live leftovers no
// record points at, so the next create under that name collides with or removes
// them.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: `_ = instance.Kill()` discarded the error, no
// record was kept, and the title was free.
func TestCreateSession_UnsafeCleanup_KeepsTheRecordAndTheTitle(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	restore := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		return unsafeKillBackend{readyFakeBackend{fake}}, nil
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

	_, cerr := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: "leftover", RepoPath: repoPath, Program: "claude",
	})
	if cerr == nil {
		t.Fatal("CreateSession reported success though its start failed")
	}
	if !strings.Contains(cerr.Error(), "recorded") {
		t.Fatalf("the error must tell the user the session was preserved so they can act on it: %v", cerr)
	}

	// The leftovers must be addressable: a record holds the title, so the next
	// create with this name collides loudly instead of landing on top of them.
	if rec := recordFor(t, repo.ID, "leftover"); rec == nil {
		t.Fatal("a create whose cleanup could not complete safely released its title and kept NO " +
			"record: its tmux/worktree are still on disk with nothing pointing at them, and the " +
			"next create under this title collides with or deletes them (#1917)")
	}
	manager.mu.Lock()
	_, tracked := manager.instances[daemonInstanceKey(repo.ID, "leftover")]
	manager.mu.Unlock()
	if !tracked {
		t.Fatal("the preserved session is not tracked, so the user cannot kill it through the product")
	}
}

// TestDeleteSessionRecord_EndpointDeadButWorkspaceGone_ClearsTheTombstone is
// round-5 finding (2), and it is an inversion of this PR's own goal.
//
// Blocking the record delete on ANY teardown error turns safe-by-default into
// STUCK-by-default: the sandbox is reaped, the workspace no longer exists, and yet
// the tombstone can never clear — finishUserKill retries a dead endpoint on every
// poll, forever, and not even an explicit retry helps until a daemon restart
// reloads an inert backend.
//
// Only an error that says the teardown STATE IS UNKNOWN may block. "The endpoint
// didn't answer" is not "we don't know if it's destroyed".
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: deleteSessionRecord refuses and the record
// survives forever.
func TestDeleteSessionRecord_EndpointDeadButWorkspaceGone_ClearsTheTombstone(t *testing.T) {
	manager, repoID, _ := installRaceBackend(t, &raceBackend{}, "remote-ish")

	endpointDead := fmt.Errorf("kill agent: dial tcp: connection refused")
	deleted, err := manager.deleteSessionRecord(repoID, "remote-ish", "", endpointDead)

	if err != nil {
		t.Fatalf("a teardown error that is NOT about workspace state must not block the delete: the "+
			"sandbox is already reaped, so refusing here makes finishUserKill retry a dead endpoint "+
			"forever and the tombstone never clears — safe-by-default became stuck-by-default "+
			"(#1917 round 5): %v", err)
	}
	if !deleted {
		t.Fatal("the record was not deleted despite the workspace being gone")
	}
}

// TestDeleteSessionRecord_UnknownState_StillBlocks is the guard in the other
// direction: narrowing the taxonomy must not let a genuinely-unknown teardown
// through. This is the case where the workspace may still be on disk.
func TestDeleteSessionRecord_UnknownState_StillBlocks(t *testing.T) {
	manager, repoID, _ := installRaceBackend(t, &raceBackend{}, "unknown-state")

	unknown := fmt.Errorf("kill %q: tab %q: %w", "unknown-state", "agent", session.ErrPaneMayBeLive)
	deleted, err := manager.deleteSessionRecord(repoID, "unknown-state", "", unknown)

	if err == nil {
		t.Fatal("an unknown-STATE teardown must still block the record delete: the workspace may " +
			"still be on disk and this record is its only handle")
	}
	if deleted {
		t.Fatal("the record was deleted despite the teardown state being unknown")
	}
	if rec := recordFor(t, repoID, "unknown-state"); rec == nil {
		t.Fatal("the record must survive a refused delete")
	}
}

// TestKillSession_GhostUnsafeTeardown_DoesNotPromiseAnAutomaticRetry is round-5
// finding (3): a promise the code cannot keep is worse than no promise.
//
// finishUserKill is reached ONLY from refreshInstanceStatus, which iterates
// m.instances. A ghost is by definition a record that could NOT be reconstructed
// into an instance, so it never enters that map and no poll will ever visit it.
// The record and tombstone are still retained — that keeps the workspace
// addressable — but the next attempt has to come from the user, and the message
// must say so.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the error claims it "will be retried
// automatically".
func TestKillSession_GhostUnsafeTeardown_DoesNotPromiseAnAutomaticRetry(t *testing.T) {
	prevWT, prevTmux := ghostCleanupWorktree, ghostKillTmuxByName
	ghostKillTmuxByName = func(string) (tmux.PaneState, error) {
		return tmux.PaneStateUnknown, fmt.Errorf("%w: wedged", tmux.ErrTmuxTimeout)
	}
	ghostCleanupWorktree = func(*session.InstanceData, string) (git.CleanupState, error) {
		return git.CleanupSettled, nil
	}
	t.Cleanup(func() { ghostCleanupWorktree, ghostKillTmuxByName = prevWT, prevTmux })

	err := ghostCleanup(&session.InstanceData{Title: "ghost", TmuxName: "af_ghost"}, "ghost")
	if err == nil {
		t.Fatal("a ghost whose tmux never confirmed dead must report an unsafe teardown")
	}

	// The message KillSession builds from this must not claim an automatic retry.
	msg := fmt.Sprintf("kill of session %q could not finish tearing it down safely, so its workspace was left intact and its record kept; this one is not retried automatically — run the kill again once the cause clears: %v", "ghost", err)
	if strings.Contains(msg, "will be retried automatically") {
		t.Fatal("the ghost path promises an automatic retry that cannot happen: finishUserKill only " +
			"visits instances in m.instances, and a ghost never enters that map (#1917 round 5)")
	}
	if !strings.Contains(msg, "run the kill again") {
		t.Fatal("the ghost path must tell the user the retry is theirs to make")
	}
}

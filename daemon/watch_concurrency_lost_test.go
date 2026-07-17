package daemon

import (
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// A Lost session is one whose backing tmux vanished with no kill on record — an
// outage, an OOM, a reboot. The daemon's restore loop keeps trying to revive it
// and never gives up on a recoverable one, so for the concurrency cap it is still
// very much this task's session: it can come back Running at any tick.
//
// These pin that the cap counts it. Freeing the slot the moment a session went
// Lost would let a capped watcher admit replacements during an outage and then
// blow past max_concurrent_runs when RestoreLostSessions revived the originals —
// exactly the breach #1892 exists to prevent, arriving through the back door.

// markLost drives a session to Lost through the same daemon-truth edge the status
// poll uses when it finds the backing tmux gone.
func markLost(t *testing.T, m *Manager, repoID, title string) *session.Instance {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.instances[daemonInstanceKey(repoID, title)]
	if inst == nil {
		t.Fatalf("no live instance for %q", title)
	}
	if err := inst.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
		t.Fatalf("transition to lost: %v", err)
	}
	return inst
}

// TestWatchConcurrencyLostSessionKeepsItsSlot is the regression for the cap
// breach: sessions lost to an outage must keep holding their slots, so the
// watcher parks its events instead of spawning replacements that would put the
// task over its cap once the restore loop revives the originals.
func TestWatchConcurrencyLostSessionKeepsItsSlot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const limit = 2
	var titles []string
	for i := 0; i < limit; i++ {
		data, err := createForTask(manager, repoPath, "task1", "outage", limit)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		titles = append(titles, data.Title)
	}

	// The outage: every one of the task's sessions loses its backing tmux.
	for _, title := range titles {
		markLost(t, manager, repo.ID, title)
	}

	// The cap must still bind. RestoreLostSessions is retrying these, so admitting
	// now would mean 2 replacements + 2 revived originals = 4 sessions for a cap
	// of 2.
	if _, err := createForTask(manager, repoPath, "task1", "outage", limit); !isAtConcurrencyLimitErr(err) {
		t.Fatalf("create while the task's sessions are lost and restorable: want the at-limit refusal, got %v", err)
	}

	// The restore lands: the revived session is Running and still holds its slot,
	// so the cap keeps binding — no replacement was admitted in the meantime.
	m := manager
	m.mu.Lock()
	revived := m.instances[daemonInstanceKey(repo.ID, titles[0])]
	m.mu.Unlock()
	if err := revived.Transition(session.ObserveLiveness(session.LiveRunning)); err != nil {
		t.Fatalf("transition to running: %v", err)
	}
	if _, err := createForTask(manager, repoPath, "task1", "outage", limit); !isAtConcurrencyLimitErr(err) {
		t.Fatalf("create after a lost session was restored: want the at-limit refusal, got %v", err)
	}

	// The off-ramp: a session that is lost for good is killed, which tombstones it
	// and releases its slot. Without this the cap could never recover from a
	// permanently unrestorable session.
	m.mu.Lock()
	tombstoned := m.instances[daemonInstanceKey(repo.ID, titles[1])]
	m.mu.Unlock()
	tombstoned.MarkUserKilled()
	m.mu.Lock()
	err = m.admitTaskRunLocked(repo.ID, "task1", limit)
	m.mu.Unlock()
	if err != nil {
		t.Fatalf("admit after a lost session was killed: want the slot released, got %v", err)
	}
}

// TestCanAutoRestoreLostSession pins which Lost sessions hold a slot. This is
// restoreLostSession's own entry gate — the cap and the loop share it precisely
// so this table describes both. Over-holding wedges a capped task; over-freeing
// breaches the cap, so both directions matter.
func TestCanAutoRestoreLostSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	newLost := func(t *testing.T, title string) *session.Instance {
		t.Helper()
		inst, err := session.NewInstance(session.InstanceOptions{
			Title: title, TaskID: "task1", Path: t.TempDir(), Program: "claude",
		})
		if err != nil {
			t.Fatalf("NewInstance: %v", err)
		}
		inst.SetStartedForTest(true)
		if err := inst.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
			t.Fatalf("transition to lost: %v", err)
		}
		return inst
	}

	t.Run("a started, recoverable, untombstoned lost session holds its slot", func(t *testing.T) {
		if !canAutoRestoreLostSession(newLost(t, "lost").LifecycleView()) {
			t.Fatal("the restore loop retries this session forever; it must keep its slot")
		}
	})

	t.Run("a tombstoned lost session releases its slot", func(t *testing.T) {
		inst := newLost(t, "killed")
		inst.MarkUserKilled()
		if canAutoRestoreLostSession(inst.LifecycleView()) {
			t.Fatal("a UserKilled record means finish-this-kill, never restore-this; it must not hold a slot")
		}
	})

	t.Run("an unstarted lost session releases its slot", func(t *testing.T) {
		inst := newLost(t, "unstarted")
		inst.SetStartedForTest(false)
		if canAutoRestoreLostSession(inst.LifecycleView()) {
			t.Fatal("restoreLostSession fences out an unstarted session; it must not hold a slot")
		}
	})

	t.Run("a live session holds its slot through the activity axis, not this", func(t *testing.T) {
		inst, err := session.NewInstance(session.InstanceOptions{
			Title: "live", TaskID: "task1", Path: t.TempDir(), Program: "claude",
		})
		if err != nil {
			t.Fatalf("NewInstance: %v", err)
		}
		inst.SetStartedForTest(true)
		if canAutoRestoreLostSession(inst.LifecycleView()) {
			t.Fatal("canAutoRestoreLostSession answers only for Lost sessions")
		}
	})

	t.Run("a zero view is not restorable", func(t *testing.T) {
		// The predicate takes a snapshot by value, so a nil instance can no longer
		// reach it — countTaskRunsLocked and restoreLostSession both fence nil at the
		// call site. The zero view is the degenerate input that survives: unstarted,
		// so it holds nothing.
		if canAutoRestoreLostSession(session.LifecycleView{}) {
			t.Fatal("a zero view is unstarted and must not hold a slot")
		}
	})
}

// TestCompletedRunDoesNotReacquireSlotWhenLost is the regression for the
// reacquire wedge (#1892). A task-spawned session keeps its TaskID for life, long
// after its run finished. When it goes idle its slot is released — but a later
// tmux outage marks that finished session Lost, and a Lost session is restorable,
// so a naive "Lost holds its slot" rule hands a slot back to a run that ended
// hours ago.
//
// A cap of 1 is the sharpest case: the finished session would consume the task's
// only slot, and every new event would park behind completed work while the
// restore loop retried it — the durable queue wedged indefinitely. The cap must
// follow the RUN, not the row.
func TestCompletedRunDoesNotReacquireSlotWhenLost(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const limit = 1
	data, err := createForTask(manager, repoPath, "task1", "done", limit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The run finishes: the agent goes idle, which releases the slot.
	manager.mu.Lock()
	finished := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()
	if err := finished.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatalf("transition to ready: %v", err)
	}
	manager.mu.Lock()
	err = manager.admitTaskRunLocked(repo.ID, "task1", limit)
	manager.mu.Unlock()
	if err != nil {
		t.Fatalf("a finished run must release its slot: %v", err)
	}

	// The outage: the finished session's tmux vanishes. It is now Lost and the
	// restore loop will happily retry it — but its work is long done.
	if err := finished.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
		t.Fatalf("transition to lost: %v", err)
	}
	if !canAutoRestoreLostSession(finished.LifecycleView()) {
		t.Fatal("precondition: the restore loop does retry this session, which is exactly why the cap must ask a second question")
	}
	if holdsTaskRunSlot(finished.LifecycleView()) {
		t.Fatal("a run that already finished must not reacquire a slot when a later outage marks it Lost")
	}

	// The real assertion: a new event still gets its session. With the wedge live,
	// this refuses forever.
	if _, err := createForTask(manager, repoPath, "task1", "done", limit); err != nil {
		t.Fatalf("a new event must not be parked behind a completed run: %v", err)
	}
}

// TestInterruptedRunKeepsSlotWhenLost is the converse, and the reason the fix
// cannot simply be "Lost never holds a slot": a session lost mid-run CAN come
// back Running, so freeing its slot lets the task exceed its cap the moment the
// restore lands. That is the original #1892 breach.
func TestInterruptedRunKeepsSlotWhenLost(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const limit = 1
	data, err := createForTask(manager, repoPath, "task1", "busy", limit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()

	// Working, then the outage hits mid-run.
	if err := inst.Transition(session.ObserveLiveness(session.LiveRunning)); err != nil {
		t.Fatalf("transition to running: %v", err)
	}
	if err := inst.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
		t.Fatalf("transition to lost: %v", err)
	}
	if !holdsTaskRunSlot(inst.LifecycleView()) {
		t.Fatal("a run interrupted mid-flight keeps its slot: the restore loop can bring it back Running and blow the cap")
	}
	if _, err := createForTask(manager, repoPath, "task1", "busy", limit); !isAtConcurrencyLimitErr(err) {
		t.Fatalf("create while an interrupted run is restorable: want the at-limit refusal, got %v", err)
	}
}

// TestSlotFollowsTheRunAcrossRestoreCycles walks one session through a full
// interruption cycle — lost mid-run, restored, finished, lost again — and pins
// that the slot follows the RUN at every step. The second loss is the sharp one:
// the same session, the same Lost state, the opposite answer, because by then its
// run is over.
func TestSlotFollowsTheRunAcrossRestoreCycles(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "cycle", TaskID: "task1", Path: t.TempDir(), Program: "claude",
	})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetStartedForTest(true)

	// Lost mid-run: the slot is held.
	if err := inst.Transition(session.ObserveLiveness(session.LiveRunning)); err != nil {
		t.Fatalf("running: %v", err)
	}
	if err := inst.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
		t.Fatalf("lost: %v", err)
	}
	if !holdsTaskRunSlot(inst.LifecycleView()) {
		t.Fatal("lost mid-run holds its slot")
	}

	// A repeated Lost observation must not disturb the run: the poll re-marks Lost
	// on every tick, and a verdict re-derived from the Lost state itself (never
	// "busy") would silently free every held slot on the next one.
	if err := inst.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
		t.Fatalf("lost again: %v", err)
	}
	if !holdsTaskRunSlot(inst.LifecycleView()) {
		t.Fatal("a repeated Lost observation must not flip the verdict loose")
	}

	// Restored, then the run finishes, then a second outage: now it is idle work.
	if err := inst.Transition(session.ObserveLiveness(session.LiveRunning)); err != nil {
		t.Fatalf("restored: %v", err)
	}
	if err := inst.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if err := inst.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
		t.Fatalf("lost after finishing: %v", err)
	}
	if holdsTaskRunSlot(inst.LifecycleView()) {
		t.Fatal("after the run finished, a later loss must not re-hold the slot")
	}
}

// TestTaskRunActiveSurvivesRestart: an outage that loses sessions is the same
// event that restarts the daemon, so the run's state has to come back from disk —
// a reload that re-decided it from the Lost state alone could not tell a finished
// run from an interrupted one.
func TestTaskRunActiveSurvivesRestart(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	newLostFrom := func(t *testing.T, title string, from session.Liveness) session.InstanceData {
		t.Helper()
		inst, err := session.NewInstance(session.InstanceOptions{
			Title: title, TaskID: "task1", Path: t.TempDir(), Program: "claude",
		})
		if err != nil {
			t.Fatalf("NewInstance: %v", err)
		}
		inst.SetStartedForTest(true)
		if err := inst.Transition(session.ObserveLiveness(from)); err != nil {
			t.Fatalf("transition to %v: %v", from, err)
		}
		if err := inst.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
			t.Fatalf("transition to lost: %v", err)
		}
		return inst.ToInstanceData()
	}

	busy := newLostFrom(t, "busy", session.LiveRunning)
	if !busy.TaskRunActive {
		t.Fatal("a session lost mid-run must persist that its run is still in flight")
	}
	idle := newLostFrom(t, "idle", session.LiveReady)
	if idle.TaskRunActive {
		t.Fatal("a session lost after finishing must persist that its run is over")
	}
}

// TestFailedArchiveOfCompletedRunDoesNotReclaimSlot is the regression for the
// reclaim bug reaching through the ARCHIVE door (#1892).
//
// Archiving is the DAEMON tearing a session down; it is not the agent working. A
// task session that finished its run and is then archived arrives at
// AbortArchiveToLost as LiveReady + OpArchiving — and every in-flight op
// classifies as pending. A predicate that reads "an op is in flight" as "the run
// is in flight" therefore decides this completed run was interrupted, and hands it
// back a slot it gave up long ago. With a small cap that parks new events forever.
//
// This is the same defect as a completed run reacquiring a slot on a tmux outage,
// reached by a different path — which is why the fix is to record the run's own
// lifetime rather than qualify one more neighbour.
func TestFailedArchiveOfCompletedRunDoesNotReclaimSlot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const limit = 1
	data, err := createForTask(manager, repoPath, "task1", "archived", limit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()

	// The run finishes.
	if err := inst.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if inst.TaskRunActive() {
		t.Fatal("precondition: the run is over once the agent goes idle")
	}

	// The user archives the finished session, and the archive FAILS: the session
	// rolls back to Lost so the restore loop can heal it in place. Its run, though,
	// ended before the archive ever began.
	if err := inst.Transition(session.BeginArchive()); err != nil {
		t.Fatalf("begin archive: %v", err)
	}
	if err := inst.Transition(session.AbortArchiveToLost()); err != nil {
		t.Fatalf("abort archive: %v", err)
	}
	if inst.TaskRunActive() {
		t.Fatal("archiving is teardown, not work: a failed archive must not resurrect a finished run")
	}
	if holdsTaskRunSlot(inst.LifecycleView()) {
		t.Fatal("a completed run whose archive failed must not reclaim a slot")
	}

	// The consequence that matters: new events still land.
	if _, err := createForTask(manager, repoPath, "task1", "archived", limit); err != nil {
		t.Fatalf("a new event must not be parked behind a completed run whose archive failed: %v", err)
	}
}

// TestFailedArchiveOfInterruptedRunKeepsSlot is the converse: a run that was still
// in flight when the archive began is still in flight when it fails, so it keeps
// its slot. The fix must not free slots wholesale.
func TestFailedArchiveOfInterruptedRunKeepsSlot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "busy-archive", TaskID: "task1", Path: t.TempDir(), Program: "claude",
	})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetStartedForTest(true)
	if err := inst.Transition(session.ObserveLiveness(session.LiveRunning)); err != nil {
		t.Fatalf("running: %v", err)
	}
	if err := inst.Transition(session.BeginArchive()); err != nil {
		t.Fatalf("begin archive: %v", err)
	}
	if err := inst.Transition(session.AbortArchiveToLost()); err != nil {
		t.Fatalf("abort archive: %v", err)
	}
	if !inst.TaskRunActive() {
		t.Fatal("the agent never went idle, so this run is still in flight")
	}
	if !holdsTaskRunSlot(inst.LifecycleView()) {
		t.Fatal("a run interrupted by a failed archive keeps its slot: the restore loop can bring it back Running")
	}
}

// TestCapHoldsAcrossConcurrentRestore is the regression for the race that let the
// cap breach back in through a different door (#1892).
//
// The slot predicate has two complementary arms — busy, or a restorable lost run.
// Complementary only against ONE view of the state: the restore loop mutates a
// session WITHOUT the manager lock (restoreLostSession releases m.mu before
// Recover, which ends in Transition(ConfirmLive) → LiveRunning). Reading the live
// instance once per arm let that transition land in between, so the first read saw
// LiveLost (not busy) and the second saw LiveRunning (not Lost) — the session fell
// through BOTH arms, went uncounted, and the cap admitted a run over its limit.
//
// TestInFlightRunIsCountedInEveryReachableView is the DETERMINISTIC lock, and the
// reason one snapshot is sufficient: an in-flight run holds its slot in every view
// it can be caught in. The restore cycle moves a session between exactly two
// observable states, and both count — so no instant exists at which the run is
// invisible, and a predicate judging any single view is correct at that instant.
//
// The old bug was not that either verdict was wrong. It was that reading twice
// produced a combination that is NOT any view: Lost for the first arm, Running for
// the second. This test states the property; TestCapHoldsAcrossConcurrentRestore
// below corroborates it against real goroutines.
func TestInFlightRunIsCountedInEveryReachableView(t *testing.T) {
	for _, tc := range []struct {
		name string
		view session.LifecycleView
	}{
		{"lost mid-restore", session.LifecycleView{
			TaskRunActive: true, Started: true, Recoverable: true,
			Liveness: session.LiveLost, Status: session.Lost,
		}},
		{"running just after the restore landed", session.LifecycleView{
			TaskRunActive: true, Started: true, Recoverable: true,
			Liveness: session.LiveRunning, Status: session.Running,
		}},
		{"mid-create", session.LifecycleView{
			TaskRunActive: true, Started: true, Recoverable: true,
			Liveness: session.LiveReady, InFlightOp: session.OpCreating, Status: session.Loading,
		}},
		{"parked at a usage limit", session.LifecycleView{
			TaskRunActive: true, Started: true, Recoverable: true,
			Liveness: session.LiveLimitReached, Status: session.Ready,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !holdsTaskRunSlot(tc.view) {
				t.Fatal("an in-flight run must hold its slot in EVERY view it can be observed in; " +
					"a view that frees it is an instant at which the cap can be exceeded")
			}
		})
	}
}

// It drives real goroutines rather than asserting on the predicate's shape: the
// restore transition fires from another goroutine while admission is being
// decided, with a cap of 1. Every admission must be refused — the session is Lost
// or Running at every instant and both hold the slot, so no admission is ever
// correct.
//
// Note on strength: this is a STRESS test, not a deterministic one. The window is
// two adjacent lock acquisitions, so it is narrow — against the pre-fix two-read
// predicate it was observed failing 31 times in 2,000,000 rounds. The count below
// is sized so a reintroduced multi-read predicate fails reliably while the suite
// stays fast; against the single-snapshot predicate it cannot fail at all, since
// no interleaving can produce a view in which an in-flight run is uncounted (see
// TestInFlightRunIsCountedInEveryReachableView, which is the real invariant).
func TestCapHoldsAcrossConcurrentRestore(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const limit = 1
	data, err := createForTask(manager, repoPath, "task1", "racing", limit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()

	// The session is working, then an outage loses it mid-run: it holds its slot.
	if err := inst.Transition(session.ObserveLiveness(session.LiveRunning)); err != nil {
		t.Fatalf("running: %v", err)
	}

	const rounds = 1000000
	const mutators = 3
	var wg sync.WaitGroup
	wg.Add(mutators)
	for m := 0; m < mutators; m++ {
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				// Lost, then restored — exactly what RestoreLostSessions does, and
				// deliberately WITHOUT m.mu, which is what makes a multi-read predicate
				// racy in production.
				_ = inst.Transition(session.ObserveLiveness(session.LiveLost))
				_ = inst.Transition(session.ObserveLiveness(session.LiveRunning))
			}
		}()
	}

	// Decide admission continuously while the restore churns underneath.
	admitted := 0
	for i := 0; i < rounds; i++ {
		manager.mu.Lock()
		err := manager.admitTaskRunLocked(repo.ID, "task1", limit)
		manager.mu.Unlock()
		if err == nil {
			admitted++
		}
	}
	wg.Wait()

	if admitted != 0 {
		t.Fatalf("the cap was exceeded %d/%d times by a restore landing between the predicate's two reads: "+
			"the session is Lost or Running at every instant and both hold the slot, so no admission is ever correct", admitted, rounds)
	}

	// And the session is counted exactly once, not twice, once it settles.
	manager.mu.Lock()
	n := manager.countTaskRunsLocked(repo.ID, "task1")
	manager.mu.Unlock()
	if n != 1 {
		t.Fatalf("a single session must be counted exactly once; got %d", n)
	}
}

// TestHoldsTaskRunSlot: the cap's slot predicate is the shared activity
// projection OR an interrupted, still-restorable Lost run. Idle and archived
// release.
func TestHoldsTaskRunSlot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	newInst := func(t *testing.T, title string) *session.Instance {
		t.Helper()
		inst, err := session.NewInstance(session.InstanceOptions{
			Title: title, TaskID: "task1", Path: t.TempDir(), Program: "claude",
		})
		if err != nil {
			t.Fatalf("NewInstance: %v", err)
		}
		inst.SetStartedForTest(true)
		return inst
	}

	working := newInst(t, "working")
	if err := working.Transition(session.ObserveLiveness(session.LiveRunning)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if !holdsTaskRunSlot(working.LifecycleView()) {
		t.Fatal("a working session holds its slot")
	}

	idle := newInst(t, "idle")
	if err := idle.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if holdsTaskRunSlot(idle.LifecycleView()) {
		t.Fatal("an idle session releases its slot")
	}

	// Lost splits on whether the RUN was still in flight — the two rows are
	// indistinguishable by their current state, which is the whole reason the
	// verdict is captured on the edge into Lost.
	lostBusy := newInst(t, "lost-busy")
	if err := lostBusy.Transition(session.ObserveLiveness(session.LiveRunning)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := lostBusy.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if !holdsTaskRunSlot(lostBusy.LifecycleView()) {
		t.Fatal("a run interrupted mid-flight holds its slot: restoring it can blow the cap")
	}

	lostIdle := newInst(t, "lost-idle")
	if err := lostIdle.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := lostIdle.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if holdsTaskRunSlot(lostIdle.LifecycleView()) {
		t.Fatal("a run that had already finished must not reacquire a slot when a later outage marks it Lost")
	}

	archived := newInst(t, "archived")
	if err := archived.Transition(session.ObserveLiveness(session.LiveArchived)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if holdsTaskRunSlot(archived.LifecycleView()) {
		t.Fatal("an archived session releases its slot")
	}
}

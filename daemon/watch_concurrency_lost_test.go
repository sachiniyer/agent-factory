package daemon

import (
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
		if !canAutoRestoreLostSession(newLost(t, "lost")) {
			t.Fatal("the restore loop retries this session forever; it must keep its slot")
		}
	})

	t.Run("a tombstoned lost session releases its slot", func(t *testing.T) {
		inst := newLost(t, "killed")
		inst.MarkUserKilled()
		if canAutoRestoreLostSession(inst) {
			t.Fatal("a UserKilled record means finish-this-kill, never restore-this; it must not hold a slot")
		}
	})

	t.Run("an unstarted lost session releases its slot", func(t *testing.T) {
		inst := newLost(t, "unstarted")
		inst.SetStartedForTest(false)
		if canAutoRestoreLostSession(inst) {
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
		if canAutoRestoreLostSession(inst) {
			t.Fatal("canAutoRestoreLostSession answers only for Lost sessions")
		}
	})

	t.Run("nil is not restorable", func(t *testing.T) {
		if canAutoRestoreLostSession(nil) {
			t.Fatal("nil must not hold a slot")
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
	if !canAutoRestoreLostSession(finished) {
		t.Fatal("precondition: the restore loop does retry this session, which is exactly why the cap must ask a second question")
	}
	if holdsTaskRunSlot(finished) {
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
	if !holdsTaskRunSlot(inst) {
		t.Fatal("a run interrupted mid-flight keeps its slot: the restore loop can bring it back Running and blow the cap")
	}
	if _, err := createForTask(manager, repoPath, "task1", "busy", limit); !isAtConcurrencyLimitErr(err) {
		t.Fatalf("create while an interrupted run is restorable: want the at-limit refusal, got %v", err)
	}
}

// TestLostWhileBusyRecomputedPerEpisode: the verdict is recorded on each edge INTO
// Lost, so a session that is lost mid-run, restored, finishes, and is lost again
// is judged idle the second time. A sticky verdict would wedge the task exactly as
// the reacquire bug does, just one restore later.
func TestLostWhileBusyRecomputedPerEpisode(t *testing.T) {
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
	if !holdsTaskRunSlot(inst) {
		t.Fatal("lost mid-run holds its slot")
	}

	// A repeated Lost observation must not overwrite the verdict by reading the
	// Lost state itself — that would silently free every held slot on the next poll.
	if err := inst.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
		t.Fatalf("lost again: %v", err)
	}
	if !holdsTaskRunSlot(inst) {
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
	if holdsTaskRunSlot(inst) {
		t.Fatal("after the run finished, a later loss must not re-hold the slot")
	}
}

// TestLostWhileBusySurvivesRestart: an outage that loses sessions is the same
// event that restarts the daemon, so the verdict has to come back from disk — a
// reload that re-decided it from the Lost state alone could not tell a finished
// run from an interrupted one.
func TestLostWhileBusySurvivesRestart(t *testing.T) {
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
	if !busy.LostWhileBusy {
		t.Fatal("a session lost mid-run must persist that it was busy")
	}
	idle := newLostFrom(t, "idle", session.LiveReady)
	if idle.LostWhileBusy {
		t.Fatal("a session lost after finishing must persist that it was not busy")
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
	if !holdsTaskRunSlot(working) {
		t.Fatal("a working session holds its slot")
	}

	idle := newInst(t, "idle")
	if err := idle.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if holdsTaskRunSlot(idle) {
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
	if !holdsTaskRunSlot(lostBusy) {
		t.Fatal("a run interrupted mid-flight holds its slot: restoring it can blow the cap")
	}

	lostIdle := newInst(t, "lost-idle")
	if err := lostIdle.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := lostIdle.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if holdsTaskRunSlot(lostIdle) {
		t.Fatal("a run that had already finished must not reacquire a slot when a later outage marks it Lost")
	}

	archived := newInst(t, "archived")
	if err := archived.Transition(session.ObserveLiveness(session.LiveArchived)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if holdsTaskRunSlot(archived) {
		t.Fatal("an archived session releases its slot")
	}
}

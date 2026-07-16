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

// TestHoldsTaskRunSlot: the cap's slot predicate is the shared activity
// projection OR an auto-restorable Lost session. Idle and archived release.
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

	lost := newInst(t, "lost")
	if err := lost.Transition(session.ObserveLiveness(session.LiveLost)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if !holdsTaskRunSlot(lost) {
		t.Fatal("a restorable lost session holds its slot")
	}

	archived := newInst(t, "archived")
	if err := archived.Transition(session.ObserveLiveness(session.LiveArchived)); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if holdsTaskRunSlot(archived) {
		t.Fatal("an archived session releases its slot")
	}
}

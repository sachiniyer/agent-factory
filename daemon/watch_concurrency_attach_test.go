package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// Watching your own task run must not stall the task (#1892 + #1160).
//
// A TUI attached full-screen renews PauseStatusPoll every second against a
// three-second lease, so the daemon's liveness probe is suppressed for the whole
// attach. The cap releases a run's slot when the agent goes IDLE, and the pane is
// the only place idleness is visible — an agent that finishes a turn does not
// exit, so there is nothing else to listen for. Un-probed, a run that completed
// under the user's eyes never released its slot: the cap stayed saturated and the
// task stopped launching sessions with NO error, because from the daemon's side
// the cap was simply full.
//
// These assert on the next task session LAUNCHING, not on the internal count —
// the count is what lied.

// busyFakeBackend is an agent mid-turn: its pane keeps changing, which is what
// "still working" looks like to the only observer there is.
type busyFakeBackend struct {
	*session.FakeBackend
}

func (busyFakeBackend) HasUpdated(*session.Instance) (bool, bool, string) {
	return true, false, "working…"
}

// pausedClock drives the pause lease and the backstop deterministically instead
// of racing real sleeps, the same way the #1160 lease tests do.
func pausedClock(t *testing.T) func(time.Duration) {
	t.Helper()
	now := time.Now()
	prevNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prevNow })
	return func(d time.Duration) { now = now.Add(d) }
}

func shortBackstop(t *testing.T, d time.Duration) {
	t.Helper()
	prev := taskRunPollBackstop
	taskRunPollBackstop = d
	t.Cleanup(func() { taskRunPollBackstop = prev })
}

// TestAttachedTaskRunReleasesItsSlotOnCompletion is the regression: a task
// session that finishes WHILE a TUI is attached must release its slot, so the
// next queued event launches.
func TestAttachedTaskRunReleasesItsSlotOnCompletion(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	advance := pausedClock(t)
	shortBackstop(t, 30*time.Second)

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
	data, err := createForTask(manager, repoPath, "task1", "watched", limit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The user attaches full-screen and the TUI starts renewing the pause.
	manager.PauseStatusPoll(repo.ID, data.Title, "")
	if !manager.isPollPaused(repo.ID, data.Title, "") {
		t.Fatal("precondition: the attach must actually pause the poll")
	}

	// The agent finishes its turn. The fake backend now reports idle output —
	// exactly what a completed run looks like — but nothing has observed it yet.
	manager.RefreshStatuses()
	if _, err := createForTask(manager, repoPath, "task1", "watched", limit); !isAtConcurrencyLimitErr(err) {
		t.Fatalf("the attach's quiet window is intact, so the run is not yet known finished: want the at-limit refusal, got %v", err)
	}

	// The attach continues — renewed, so the lease never lapses. The backstop is
	// what has to save this; the pause itself never will.
	advance(taskRunPollBackstop + time.Second)
	manager.PauseStatusPoll(repo.ID, data.Title, "")
	if !manager.isPollPaused(repo.ID, data.Title, "") {
		t.Fatal("precondition: the TUI renews the lease, so the poll stays paused — this bug is not fixed by the lease lapsing")
	}
	manager.RefreshStatuses()

	// THE assertion: the next event launches. Not "the counter went down".
	if _, err := createForTask(manager, repoPath, "task1", "watched", limit); err != nil {
		t.Fatalf("a task run that finished while its user watched it must release its slot and let the next event launch; got %v", err)
	}
}

// TestAttachedTaskRunKeepsItsSlotWhileWorking is the converse: the backstop must
// not free a slot for a run that is still going. It ends runs, it does not free
// slots on a timer.
func TestAttachedTaskRunKeepsItsSlotWhileWorking(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	advance := pausedClock(t)
	shortBackstop(t, 30*time.Second)

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
	data, err := createForTask(manager, repoPath, "task1", "busy-watched", limit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()

	// A working agent: the pane keeps changing.
	inst.SetBackend(busyFakeBackend{session.NewFakeBackend()})
	manager.PauseStatusPoll(repo.ID, data.Title, "")

	for i := 0; i < 3; i++ {
		advance(taskRunPollBackstop + time.Second)
		manager.PauseStatusPoll(repo.ID, data.Title, "")
		manager.RefreshStatuses()
	}
	if !inst.TaskRunActive() {
		t.Fatal("the agent is still working: the backstop must observe, not assume")
	}
	if _, err := createForTask(manager, repoPath, "task1", "busy-watched", limit); !isAtConcurrencyLimitErr(err) {
		t.Fatalf("a run still in flight keeps its slot however long the user watches it: want the at-limit refusal, got %v", err)
	}
}

// TestAttachedTaskRunIsNeverMarkedLost: the backstop observes idleness, but it
// must never conclude DEATH. A pause outlasts remoteLostGracePeriod, so a blip
// settling Lost during an attach would hand RestoreLostSessions a session the
// user is typing into — and a remote Recover RE-PROVISIONS, orphaning the live
// sandbox and its unpushed work (#1794). The attach is positive evidence of life.
func TestAttachedTaskRunIsNeverMarkedLost(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	advance := pausedClock(t)
	shortBackstop(t, 30*time.Second)

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	data, err := createForTask(manager, repoPath, "task1", "vanishing", 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()

	// The session's tmux would probe as gone — but a TUI is attached to it, which
	// no dead session could serve. The normal poll marks this Lost; the backstop
	// must not.
	inst.SetBackend(deadTmuxBackend{session.NewFakeBackend()})
	manager.PauseStatusPoll(repo.ID, data.Title, "")
	for i := 0; i < 3; i++ {
		advance(taskRunPollBackstop + time.Second)
		manager.PauseStatusPoll(repo.ID, data.Title, "")
		manager.RefreshStatuses()
	}
	if got := inst.GetStatus(); got == session.Lost {
		t.Fatal("the backstop must never conclude death: a Lost mark here feeds a remote Recover that re-provisions the sandbox the user is attached to (#1794)")
	}
}

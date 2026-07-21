package daemon

import (
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The counter's universe (#1892).
//
// The transition table says what a task run does as its session MOVES. These
// cover a case that makes no transitions at all: a run that exists ON DISK and
// never becomes an in-memory Instance, because FromInstanceData failed — a
// vanished worktree, an unresolvable backend, a wedged tmux name.
//
// refreshDaemonInstances logs and skips such a row, so everything that walks
// m.instances behaves as though the session does not exist. The cap counted by
// walking that map, so a task whose sessions failed to load admitted replacements
// past max_concurrent_runs on every daemon restart — silently, and precisely
// against the restart-survival guarantee the cap is built on. A row we could not
// LOAD is not a run that stopped: its agent may still be up, and the persisted
// marker is the authority on whether the run is in flight.

// failLoadFor makes titles fail to materialize during refresh, exactly as a
// broken row does in production (see the "daemon skipping instance" branch).
// fromInstanceDataForRefresh is a package var for this purpose.
func failLoadFor(t *testing.T, titles ...string) {
	t.Helper()
	broken := make(map[string]bool, len(titles))
	for _, title := range titles {
		broken[title] = true
	}
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(data session.InstanceData) (*session.Instance, error) {
		if broken[data.Title] {
			return nil, errors.New("worktree path is empty")
		}
		return prev(data)
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })
}

// TestGhostTaskRunIsCountedAfterRestart is the regression: a persisted, in-flight
// task run that cannot be materialized must still hold its slot, so the cap binds
// across a daemon restart instead of being bypassed by it.
func TestGhostTaskRunIsCountedAfterRestart(t *testing.T) {
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
	data, err := createForTask(manager, repoPath, "task1", "ghost", limit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The run is in flight and persisted that way.
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()
	if !inst.TaskRunActive() {
		t.Fatal("precondition: a freshly created task session's run is in flight")
	}
	manager.persistInstance(repo.ID, inst)

	// The daemon restarts, and this row will not load.
	failLoadFor(t, data.Title)
	restarted, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager after restart: %v", err)
	}
	if err := restarted.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}

	// It is genuinely invisible to the in-memory map — that is the whole premise.
	restarted.mu.Lock()
	_, live := restarted.instances[daemonInstanceKey(repo.ID, data.Title)]
	counted := restarted.countTaskRunsLocked(repo.ID, "task1")
	restarted.mu.Unlock()
	if live {
		t.Fatal("precondition: the row must have failed to materialize for this to test anything")
	}
	if counted != 1 {
		t.Fatalf("a persisted in-flight run that failed to load must still be counted; got %d "+
			"(the cap now admits replacements beyond max_concurrent_runs after every restart)", counted)
	}

	// The consequence that matters: the cap still binds.
	restarted.mu.Lock()
	err = restarted.admitTaskRunLocked(repo.ID, "task1", limit)
	restarted.mu.Unlock()
	if !isAtConcurrencyLimitErr(err) {
		t.Fatalf("create while the task's only run is an unloadable row: want the at-limit refusal, got %v", err)
	}
}

// TestGhostTaskRunReleasesWhenItsRunIsOver: the ghost count follows the same
// persisted fact as everything else. A row whose run already FINISHED holds
// nothing, even if it fails to load — otherwise an unloadable but completed
// session would wedge a capped task forever, which is the failure mode this PR
// keeps having to avoid.
func TestGhostTaskRunReleasesWhenItsRunIsOver(t *testing.T) {
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
	data, err := createForTask(manager, repoPath, "task1", "done-ghost", limit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()

	// The run finishes, and that is what reaches disk.
	if err := inst.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatalf("ready: %v", err)
	}
	manager.persistInstance(repo.ID, inst)

	failLoadFor(t, data.Title)
	restarted, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager after restart: %v", err)
	}
	if err := restarted.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}

	restarted.mu.Lock()
	counted := restarted.countTaskRunsLocked(repo.ID, "task1")
	err = restarted.admitTaskRunLocked(repo.ID, "task1", limit)
	restarted.mu.Unlock()
	if counted != 0 {
		t.Fatalf("a finished run holds no slot, loadable or not; counted %d", counted)
	}
	if err != nil {
		t.Fatalf("a new event must land: an unloadable row whose run already finished must not wedge the task: %v", err)
	}
}

// TestStartupUnknownGhostDoesNotHoldTaskRunSlot covers contradictory rows from
// the rollout window: StartupStateUnknown is terminal even if an older writer
// left TaskRunActive set. Ghost accounting reads raw storage because the row did
// not load, so it must honor the terminal marker directly rather than wedging a
// task behind a bit no in-memory lifecycle transition can ever clear.
func TestStartupUnknownGhostDoesNotHoldTaskRunSlot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	const title = "startup-unknown-ghost"
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:                  "startup-unknown-id",
		TaskID:              "task1",
		Title:               title,
		Path:                repoPath,
		Status:              session.Lost,
		Liveness:            session.LiveLost,
		TaskRunActive:       true,
		StartupStateUnknown: true,
		BackendType:         "local",
		Worktree:            session.GitWorktreeData{RepoPath: repoPath},
	}); err != nil {
		t.Fatalf("append startup-unknown row: %v", err)
	}
	failLoadFor(t, title)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := manager.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}
	manager.mu.Lock()
	counted := manager.countTaskRunsLocked(repo.ID, "task1")
	admitErr := manager.admitTaskRunLocked(repo.ID, "task1", 1)
	manager.mu.Unlock()
	if counted != 0 {
		t.Fatalf("startup-unknown ghost consumed %d task slot(s); terminal startup outcomes must release the cap", counted)
	}
	if admitErr != nil {
		t.Fatalf("startup-unknown ghost blocked the next task event: %v", admitErr)
	}
}

// TestGhostTaskRunClearsWhenTheRowLoadsAgain: the ghost set is a projection,
// rebuilt every refresh — not bookkeeping. A row that starts loading again must
// stop being a ghost, or its slot would be held twice: once by the ghost and once
// by the instance it became.
func TestGhostTaskRunClearsWhenTheRowLoadsAgain(t *testing.T) {
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
	data, err := createForTask(manager, repoPath, "task1", "flaky", limit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, data.Title)]
	manager.mu.Unlock()
	manager.persistInstance(repo.ID, inst)

	// A transient load failure: counted as a ghost.
	restore := fromInstanceDataForRefresh
	failLoadFor(t, data.Title)
	restarted, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager after restart: %v", err)
	}
	if err := restarted.RestoreInstances(); err != nil {
		t.Fatalf("RestoreInstances: %v", err)
	}
	restarted.mu.Lock()
	ghosted := restarted.countTaskRunsLocked(repo.ID, "task1")
	restarted.mu.Unlock()
	if ghosted != 1 {
		t.Fatalf("the unloadable row must be counted once; got %d", ghosted)
	}

	// The row loads on the next refresh. It must be counted once — as an instance
	// now, not as an instance PLUS a stale ghost.
	fromInstanceDataForRefresh = restore
	restarted.mu.Lock()
	if err := restarted.refreshLocked(); err != nil {
		restarted.mu.Unlock()
		t.Fatalf("refresh: %v", err)
	}
	healed := restarted.countTaskRunsLocked(repo.ID, "task1")
	restarted.mu.Unlock()
	if healed != 1 {
		t.Fatalf("a row that loads again is counted once, not twice (ghost + instance); got %d", healed)
	}
}

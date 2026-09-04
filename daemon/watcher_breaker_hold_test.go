package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/require"
)

// The crash-loop breaker's promise (#3837). A watcher that stopped for good —
// the breaker tripped, or the script exited 0 — must stay stopped until
// somebody re-arms THAT task. Before this, every task write anywhere in the
// store ran a global reconcile that classified the terminal watcher as stale,
// dropped it, and started a fresh one with its failure count and backoff chain
// reset, so a permanently broken script spawned forever on a box with routine
// task churn (four trips, four resurrections in six hours on the dev box).

// watcherInstance returns the supervisor's current watcher object for a task,
// so a test can tell "left exactly as it was" from "replaced by a fresh one".
// Pointer identity is the assertion that matters: a replacement is precisely
// what resets failures to nil and backoff to baseBackoff.
func (s *watcherSupervisor) watcherInstance(taskID string) *taskWatcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchers[taskID]
}

// statusesFor returns the lifecycle statuses recorded for one task, in order.
// The breaker's summary is what reaches last_run_status and the TUI's failed
// rendering (ui/task_pane.go), so a resurrection shows up here as a flap.
func (r *watchRecorder) statusesFor(taskID string) []string {
	var out []string
	for _, s := range r.statusesSnapshot() {
		if strings.HasPrefix(s, taskID+":") {
			out = append(out, strings.TrimPrefix(s, taskID+":"))
		}
	}
	return out
}

// breakerHoldFixture is the issue's repro: two enabled watch tasks in a real
// (scratch) task store, A terminal and B untouched, behind the same control
// server the CLI/TUI/web writes reach.
type breakerHoldFixture struct {
	server   *controlServer
	watchers *watcherSupervisor
	rec      *watchRecorder
	a, b     task.Task
	// runsAtStop is how many times A's script had run when it went terminal.
	runsAtStop int
	// aWatcher is A's watcher object at the moment it went terminal.
	aWatcher *taskWatcher
}

// newBreakerHoldFixture seeds A (aCmd, expected to reach a terminal state) and
// B, arms both, and waits for A to stop. wantStatus is the terminal status A
// must have persisted.
func newBreakerHoldFixture(t *testing.T, aCmd, wantStatus string) *breakerHoldFixture {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	dir := t.TempDir()
	f := &breakerHoldFixture{
		a: watchTask("aaaa3837", aCmd, dir),
		b: watchTask("bbbb3837", "sleep 60", dir),
	}
	require.NoError(t, task.AddTask(f.a))
	require.NoError(t, task.AddTask(f.b))

	f.watchers, f.rec = newTestSupervisor(t, task.LoadTasks)
	f.server = &controlServer{scheduler: newTaskScheduler(), watchers: f.watchers}
	require.NoError(t, f.watchers.Reload(), "arm both watch tasks")

	waitUntil(t, 20*time.Second, "task A to reach a terminal state", func() bool {
		got := f.rec.statusesFor(f.a.ID)
		return len(got) > 0 && got[len(got)-1] == wantStatus
	})
	require.Equal(t, []string{wantStatus}, f.rec.statusesFor(f.a.ID))
	f.runsAtStop = len(f.rec.eventsSnapshot())
	f.aWatcher = f.watchers.watcherInstance(f.a.ID)
	require.NotNil(t, f.aWatcher, "a terminal watcher must stay on the books")
	return f
}

// requireHeld asserts A is exactly where the breaker left it: the same watcher
// object (so no counter was reset), no further runs, no status flap, and the
// arming surface still reporting it down.
func (f *breakerHoldFixture) requireHeld(t *testing.T, wantStatus string) {
	t.Helper()
	// Compared as raw pointers rather than through require.Same, which dumps
	// both watchers' full struct on failure and buries the message.
	require.True(t, f.aWatcher == f.watchers.watcherInstance(f.a.ID),
		"task A's watcher was replaced — its failure count and backoff chain were reset")
	require.Equal(t, task.ArmingNotArmed, f.watchers.armingFor(f.a),
		"a terminal watcher must keep reporting not-armed")
	// A resurrection restarts at baseBackoff, so a wait many times that long
	// makes a silent event stream mean "still stopped" rather than "not yet".
	time.Sleep(10 * f.watchers.baseBackoff)
	require.Equal(t, f.runsAtStop, len(f.rec.eventsSnapshot()),
		"task A's script ran again after it had stopped for good")
	require.Equal(t, []string{wantStatus}, f.rec.statusesFor(f.a.ID),
		"last_run_status flapped away from the terminal status")
}

// requireRearmed asserts the opposite: this gesture DID re-arm A.
func (f *breakerHoldFixture) requireRearmed(t *testing.T, what string) {
	t.Helper()
	waitUntil(t, 20*time.Second, what+" to re-arm task A", func() bool {
		return len(f.rec.eventsSnapshot()) > f.runsAtStop
	})
	require.False(t, f.aWatcher == f.watchers.watcherInstance(f.a.ID),
		"a re-arm must build a fresh watcher")
}

// updateOther writes an unrelated task — the exact step 3 of the issue's repro,
// `af tasks update B --prompt 'unrelated edit'` — and checks the write did what
// it was for: B is reconciled, and a delivery-field edit leaves its long-lived
// script running rather than restarting it.
func (f *breakerHoldFixture) updateOther(t *testing.T) {
	t.Helper()
	live := f.watchers.watcherInstance(f.b.ID)
	require.NotNil(t, live, "precondition: task B is watching")
	prompt := "unrelated edit"
	require.NoError(t, f.server.updateTask(context.Background(),
		UpdateTaskRequest{ID: f.b.ID, Update: task.TaskUpdate{Prompt: &prompt}},
		&UpdateTaskResponse{}))
	require.True(t, live == f.watchers.watcherInstance(f.b.ID),
		"a delivery-field edit must not restart the edited task's own script either")
}

// TestWatcherTrippedBreakerSurvivesAnUnrelatedTaskUpdate is the issue's repro at the
// control-server boundary every task write crosses.
func TestWatcherTrippedBreakerSurvivesAnUnrelatedTaskUpdate(t *testing.T) {
	const wantStatus = "errored: exit status 1"
	f := newBreakerHoldFixture(t, "echo run; exit 1", wantStatus)
	require.Equal(t, f.watchers.crashMaxExits, f.runsAtStop,
		"the breaker should have tripped after exactly crashMaxExits runs")
	f.updateOther(t)
	f.requireHeld(t, wantStatus)
}

// TestWatcherCleanExitSurvivesAnUnrelatedTaskUpdate is the same for a
// script whose contract is "exit 0 when there is nothing left to watch". There
// the resurrection is worse: the script is re-run because somebody edited
// another task.
func TestWatcherCleanExitSurvivesAnUnrelatedTaskUpdate(t *testing.T) {
	const wantStatus = "stopped"
	f := newBreakerHoldFixture(t, "echo run; exit 0", wantStatus)
	require.Equal(t, 1, f.runsAtStop)
	f.updateOther(t)
	f.requireHeld(t, wantStatus)
}

// TestWatcherTerminalStateSurvivesUnrelatedAddAndRemove covers the other two
// writers that share the post-write reconcile (AddTask, RemoveTask).
func TestWatcherTerminalStateSurvivesUnrelatedAddAndRemove(t *testing.T) {
	const wantStatus = "errored: exit status 1"
	f := newBreakerHoldFixture(t, "echo run; exit 1", wantStatus)

	added := watchTask("cccc3837", "sleep 60", f.b.ProjectPath)
	require.NoError(t, f.server.addTask(context.Background(),
		AddTaskRequest{Task: added}, &AddTaskResponse{}))
	// Narrowing the reconcile must not narrow it past the row it wrote: the new
	// task is armed by its own create, exactly as before.
	require.Contains(t, f.watchers.watchingTaskIDs(), added.ID,
		"a create must still arm the task it created")
	f.requireHeld(t, wantStatus)

	require.NoError(t, f.server.RemoveTask(RemoveTaskRequest{ID: added.ID}, &RemoveTaskResponse{}))
	require.NotContains(t, f.watchers.watchingTaskIDs(), added.ID,
		"a removal must still stop the task it removed")
	f.requireHeld(t, wantStatus)
}

// TestWatcherDisableThroughTheControlServerStopsTheWatcher pins the other half
// of "reconciles the task it wrote": a scoped write that takes its own task out
// of the desired set still disarms it, promptly and through the same stop-and-
// join path a full reload uses.
func TestWatcherDisableThroughTheControlServerStopsTheWatcher(t *testing.T) {
	const wantStatus = "errored: exit status 1"
	f := newBreakerHoldFixture(t, "echo run; exit 1", wantStatus)
	require.Contains(t, f.watchers.watchingTaskIDs(), f.b.ID, "precondition: task B is watching")

	disabled := false
	require.NoError(t, f.server.updateTask(context.Background(),
		UpdateTaskRequest{ID: f.b.ID, Update: task.TaskUpdate{Enabled: &disabled}},
		&UpdateTaskResponse{}))
	require.NotContains(t, f.watchers.watchingTaskIDs(), f.b.ID,
		"a scoped reconcile must still disarm the task it disabled")
	f.requireHeld(t, wantStatus)
}

// TestWatcherReloadTasksRPCRearmsATrippedBreaker is the first negative: the explicit
// reload poke still means what it says.
func TestWatcherReloadTasksRPCRearmsATrippedBreaker(t *testing.T) {
	f := newBreakerHoldFixture(t, "echo run; exit 1", "errored: exit status 1")
	require.NoError(t, f.server.ReloadTasks(ReloadTasksRequest{}, &ReloadTasksResponse{}))
	f.requireRearmed(t, "the ReloadTasks poke")
}

// TestWatcherExplicitReEnableRearmsATrippedBreaker is the second negative: an
// enable/disable names the task, so it re-arms it.
func TestWatcherExplicitReEnableRearmsATrippedBreaker(t *testing.T) {
	f := newBreakerHoldFixture(t, "echo run; exit 1", "errored: exit status 1")
	enabled := true
	require.NoError(t, f.server.updateTask(context.Background(),
		UpdateTaskRequest{ID: f.a.ID, Update: task.TaskUpdate{Enabled: &enabled}},
		&UpdateTaskResponse{}))
	f.requireRearmed(t, "an explicit re-enable")
}

// TestWatcherOwnScriptEditRearmsAStoppedTask is the third negative: a change
// to the watcher signature is a new process to try, terminal or not.
func TestWatcherOwnScriptEditRearmsAStoppedTask(t *testing.T) {
	f := newBreakerHoldFixture(t, "echo run; exit 1", "errored: exit status 1")
	cmd := "echo restarted; sleep 60"
	require.NoError(t, f.server.updateTask(context.Background(),
		UpdateTaskRequest{ID: f.a.ID, Update: task.TaskUpdate{WatchCmd: &cmd}},
		&UpdateTaskResponse{}))
	f.requireRearmed(t, "an edit to the task's own watch_cmd")
	require.Contains(t, f.rec.eventsSnapshot(), f.a.ID+":restarted")
}

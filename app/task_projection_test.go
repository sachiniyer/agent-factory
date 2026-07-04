package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/task"
)

// TestTaskTriggerRoutesThroughDaemonSharedPath is the regression guard for
// #1169: pressing `r` (run now) in the tasks overlay must route through the
// daemon's single shared trigger path (daemon.RunTask, via
// triggerTaskThroughDaemon) — the SAME entrypoint `af tasks trigger` and cron
// use — passing the task's own ID. The old handler unconditionally spawned a
// brand-new per-run session, ignoring target_session and orphaning it. Routing
// through RunTask makes the daemon honor target_session (deliver into it) and
// spawn a fresh session only when there is none. This test asserts the TUI no
// longer spawns a session itself and hands the right task ID to the shared path.
func TestTaskTriggerRoutesThroughDaemonSharedPath(t *testing.T) {
	h := newTestHome(t)

	var gotID string
	restore := SetTaskTriggerForTest(func(taskID string) error {
		gotID = taskID
		return nil
	})
	defer restore()

	sp := h.automations.TaskPane()
	sp.SetTasks([]task.Task{{
		ID:            "task-abc",
		Name:          "hello-task",
		CronExpr:      "0 0 * * *",
		Prompt:        "echo hi",
		TargetSession: "todo-add",
		Enabled:       true,
	}})
	sp.SelectTask(0)

	before := h.store.NumInstances()
	cmd := h.handleTaskTrigger()
	require.NotNil(t, cmd)

	// The trigger fires off-loop inside a batch; drain it to run the goroutine.
	drainCmd(t, cmd, 500*time.Millisecond)

	require.Equal(t, "task-abc", gotID,
		"run-now must route the task's own ID through the shared daemon trigger path")
	require.Equal(t, before, h.store.NumInstances(),
		"run-now must NOT spawn a divergent per-run session (#1169); the daemon owns create-or-deliver")
}

// TestTaskTriggerWatchRefused pins that a watch task still cannot be manually
// triggered (it fires from its watch command's stdout), matching daemon.RunTask.
func TestTaskTriggerWatchRefused(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)

	called := false
	restore := SetTaskTriggerForTest(func(string) error { called = true; return nil })
	defer restore()

	sp := h.automations.TaskPane()
	sp.SetTasks([]task.Task{{ID: "w1", Name: "watcher", WatchCmd: "tail -f log", Enabled: true}})
	sp.SelectTask(0)

	h.handleTaskTrigger()
	require.False(t, called, "a watch task must never reach the daemon trigger path")
	require.Contains(t, h.errBox.String(), "watch task")
}

// TestRefreshTasksLiveProjectsOutOfBandChange is the regression guard for #1168:
// a task added out-of-band (CLI/daemon `af tasks add`) must live-project into
// the running TUI — both the automations rail (store) and the tasks overlay
// (pane) — without a relaunch. refreshTasks is the poll-driven apply the
// snapshot loop calls with the freshly re-read task list.
func TestRefreshTasksLiveProjectsOutOfBandChange(t *testing.T) {
	h := newTestHome(t)
	require.Empty(t, h.store.GetTasks())
	require.Empty(t, h.automations.TaskPane().GetTasks())

	fresh := []task.Task{{
		ID:            "t1",
		Name:          "hello-task",
		CronExpr:      "0 0 * * *",
		Prompt:        "echo hi",
		TargetSession: "todo-add",
		Enabled:       true,
	}}

	require.True(t, h.refreshTasks(fresh, nil),
		"an out-of-band task add must register as a visible change")
	require.Len(t, h.store.GetTasks(), 1, "automations rail must live-project the new task")
	require.Len(t, h.automations.TaskPane().GetTasks(), 1, "tasks overlay must live-project the new task")

	// Idempotent: the same list on the next poll is not a change (no needless repaint).
	require.False(t, h.refreshTasks(fresh, nil), "an unchanged task list must not report a change")

	// A read error leaves the last-known list intact.
	require.False(t, h.refreshTasks(nil, errRefreshFailed))
	require.Len(t, h.store.GetTasks(), 1, "a failed refresh must not wipe the projected tasks")
}

// TestRefreshTasksSkipsPaneWhileCreating pins that a background refresh updates
// the rail but never clobbers the overlay pane while the user is mid-create, so
// an in-flight task form (or unsaved deletions) survive a concurrent poll.
func TestRefreshTasksSkipsPaneWhileCreating(t *testing.T) {
	h := newTestHome(t)
	sp := h.automations.TaskPane()
	sp.EnterCreateMode(t.TempDir())
	require.True(t, sp.IsCreating(), "precondition: pane is mid-create")

	fresh := []task.Task{{ID: "t1", Name: "hello", CronExpr: "0 0 * * *", Prompt: "echo hi", Enabled: true}}
	h.refreshTasks(fresh, nil)

	require.Len(t, h.store.GetTasks(), 1, "the rail must still live-project while the pane edits")
	require.Empty(t, sp.GetTasks(), "the tasks overlay must not be clobbered mid-create")
}

var errRefreshFailed = errTest("refresh failed")

type errTest string

func (e errTest) Error() string { return string(e) }

package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

func TestTaskTriggerSuccessShowsTransientNotice(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)

	_, cmd := h.Update(taskTriggeredMsg{title: "hello-task"})

	require.NotNil(t, cmd, "success notice should schedule its clear timer")
	require.Contains(t, h.errBox.String(), "triggered hello-task")
}

func TestTransientNoticeIgnoresStaleHideTimer(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)

	_, errCmd := h.Update(errTest("first failure"))
	require.NotNil(t, errCmd, "error notice should schedule its clear timer")
	oldNoticeID := h.transientNoticeID
	require.Contains(t, h.errBox.String(), "first failure")

	_, successCmd := h.Update(taskTriggeredMsg{title: "hello-task"})
	require.NotNil(t, successCmd, "success notice should schedule its clear timer")
	newNoticeID := h.transientNoticeID
	require.NotEqual(t, oldNoticeID, newNoticeID, "success must advance the notice generation")
	require.Contains(t, h.errBox.String(), "triggered hello-task")

	_, _ = h.Update(hideErrMsg{noticeID: oldNoticeID})
	require.Contains(t, h.errBox.String(), "triggered hello-task",
		"a stale timer from an older error must not clear the newer success notice")

	_, _ = h.Update(hideErrMsg{noticeID: newNoticeID})
	require.Empty(t, h.errBox.FullError(), "the current timer still clears the notice after its own delay")
}

func TestDirectTransientWriteIgnoresPriorRunNowTimer(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)

	_, runNowCmd := h.Update(taskTriggeredMsg{title: "hello-task"})
	require.NotNil(t, runNowCmd, "run-now notice should schedule its clear timer")
	runNowNoticeID := h.transientNoticeID
	require.Contains(t, h.errBox.String(), "triggered hello-task")

	_ = h.confirmAction("confirm direct error?", func() tea.Msg {
		return errTest("confirmation failed")
	})
	require.NotNil(t, h.confirmationOverlay)
	h.confirmationOverlay.OnConfirm()
	directNoticeID := h.transientNoticeID
	require.NotEqual(t, runNowNoticeID, directNoticeID,
		"direct status-bar writes must advance the notice generation too")
	require.Contains(t, h.errBox.String(), "confirmation failed")

	_, _ = h.Update(hideErrMsg{noticeID: runNowNoticeID})
	require.Contains(t, h.errBox.String(), "confirmation failed",
		"a stale run-now timer must not clear a later direct status-bar write")

	_, _ = h.Update(hideErrMsg{noticeID: directNoticeID})
	require.Empty(t, h.errBox.FullError(), "the direct write's own timer id remains authoritative")
}

// TestTaskTriggerWatchRefused pins that a watch task reaches the same daemon
// trigger seam as `af tasks trigger`; the daemon refusal is what the TUI
// surfaces, instead of the TUI taking a divergent pre-check path.
func TestTaskTriggerWatchRefused(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)

	var gotID string
	restore := SetTaskTriggerForTest(func(taskID string) error {
		gotID = taskID
		return errTest("task w1 is a watch task; it fires when its watch command emits output")
	})
	defer restore()

	sp := h.automations.TaskPane()
	sp.SetTasks([]task.Task{{ID: "w1", Name: "watcher", WatchCmd: "tail -f log", Enabled: true}})
	sp.SelectTask(0)

	cmd := h.handleTaskTrigger()
	require.NotNil(t, cmd)
	for _, msg := range drainCmd(t, cmd, 500*time.Millisecond) {
		if triggered, ok := msg.(taskTriggeredMsg); ok {
			_, _ = h.Update(triggered)
		}
	}

	require.Equal(t, "w1", gotID, "watch tasks must still route through the daemon trigger path")
	require.Contains(t, h.errBox.String(), "watch task")
}

// TestTaskTriggerDisabledMatchesDaemonCLIBehavior pins the disabled-task edge:
// the TUI still sends the selected ID to the daemon trigger seam, and surfaces
// the same refusal `af tasks trigger <id>` would return.
func TestTaskTriggerDisabledMatchesDaemonCLIBehavior(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)

	var gotID string
	restore := SetTaskTriggerForTest(func(taskID string) error {
		gotID = taskID
		return errTest("task d1 is disabled")
	})
	defer restore()

	sp := h.automations.TaskPane()
	sp.SetTasks([]task.Task{{
		ID:       "d1",
		Name:     "disabled",
		CronExpr: "0 0 * * *",
		Prompt:   "do it",
		Enabled:  false,
	}})
	sp.SelectTask(0)

	cmd := h.handleTaskTrigger()
	require.NotNil(t, cmd)
	for _, msg := range drainCmd(t, cmd, 500*time.Millisecond) {
		if triggered, ok := msg.(taskTriggeredMsg); ok {
			_, _ = h.Update(triggered)
		}
	}

	require.Equal(t, "d1", gotID)
	require.Contains(t, h.errBox.String(), "disabled")
}

func TestTaskOverlayRunNowWithoutSelectionShowsFeedback(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	h.state = stateTasks

	sp := h.automations.TaskPane()
	sp.SetTasks(nil)
	sp.SetFocus(true)

	_, cmd := h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})

	require.NotNil(t, cmd, "no-selection feedback should schedule its clear timer")
	require.Contains(t, h.errBox.String(), "no task selected")
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

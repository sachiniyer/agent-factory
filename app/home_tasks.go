package app

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// handleTaskCreate processes a pending task creation from the inline form.
func (m *home) handleTaskCreate() tea.Cmd {
	sp := m.automations.TaskPane()
	name, prompt, cronExpr, watchCmd, targetSession, projectPath, program := sp.ConsumePendingCreate()

	if name == "" {
		return m.handleNotice(fmt.Errorf("task name is required"))
	}
	// Re-validate the trigger contract behind the form (#782): exactly one of
	// cron / watch cmd, and cron tasks need a prompt — there is no event line
	// to fall back to. Mirrors `af tasks add` (api/tasks.go).
	hasCron := cronExpr != ""
	hasWatch := watchCmd != ""
	if hasCron == hasWatch {
		return m.handleNotice(fmt.Errorf("exactly one of cron or watch cmd is required"))
	}
	if hasCron {
		if strings.TrimSpace(prompt) == "" {
			return m.handleNotice(fmt.Errorf("prompt must be non-empty"))
		}
		if err := task.ValidateCronExpr(cronExpr); err != nil {
			return m.handleNotice(fmt.Errorf("invalid cron: %v", err))
		}
	}
	// ResolveUserPath expands a leading ~ before absolutizing — filepath.Abs
	// alone would turn "~/project" into "<cwd>/~/project" (#924). validateForm
	// already normalized the field, so this is idempotent.
	//
	// handleError, unlike the form-validation branches above. ResolveUserPath is
	// ExpandTilde + filepath.Abs, and Abs's only failure is os.Getwd() — the TUI's
	// working directory was removed or became unreadable. Nothing about the typed
	// path caused that, and nothing the user retypes fixes it, so it is an
	// operation failure. ("invalid path" is the user-facing half; the severity is
	// the monitoring half, and they answer different questions.)
	absPath, err := config.ResolveUserPath(projectPath)
	if err != nil {
		return m.handleError(fmt.Errorf("invalid path: %v", err))
	}
	if program == "" {
		program = m.program
	}
	id, err := task.GenerateID()
	if err != nil {
		return m.handleError(fmt.Errorf("failed to generate task id: %v", err))
	}
	t := task.Task{
		ID:            id,
		Name:          name,
		Prompt:        prompt,
		CronExpr:      cronExpr,
		WatchCmd:      watchCmd,
		TargetSession: targetSession,
		ProjectPath:   absPath,
		Program:       program,
		Enabled:       true,
		CreatedAt:     time.Now(),
	}
	// Route the create through the daemon (#1029 PR 6): it is the sole writer of
	// tasks.json among clients (#960) and re-arms its own scheduler/watchers in
	// the same RPC, so there is no separate ReloadTasks poke. The task-file write
	// is the durable commit; a later refresh failure is classified so this caller
	// can accept the saved task without presenting a retryable create form.
	addErr := addTaskThroughDaemon(t)
	if addErr != nil && !apiclient.IsMutationCommitted(addErr) {
		return m.handleError(fmt.Errorf("failed to save task: %v", addErr))
	}
	// Refresh sidebar and task pane
	tasks, err := task.LoadTasksForCurrentRepo()
	if err == nil {
		m.store.SetTasks(tasks)
		sp.SetTasks(tasks)
		// Reflow so the new automation grows the rail's section (#1126).
		m.relayout()
	}
	if addErr != nil {
		return m.handleError(fmt.Errorf("task was saved, but the daemon could not refresh its schedules: %w", addErr))
	}
	return nil
}

// taskTriggeredMsg reports the outcome of a TUI "run now" (#1169). The run
// itself happens daemon-side (create-or-deliver + status write); the resulting
// session and updated task status live-project back into the TUI, so success
// needs no on-loop mutation — only a failure is surfaced to the user.
type taskTriggeredMsg struct {
	title string
	err   error
}

// handleTaskTrigger runs the selected task through the daemon's single shared
// trigger path — via the TriggerTask RPC, which calls daemon.RunTask, the SAME
// entrypoint `af tasks trigger` and the cron scheduler use. Previously the TUI
// "run now" unconditionally spawned a new per-run session, ignoring the task's
// target_session and orphaning it (#1169); routing through RunTask makes it
// honor target_session (deliver into it, auto-creating when missing) and spawn
// a fresh session only when there is no target — matching CLI/cron exactly. The
// daemon owns the create/deliver, so the new/updated session appears via the
// Snapshot projection and the task's run status via the task refresh, with no
// divergent TUI spawn path to drift.
func (m *home) handleTaskTrigger() tea.Cmd {
	sp := m.automations.TaskPane()
	tsk := sp.ConsumePendingTrigger()
	if tsk == nil {
		return m.handleNotice(fmt.Errorf("no task selected"))
	}

	// The trigger fires from inside the tasks overlay: close it and move focus to
	// the tree so the user is looking at the sessions, where the run lands (a
	// fresh per-run session, or the delivered-into target_session).
	if m.state == stateTasks {
		m.state = stateDefault
		sp.SetFocus(false)
	}
	m.focusRegion(layout.RegionTree)

	taskID := tsk.ID
	taskTitle := task.TaskRunBaseTitle(*tsk)
	// Capture the trigger seam on the event loop before the goroutine reads it, so
	// a concurrent test-seam swap can't race the read (#960 PR 4 race-fix class).
	trigger := triggerTaskThroughDaemon
	triggerCmd := func() tea.Msg {
		if err := trigger(taskID); err != nil {
			return taskTriggeredMsg{title: taskTitle, err: err}
		}
		return taskTriggeredMsg{title: taskTitle}
	}

	return tea.Batch(tea.WindowSize(), m.selectionChanged(), triggerCmd)
}

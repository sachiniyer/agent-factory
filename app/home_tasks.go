package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// handleTaskCreate processes a pending task creation from the inline form.
func (m *home) handleTaskCreate() tea.Cmd {
	sp := m.automations.TaskPane()
	draft := sp.ConsumePendingCreate()
	name, prompt := draft.Name, draft.Prompt
	cronExpr, watchCmd := draft.Cron, draft.WatchCmd
	targetSession, projectPath, program := draft.TargetSession, draft.Path, draft.Program
	committed := false
	defer func() {
		if !committed {
			sp.RestoreCreateMode()
		}
	}()

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
	absPath, err := ui.ResolveTaskProjectPath(projectPath)
	if err != nil {
		return m.handleError(fmt.Errorf("invalid path: %v", err))
	}

	// An empty program field is "Use config default". When the launch itself
	// committed to a program — an explicit --program (programFollowsConfig is
	// false) — THAT is the default the label refers to, so the task stores it
	// and later runs keep launching it regardless of config drift (#4889
	// review). Only a config-following launch leaves the stored program empty,
	// so task/task.go's runner resolves the live default_program against the
	// task's own repo at RUN time; folding m.defaultProgram() into that case
	// would bake a possibly-stale cache in permanently.
	if program == "" && !m.programFollowsConfig {
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
		// The spawned-session lifecycle the form collected (#2595). Already in
		// its stored form ("" for keep, and "" whenever the draft names a target
		// session), so a task created here is byte-identical to one created
		// before the field was reachable unless the user chose otherwise.
		OnComplete: draft.OnComplete,
		Host:       draft.Host,
		// The concurrency cap the form collected (#4180), in the same stored
		// form: 0 for unlimited, and 0 whenever the draft's shape cannot carry
		// one.
		MaxConcurrentRuns: draft.MaxConcurrentRuns,
		ProjectPath:       absPath,
		Program:           program,
		Enabled:           true,
		CreatedAt:         time.Now(),
	}
	// Route the create through the daemon (#1029 PR 6): it is the sole writer of
	// tasks.json among clients (#960) and re-arms its own scheduler/watchers in
	// the same RPC, so there is no separate ReloadTasks poke. The task-file write
	// is the durable commit; a later refresh failure is classified so this caller
	// can accept the saved task without presenting a retryable create form.
	addErr := addTaskThroughDaemon(t)
	m.noteRemoteCreate(t, addErr)
	// Only a definitive refusal re-arms the form. A task that may have been
	// saved — committed, or a reply lost after the daemon received it (#4820) —
	// closes the form like a success, so the user cannot submit a duplicate, and
	// the list below is re-read from disk to show whether it landed.
	if addErr != nil && !mutationMayHaveLanded(addErr) {
		return m.showRecovery("Cannot save task", "Your input is retained. "+addErr.Error(), "Press any key to return to the form.", fmt.Errorf("failed to save task: %v", addErr))
	}
	committed = true
	// Refresh sidebar and task pane
	// A reload failure is NOT nothing: the save committed, so the list on screen is
	// now stale, and returning nil tells the user it is current. They then read a
	// list missing the task they just created and reasonably conclude the save
	// failed — so they create it again (#3095). Surfaced the way
	// saveContentPaneState already surfaces the same reload.
	//
	// Scope the reload by the TUI's ACTIVE project (m.repoRoot/m.repoID), not the
	// process cwd, so a task created after an in-place project switch (#1461)
	// shows up in the switched-to project's pane rather than being clobbered by
	// the launch repo's rows. The cross-repo create case — a path pointing at a
	// different repo than the active scope — is intentionally outside this list
	// and is surfaced below.
	var reloadErr error
	tasks, err := m.activeTasks(m.repoRoot, m.repoID)
	if err == nil {
		// Like refreshTasks, a successful read clears load errors separately
		// from replacing the task data and its transient pane state.
		sp.SetUnavailable(nil)
		m.store.SetTasks(tasks)
		sp.SetTasks(tasks)
		// Reflow so the new automation grows the rail's section (#1126).
		m.relayout()
	} else {
		reloadErr = fmt.Errorf("the task was saved, but this list could not be reloaded, so it is showing the tasks from before — do not create it again: %w", err)
		if addErr != nil && !apiclient.IsMutationCommitted(addErr) {
			reloadErr = fmt.Errorf("this list could not be reloaded, so it is showing the tasks from before — wait for it to refresh before creating the task again: %w", err)
		}
	}
	// The binding is committed whatever the reload did, so when it lands outside
	// the active scope the destination is known even though the list could not be
	// re-read to show its absence. It rides along with a reload failure instead of
	// being dropped (#5667): the user is told the save committed, and must also
	// learn it is in another project.
	var bindingNotice error
	if reloadErr != nil && (addErr == nil || apiclient.IsMutationCommitted(addErr)) && boundOutsideActiveScope(m.repoID, absPath) {
		bindingNotice = fmt.Errorf("task %q was saved and bound to %s; it does not appear in this project's list", name, absPath)
	}
	if addErr != nil && apiclient.IsMutationCommitted(addErr) {
		err := errors.Join(
			fmt.Errorf("task was saved, but the daemon could not refresh its schedules: %w", addErr),
			reloadErr, bindingNotice)
		// A cross-repo create that committed with a warning still landed in
		// another project, so the active-scope reload omits it — the scheduler
		// warning alone reads as a failed save that lost the task. The nil-error
		// cross-repo branch below names where it landed; surface the same
		// destination here so a committed-with-warning create does not regress
		// it. Only when the reload succeeded: a reload failure leaves the list
		// stale and the absence unreadable.
		if reloadErr == nil {
			for _, t := range tasks {
				if t.ID == id {
					return m.handleError(err)
				}
			}
			err = errors.Join(err,
				fmt.Errorf("task %q was saved and bound to %s; it does not appear in this project's list", name, absPath))
		}
		return m.handleError(err)
	}
	if addErr != nil {
		return m.handleError(errors.Join(
			mutationOutcomeError(fmt.Sprintf("saving task %q", name), "the task list", addErr),
			reloadErr))
	}
	if reloadErr != nil {
		return m.handleError(errors.Join(reloadErr, bindingNotice))
	}
	// Cross-repo binding notice. A committed save whose owning project differs
	// from the active scope is omitted from the reloaded list above (the same
	// scope-matching the snapshot poll uses), so the pane closes as it opened
	// for that task and the user reads the unchanged list as a failed save and
	// creates the task again — the same #3095 duplicate-create hazard the
	// reload-failure branch above calls out. The reload succeeded, so the
	// list IS current; what it lacks is the task, because the task landed in
	// another repo. Mirror the CLI's project_path echo (#1891, "the id alone
	// left the caller no way to tell which project the task attached to") by
	// naming where this one landed. Same treatment for the registry-mode
	// create (empty active scope): the task is bound, but no project is
	// selected to display it in.
	for _, t := range tasks {
		if t.ID == id {
			return nil
		}
	}
	return m.handleNotice(fmt.Errorf("task %q was saved and bound to %s; it does not appear in this project's list", name, absPath))
}

// boundOutsideActiveScope reports whether a task bound to projectPath is omitted
// from the active project's task list: registry mode (no active scope) or a
// path that resolves to a different repo. Used when the list could not be
// reloaded to show the absence directly (#5667).
func boundOutsideActiveScope(activeRepoID, projectPath string) bool {
	if isRemoteTarget() {
		// The path is on the daemon's host; this machine cannot resolve it to a
		// repo, so only registry mode is known to be out of scope.
		return activeRepoID == ""
	}
	return activeRepoID == "" || config.RepoIDForPath(projectPath) != activeRepoID
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
	// Pin the project binding of the record the user fired from (#3230): the
	// daemon re-verifies it against the same load that produces the fired
	// record, so a "run now" on a stale row cannot fire a task another client
	// rebound to a different project.
	expect := task.ExpectProject(*tsk)
	// Capture the trigger seam on the event loop before the goroutine reads it, so
	// a concurrent test-seam swap can't race the read (#960 PR 4 race-fix class).
	trigger := triggerTaskThroughDaemon
	triggerCmd := func() tea.Msg {
		if err := trigger(taskID, expect); err != nil {
			return taskTriggeredMsg{title: taskTitle, err: err}
		}
		return taskTriggeredMsg{title: taskTitle}
	}

	return tea.Batch(tea.WindowSize(), m.selectionChanged(), triggerCmd)
}

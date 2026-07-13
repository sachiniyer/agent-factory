package app

import (
	"fmt"
	"os"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/overlay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleStateSelectProgramSelectsEnum verifies that selecting an agent
// from the program overlay writes the bare enum to pendingProgram. Under the
// enum-only program model (#658), m.program itself is an enum, so the old
// "preserve user's full path-and-flags when re-selecting the matching
// agent" branch is gone — every selection is just the canonical agent name.
func TestHandleStateSelectProgramSelectsEnum(t *testing.T) {
	h := newTestHome(t)
	h.program = tmux.ProgramClaude
	h.selectionOverlay = overlay.NewSelectionOverlay("Select Program", tmux.SupportedPrograms)
	h.selectionOverlay.SetSelectedIndex(0) // claude
	h.state = stateSelectProgram

	_, _ = h.handleStateSelectProgram(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, tmux.ProgramClaude, h.pendingProgram)
}

// TestHandleStateSelectProgramSwitchesAgent verifies that switching from the
// configured default to a different agent enum updates pendingProgram to the
// new selection.
func TestHandleStateSelectProgramSwitchesAgent(t *testing.T) {
	h := newTestHome(t)
	h.program = tmux.ProgramClaude
	h.selectionOverlay = overlay.NewSelectionOverlay("Select Program", tmux.SupportedPrograms)
	// Walk to codex (index 1 in SupportedPrograms).
	h.selectionOverlay.SetSelectedIndex(1)
	h.state = stateSelectProgram

	_, _ = h.handleStateSelectProgram(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, tmux.ProgramCodex, h.pendingProgram)
}

func TestPaneOverlaysQuitWithReboundKeyBeforeEditFieldsConsumeIt(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(map[string][]string{"quit": {"Q"}}))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	t.Run("tasks create form", func(t *testing.T) {
		h := newTestHome(t)
		tp := h.automations.TaskPane()
		tp.SetFocus(true)
		tp.EnterCreateMode("/tmp/repo")
		h.state = stateTasks

		_, cmd := h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Q")})

		assert.True(t, reachesQuit(cmd), "rebound quit must quit before the task form types it")
	})

	t.Run("hooks add form", func(t *testing.T) {
		h := newTestHome(t)
		h.hooksPane.SetFocus(true)
		h.state = stateHooks
		_, _ = h.handleStateHooks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})

		_, cmd := h.handleStateHooks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Q")})

		assert.True(t, reachesQuit(cmd), "rebound quit must quit before the hooks form types it")
	})
}

func TestTaskOverlayQuitKeysBypassFocusedCreateForm(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(nil))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	for _, tc := range []struct {
		name string
		msg  tea.KeyMsg
	}{
		{name: "configured quit", msg: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}},
		{name: "ctrl+c hard quit", msg: tea.KeyMsg{Type: tea.KeyCtrlC}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			tp := h.automations.TaskPane()
			tp.SetFocus(true)
			tp.EnterCreateMode("/tmp/repo")
			h.state = stateTasks

			_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("draft")})
			_, cmd := h.handleStateTasks(tc.msg)

			assert.True(t, reachesQuit(cmd), "%s must quit before the form consumes it", tc.name)
			assert.True(t, tp.IsCreating(), "quit routing must not first cancel the form")
		})
	}
}

// TestHandleStateTasks_PendingCreateFlushesDirtyTaskState is the regression
// guard for #578, re-homed to the tasks overlay (#1096 play-test moved the
// manager out of the rail).
//
// The bug: toggling a task with 'x' marks the TaskPane dirty in memory but
// the toggle is not yet on disk. Submitting the inline create form sets
// pendingCreate WITHOUT releasing focus, so the "save on close" branch in
// the overlay handler does not run. handleTaskCreate then writes the new
// task to disk and calls LoadTasksForCurrentRepo + SetTasks, which overwrites
// the in-memory TaskPane with stale disk state and clears `dirty` — silently
// discarding the toggle.
//
// The fix is one extra m.saveContentPaneState() call before m.handleTaskCreate()
// in app/handle_overlay.go so dirty toggles/edits/deletes hit disk before the
// reload.
//
// We assert directly on tasks.json after driving the handler with the same
// key sequence a user would press. handleTaskCreate's daemon reload poke is
// stubbed by newTestHome, so the only side effect under test is the disk
// write: the on-disk Enabled bit must reflect the user's toggle. Without the
// fix it would still be `true` on disk because saveContentPaneState never ran.
func TestHandleStateTasks_PendingCreateFlushesDirtyTaskState(t *testing.T) {
	h := newTestHome(t)

	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	// Seed an existing, enabled task on disk and load it into the TaskPane —
	// this is the equivalent of opening the app with one task configured.
	existing := task.Task{
		ID:          "existing-578",
		Name:        "nightly",
		Prompt:      "do something",
		CronExpr:    "* * * * *",
		ProjectPath: repo.Root,
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now(),
	}
	require.NoError(t, task.AddTask(existing))

	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	tp := h.automations.TaskPane()
	tp.SetTasks(loaded)
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	// The overlay now opens the task straight into its edit form (#1249); Esc
	// steps back to the list to drive the list-mode keys below.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})

	// User presses 'x' to toggle the task off — dirty in memory, not yet on disk.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	require.True(t, tp.IsDirty(), "toggle must mark the pane dirty")
	require.False(t, tp.GetTasks()[0].Enabled, "in-memory state reflects the toggle")

	diskBefore, err := task.LoadTasks()
	require.NoError(t, err)
	require.True(t, diskBefore[0].Enabled,
		"disk must still hold the pre-toggle value until something flushes the pane")

	// User opens the inline create form with 'n' and fills it in.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	require.True(t, tp.IsCreating(), "'n' must open the inline create form")

	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new-task")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> trigger selector (cron stays selected)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> cron value
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("* * * * *")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do other thing")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> target session
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> path
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> program
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> save button

	// Submit. This sets pendingCreate inside TaskPane and then the overlay
	// handler's HasPendingCreate branch runs — which is the code path the fix
	// modifies. We only care that the toggle is on disk by the time the dust
	// settles.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	diskAfter, err := task.LoadTasks()
	require.NoError(t, err)
	var found *task.Task
	for i := range diskAfter {
		if diskAfter[i].ID == existing.ID {
			found = &diskAfter[i]
			break
		}
	}
	require.NotNil(t, found, "existing task must still be on disk after the create flow")
	assert.False(t, found.Enabled,
		"toggle must be persisted to disk before handleTaskCreate reloads "+
			"(regression for #578: handler now calls saveContentPaneState first)")
}

// TestHandleStateTasks_ValidationFailureLeavesTaskPaneStale is the
// regression guard for #934, re-homed to the tasks overlay.
//
// The bug: saveContentPaneState swallowed task.UpdateTask/RemoveTask errors
// (log-only), cleared the TaskPane's dirty flag unconditionally via
// ConsumeDeleted, and reloaded ONLY the sidebar from disk. On a save failure
// the user's edit was silently dropped, the TaskPane kept stale in-memory
// state while the sidebar showed disk state (divergence), and dirty was
// cleared so the user couldn't tell anything went wrong.
//
// The fix's chosen recovery semantics (documented on saveContentPaneState):
// reload BOTH panes from disk so they can never diverge, and surface the
// persist failure via handleError so the dropped edit is never silent. We do
// NOT keep dirty=true — after reloading from disk the in-memory edit is gone,
// so a lingering dirty flag would point at nothing.
//
// We inject a real UpdateTask failure by making the config dir unwritable
// after seeding: the file-lock/atomic-write both need to create files in that
// dir, so the persist fails, while reads (LoadTasksForCurrentRepo) still
// succeed and return the committed disk state.
func TestHandleStateTasks_ValidationFailureLeavesTaskPaneStale(t *testing.T) {
	h := newTestHome(t)

	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	// Seed an existing, enabled task on disk and load it into the TaskPane.
	existing := task.Task{
		ID:          "existing-934",
		Name:        "nightly",
		Prompt:      "do something",
		CronExpr:    "* * * * *",
		ProjectPath: repo.Root,
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now(),
	}
	require.NoError(t, task.AddTask(existing))

	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	tp := h.automations.TaskPane()
	tp.SetTasks(loaded)
	h.store.SetTasks(loaded)
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	h.errBox.SetSize(500, 1)
	// The overlay now opens the task straight into its edit form (#1249); Esc
	// steps back to the list to drive the list-mode keys below.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})

	// User presses 'x' to toggle the task off — dirty in memory, not yet saved.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	require.True(t, tp.IsDirty(), "toggle must mark the pane dirty")
	require.False(t, tp.GetTasks()[0].Enabled, "in-memory state reflects the toggle")

	// Make the persist fail: strip write permission from the config dir so the
	// file lock / atomic write that UpdateTask performs cannot create files,
	// while existing files stay readable (read+execute bits retained).
	configDir, err := config.GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.Chmod(configDir, 0o500))
	// Restore before the tempdir cleanup so RemoveAll can delete the dir.
	t.Cleanup(func() { _ = os.Chmod(configDir, 0o700) })

	// Pressing Esc releases the manager's focus, which closes the overlay and
	// triggers saveContentPaneState. The UpdateTask write fails; the handler
	// surfaces it via handleError.
	_, cmd := h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state, "Esc must close the tasks overlay")
	require.NotNil(t, cmd, "a failed save must return an error-surfacing command")

	// (a) The user is notified — the failure is surfaced inline, not swallowed.
	assert.NotEmpty(t, h.errBox.String(),
		"BUG: save failure must be surfaced to the user, not silently swallowed")

	// (b) TaskPane and sidebar agree, and both reflect committed disk state.
	disk, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	require.Len(t, disk, 1)
	assert.True(t, disk[0].Enabled,
		"the failed write must not have changed disk: it still holds the pre-toggle value")
	require.Len(t, tp.GetTasks(), 1)
	assert.True(t, tp.GetTasks()[0].Enabled,
		"BUG: TaskPane must reload from disk after a failed save, not keep its stale toggle")
	require.Len(t, h.store.GetTasks(), 1)
	assert.True(t, h.store.GetTasks()[0].Enabled,
		"sidebar must agree with the TaskPane (both reflect disk)")

	// (c) State is not left silently "saved": reloading cleared dirty, but the
	// error surfaced above means the user knows the edit was dropped. A
	// lingering dirty flag would point at edits the reload already discarded.
	assert.False(t, tp.IsDirty(),
		"reloading from disk clears dirty; the dropped edit is communicated via the error, not a dangling dirty flag")
}

// TestHandleStateTasks_PendingTriggerSurvivesDeleteFailureReloadByID covers
// #1474: after a delete fails, saveContentPaneState reloads the task pane from
// disk. A pending run-now intent must still target the task selected when `r`
// was pressed, not whatever task lands at the old selected index after reload.
func TestHandleStateTasks_PendingTriggerSurvivesDeleteFailureReloadByID(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(500, 1)

	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	taskA := task.Task{
		ID: "task-a-1474", Name: "Task A", Prompt: "p", CronExpr: "* * * * *",
		ProjectPath: repo.Root, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	taskB := task.Task{
		ID: "task-b-1474", Name: "Task B", Prompt: "p", CronExpr: "* * * * *",
		ProjectPath: repo.Root, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	require.NoError(t, task.AddTask(taskA))
	require.NoError(t, task.AddTask(taskB))

	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	tp := h.automations.TaskPane()
	tp.SetTasks(loaded)
	h.store.SetTasks(loaded)
	_, _ = h.showTasksOverlay()
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})

	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	require.Len(t, tp.GetTasks(), 1)
	require.Equal(t, "task-b-1474", tp.GetTasks()[0].ID)

	t.Cleanup(SetTaskRemoverForTest(func(string) error {
		return fmt.Errorf("daemon RPC failure")
	}))

	_, cmd := h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	require.NotNil(t, cmd)
	assert.Contains(t, h.errBox.String(), "failed to remove task")
	require.True(t, tp.HasPendingTrigger())
	require.Len(t, tp.GetTasks(), 2)
	require.Equal(t, "task-a-1474", tp.GetTasks()[0].ID)
	require.Equal(t, "task-b-1474", tp.GetTasks()[1].ID)

	var triggeredID string
	t.Cleanup(SetTaskTriggerForTest(func(id string) error {
		triggeredID = id
		return nil
	}))

	_, cmd = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	drainCmd(t, cmd, 500*time.Millisecond)

	assert.Equal(t, "task-b-1474", triggeredID,
		"pending trigger must resolve by task ID after the failed-delete reload")
}

// TestHandleStateTasks_FailedCreateDoesNotDuplicateOnReopen covers #1531: when
// a create is submitted but its pre-create flush (saveContentPaneState) fails,
// handleTaskCreate never runs and pendingCreate is left set. Pre-fix, that flag
// survived the overlay close, so reopening the manager (which drops straight
// into the selected task's edit form, #1249) and pressing any key fired
// HasPendingCreate() → handleTaskCreate() against the reloaded buffers,
// duplicating the selected task. The fix clears the stuck pending create on the
// failed-flush reload (SetTasks) and on overlay close (SetFocus(false)), so no
// phantom task is created.
func TestHandleStateTasks_FailedCreateDoesNotDuplicateOnReopen(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(500, 1)

	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	existing := task.Task{
		ID: "existing-1531", Name: "nightly", Prompt: "do something",
		CronExpr: "* * * * *", ProjectPath: repo.Root, Program: "claude",
		Enabled: true, CreatedAt: time.Now(),
	}
	require.NoError(t, task.AddTask(existing))

	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	tp := h.automations.TaskPane()
	tp.SetTasks(loaded)
	h.store.SetTasks(loaded)

	// Record every create routed through the daemon RPC seam.
	var creates []task.Task
	t.Cleanup(SetTaskAdderForTest(func(tk task.Task) error {
		creates = append(creates, tk)
		return task.AddTask(tk)
	}))

	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc}) // edit form -> list

	// Toggle the task so the pre-create flush has dirty state to persist.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	require.True(t, tp.IsDirty(), "toggle must mark the pane dirty")

	// Force the pre-create flush to fail, so the create is submitted but
	// handleTaskCreate never runs and pendingCreate is stranded.
	restoreUpdater := SetTaskUpdaterForTest(func(string, task.TaskUpdate) error {
		return fmt.Errorf("daemon RPC failure")
	})

	// Fill and submit the inline create form.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	require.True(t, tp.IsCreating(), "'n' must open the inline create form")
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("dup-task")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> trigger selector
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> cron value
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("* * * * *")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("dup prompt")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> target session
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> path
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> program
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> save button
	_, cmd := h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "the failed pre-create flush must surface an error")
	require.Empty(t, creates, "the create must not have persisted — its flush failed")

	// The transient failure clears; a later flush would succeed.
	restoreUpdater()

	// Esc closes the overlay.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state, "Esc must close the tasks overlay")

	// Reopen (drops into the selected task's edit form) and press a key that
	// keeps focus so the HasPendingCreate branch is reached. Pre-fix this fired
	// handleTaskCreate against the reloaded buffers and duplicated the task.
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})

	assert.Empty(t, creates,
		"reopening after a failed create must NOT create a duplicate task (#1531)")
	disk, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	assert.Len(t, disk, 1, "only the original task remains — no phantom duplicate")
}

// TestHandleTaskCreate_RoutesThroughDaemonRPC is the #1029 PR 6 guard for the
// create path: the TUI inline create form must persist the new task through the
// daemon RPC — the sole writer of tasks.json among clients (#960) — not by
// writing the file directly. We swap the add seam for a recorder and assert (a)
// it received the composed task and (b) nothing touched tasks.json on disk, so
// the TUI no longer originates a task write.
func TestHandleTaskCreate_RoutesThroughDaemonRPC(t *testing.T) {
	h := newTestHome(t)

	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	var got *task.Task
	calls := 0
	// Overrides newTestHome's default (direct disk writer) with a recorder.
	t.Cleanup(SetTaskAdderForTest(func(tk task.Task) error {
		calls++
		got = &tk
		return nil
	}))

	tp := h.automations.TaskPane()
	tp.SetTasks(nil)
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)

	// Fill and submit the inline create form (the same key path a user drives).
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	require.True(t, tp.IsCreating(), "'n' must open the inline create form")
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new-task")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> trigger selector (cron stays selected)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> cron value
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("* * * * *")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do a thing")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> target session
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> path
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> program
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> save button
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	// (a) The create was dispatched through the daemon seam exactly once, with
	// the task the user composed.
	require.Equal(t, 1, calls, "create must route through the daemon RPC, not a direct disk write")
	require.NotNil(t, got)
	assert.Equal(t, "new-task", got.Name)
	assert.Equal(t, "* * * * *", got.CronExpr)

	// (b) The TUI wrote nothing to disk: with the daemon stubbed out, the only
	// writer, tasks.json holds no task.
	disk, err := task.LoadTasks()
	require.NoError(t, err)
	assert.Empty(t, disk, "TUI must not write tasks.json directly (#960 sole writer)")
}

// TestSaveContentPaneState_RoutesThroughDaemonRPC is the #1029 PR 6 guard for
// the edit/delete path: closing the task manager with dirty edits must persist
// them through the daemon RPCs (update + remove), not by writing tasks.json
// directly. We seed two tasks, toggle one and delete the other, swap the update
// and remove seams for recorders, and assert both were dispatched with the right
// payloads and disk was left untouched.
func TestSaveContentPaneState_RoutesThroughDaemonRPC(t *testing.T) {
	h := newTestHome(t)

	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	keep := task.Task{
		ID: "keep-1029", Name: "keep", Prompt: "p", CronExpr: "* * * * *",
		ProjectPath: repo.Root, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	gone := task.Task{
		ID: "gone-1029", Name: "gone", Prompt: "p", CronExpr: "* * * * *",
		ProjectPath: repo.Root, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	require.NoError(t, task.AddTask(keep))
	require.NoError(t, task.AddTask(gone))

	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	require.Len(t, loaded, 2)
	tp := h.automations.TaskPane()
	tp.SetTasks(loaded)
	h.store.SetTasks(loaded)
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	// The overlay now opens the task straight into its edit form (#1249); Esc
	// steps back to the list to drive the list-mode keys below.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})

	// Swap the write seams for recorders AFTER seeding (the seed used the direct
	// writer); the save-on-close must now dispatch through these instead of disk.
	var updated []task.TaskEdit
	var removed []string
	t.Cleanup(SetTaskUpdaterForTest(func(id string, update task.TaskUpdate) error {
		updated = append(updated, task.TaskEdit{ID: id, Update: update})
		return nil
	}))
	t.Cleanup(SetTaskRemoverForTest(func(id string) error { removed = append(removed, id); return nil }))

	// Toggle the first task (keep) off — dirty, not yet persisted.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	// Move to the second task (gone) and delete it.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	require.True(t, tp.IsDirty(), "toggle + delete must mark the pane dirty")

	// Esc releases focus → the overlay closes and saveContentPaneState runs.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state)

	// The remaining task's update and the deleted task's remove both routed
	// through the daemon seams.
	require.Len(t, removed, 1, "delete must route through the remove RPC")
	assert.Equal(t, "gone-1029", removed[0])
	var sawToggledKeep bool
	for _, edit := range updated {
		if edit.ID == "keep-1029" {
			sawToggledKeep = true
			require.NotNil(t, edit.Update.Enabled, "the toggle must ship the Enabled field")
			assert.False(t, *edit.Update.Enabled, "the toggled state must be dispatched to the update RPC")
		}
	}
	assert.True(t, sawToggledKeep, "the surviving task must route through the update RPC")

	// The TUI wrote nothing to disk: with the daemon stubbed out, tasks.json
	// still holds both seeded tasks, both enabled.
	disk, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, disk, 2, "TUI must not write tasks.json directly (#960 sole writer)")
	for _, tk := range disk {
		assert.True(t, tk.Enabled, "the TUI is no longer a tasks.json writer, so disk stays untouched")
	}
}

// TestSaveContentPaneState_DoesNotClobberUneditedTask is the regression guard
// for #1213: saving a dirty task pane must persist ONLY the tasks the user
// actually edited, never the whole pane. Before the fix, saveContentPaneState
// iterated every task in the pane and wrote each back, so a stale pane copy of
// an untouched task silently overwrote a change another writer (CLI, daemon)
// committed while the pane was open — a lost update.
//
// Scenario: the user opens the manager and toggles Task A (only A is dirty).
// While the pane is open, the CLI changes Task B's prompt on disk. Closing the
// manager must land A's toggle without reverting B's out-of-band change.
func TestSaveContentPaneState_DoesNotClobberUneditedTask(t *testing.T) {
	h := newTestHome(t)

	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	// newTestHome points the write seams at the direct disk writers, so the
	// save-on-close and the simulated CLI write both land in the same tasks.json.
	taskA := task.Task{
		ID: "task-a-1213", Name: "Task A", Prompt: "prompt A", CronExpr: "* * * * *",
		ProjectPath: repo.Root, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	taskB := task.Task{
		ID: "task-b-1213", Name: "Task B", Prompt: "prompt B original", CronExpr: "* * * * *",
		ProjectPath: repo.Root, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	require.NoError(t, task.AddTask(taskA))
	require.NoError(t, task.AddTask(taskB))

	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	require.Len(t, loaded, 2)
	tp := h.automations.TaskPane()
	tp.SetTasks(loaded)
	h.store.SetTasks(loaded)
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	// The overlay now opens the task straight into its edit form (#1249); Esc
	// steps back to the list to drive the list-mode keys below.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})

	// User toggles Task A (index 0) off — only A is dirty; B is never touched.
	tp.SelectTask(0)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	require.True(t, tp.IsDirty(), "toggling A must mark the pane dirty")

	// The CLI changes Task B on disk while the pane is open, holding a stale B.
	newPrompt := "CLI MODIFIED"
	_, err = task.UpdateTask("task-b-1213", task.TaskUpdate{Prompt: &newPrompt})
	require.NoError(t, err)

	// Esc releases focus → the overlay closes and saveContentPaneState runs.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state)

	disk, err := task.LoadTasks()
	require.NoError(t, err)
	byID := map[string]task.Task{}
	for _, tk := range disk {
		byID[tk.ID] = tk
	}

	// A's edit landed.
	gotA, ok := byID["task-a-1213"]
	require.True(t, ok)
	assert.False(t, gotA.Enabled, "the toggled task A must be persisted")

	// B's out-of-band CLI change survives — the untouched task was NOT written
	// back from the stale pane copy (the #1213 lost update).
	gotB, ok := byID["task-b-1213"]
	require.True(t, ok)
	assert.Equal(t, "CLI MODIFIED", gotB.Prompt,
		"unedited Task B must keep the concurrent CLI change, not be clobbered by the stale pane copy")
}

// dirtyHooksHome returns a home wired to a real repo with a single dirty,
// unsaved hook edit ("echo test") in the HooksPane. The hooks-save seam is
// left at the caller's discretion (default real save, or a stub).
func dirtyHooksHome(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)

	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID
	h.repoRoot = repo.Root

	hp := h.hooksPane
	hp.SetCommands([]string{})
	// Host the editor as the modal overlay it now lives in (#1024 PR 4).
	h.state = stateHooks
	hp.SetFocus(true)
	h.errBox.SetSize(500, 1)

	// Add a hook: dirty in memory, not yet persisted.
	hp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	hp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("echo test")})
	hp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, hp.IsDirty(), "hooks should be dirty after edit")
	require.Contains(t, hp.GetCommands(), "echo test")
	return h
}

// stubHooksSaveFailure forces the hooks-save seam to fail and restores it after
// the test. Returns the sentinel error it injects.
func stubHooksSaveFailure(t *testing.T) error {
	t.Helper()
	wantErr := fmt.Errorf("injected hooks save failure")
	orig := saveInRepoPostWorktreeCommandsFn
	saveInRepoPostWorktreeCommandsFn = func(string, []string) error { return wantErr }
	t.Cleanup(func() { saveInRepoPostWorktreeCommandsFn = orig })
	return wantErr
}

// TestSaveContentPaneState_HooksSaveFailureReturnedAndPreserved is the core
// regression guard for #1001. Before the fix, SaveInRepoPostWorktreeCommands
// failures were only logged and saveContentPaneState returned nil, so the
// caller never aborted the destructive action — the edit was lost silently.
//
// The fix returns the error AND deliberately leaves the HooksPane dirty (no
// disk reload, unlike tasks) so the in-memory edit survives for the user to
// retry.
func TestSaveContentPaneState_HooksSaveFailureReturnedAndPreserved(t *testing.T) {
	h := dirtyHooksHome(t)
	wantErr := stubHooksSaveFailure(t)
	hp := h.hooksPane

	err := h.saveContentPaneState()
	require.Error(t, err, "hooks save failure must be returned, not swallowed (#1001)")
	assert.ErrorIs(t, err, wantErr)

	assert.True(t, hp.IsDirty(),
		"hooks pane must stay dirty so the edit survives for retry (#1001)")
	assert.Contains(t, hp.GetCommands(), "echo test",
		"the in-memory edit must be preserved, not reloaded away")
}

// TestHandleStateHooks_HooksSaveFailureSurfacedOnEsc reproduces the exact
// path from the issue's failing test: pressing Esc closes the hooks overlay,
// which triggers the save, and a hooks-save failure must return an
// error-surfacing command and show the error inline (previously cmd was nil
// and the errBox stayed empty).
func TestHandleStateHooks_HooksSaveFailureSurfacedOnEsc(t *testing.T) {
	h := dirtyHooksHome(t)
	stubHooksSaveFailure(t)
	hp := h.hooksPane

	_, cmd := h.handleStateHooks(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state, "Esc must close the hooks overlay")
	require.NotNil(t, cmd, "a failed hooks save must return an error-surfacing command (#1001)")
	assert.Contains(t, h.errBox.String(), "failed to save hooks",
		"the hooks save failure must be surfaced to the user, not swallowed (#1001)")
	assert.True(t, hp.IsDirty(), "the dropped edit must be preserved for retry (#1001)")
}

// TestHandleQuit_HooksSaveFailureAbortsQuit verifies the destructive path: a
// hooks-save failure must abort the quit (surface the error via handleError)
// rather than proceeding to tea.Quit and losing the edit. The errBox being set
// is the discriminator — the tea.Quit path never touches it.
func TestHandleQuit_HooksSaveFailureAbortsQuit(t *testing.T) {
	h := dirtyHooksHome(t)
	stubHooksSaveFailure(t)
	hp := h.hooksPane

	_, cmd := h.handleQuit()
	require.NotNil(t, cmd, "a failed save must return a command (the handleError cmd)")
	assert.Contains(t, h.errBox.String(), "failed to save hooks",
		"quit must be aborted with the error surfaced, not proceed to tea.Quit (#1001)")
	assert.True(t, hp.IsDirty(), "the edit must survive the aborted quit for retry (#1001)")
}

// TestHandleQuit_HooksSaveSuccessQuitsAndUpdatesHookCount verifies the success
// path is unchanged: a clean save proceeds to tea.Quit and the sidebar hook
// count reflects the saved edit.
func TestHandleQuit_HooksSaveSuccessQuitsAndUpdatesHookCount(t *testing.T) {
	h := dirtyHooksHome(t) // default seam performs a real, successful save

	_, cmd := h.handleQuit()
	require.NotNil(t, cmd, "handleQuit returns a command on the success path")
	assert.NotContains(t, h.errBox.String(), "failed to save hooks",
		"no error must be surfaced on a successful save")

	assert.True(t, commandEmitsQuit(cmd), "a successful save must proceed to tea.Quit")

	assert.Equal(t, 1, h.store.GetHookCount(),
		"the sidebar hook count must reflect the saved hook")

	// The save actually landed on disk.
	cfg, _, err := config.LoadInRepoConfig(h.repoRoot)
	require.NoError(t, err)
	assert.Equal(t, []string{"echo test"}, cfg.PostWorktreeCommands,
		"the hook edit must be persisted to the in-repo config")
}

// TestSaveContentPaneState_HooksAndTaskFailuresBothSurfaced verifies that when
// BOTH panes are dirty and BOTH fail, neither error is dropped — the hooks fix
// must not clobber the task error (and vice versa).
func TestSaveContentPaneState_HooksAndTaskFailuresBothSurfaced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based write-failure injection is bypassed when running as root")
	}
	h := dirtyHooksHome(t)
	wantHooksErr := stubHooksSaveFailure(t)

	// Seed a task on disk and load it, then toggle it dirty.
	existing := task.Task{
		ID:          "existing-1001",
		Name:        "nightly",
		Prompt:      "do something",
		CronExpr:    "* * * * *",
		ProjectPath: h.repoRoot,
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now(),
	}
	require.NoError(t, task.AddTask(existing))
	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	tp := h.automations.TaskPane()
	tp.SetTasks(loaded)
	_, _ = h.showTasksOverlay()
	// The overlay now opens the task straight into its edit form (#1249); Esc
	// steps back to the list where `x` toggles enabled and marks it dirty.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	require.True(t, tp.IsDirty(), "toggle must mark the task pane dirty")

	// Make the task persist fail too: strip write permission from the config dir.
	configDir, err := config.GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.Chmod(configDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(configDir, 0o700) })

	err = h.saveContentPaneState()
	require.Error(t, err, "both failures must surface")
	assert.ErrorIs(t, err, wantHooksErr, "the hooks error must not be dropped")
	assert.Contains(t, err.Error(), "failed to save task",
		"the task error must not be dropped when hooks also fail")
}

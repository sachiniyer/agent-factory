package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
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

// TestHandleContentPaneFocus_PendingCreateFlushesDirtyTaskState is the
// regression guard for #578.
//
// The bug: toggling a task with 'x' marks the TaskPane dirty in memory but
// the toggle is not yet on disk. Submitting the inline create form sets
// pendingCreate WITHOUT releasing focus, so the "save on Esc" branch in
// handleContentPaneFocus does not run. handleTaskCreate then writes the new
// task to disk and calls LoadTasksForCurrentRepo + SetTasks, which overwrites
// the in-memory TaskPane with stale disk state and clears `dirty` — silently
// discarding the toggle.
//
// The fix is one extra m.saveContentPaneState() call before m.handleTaskCreate()
// in app/handle_overlay.go so dirty toggles/edits/deletes hit disk before the
// reload.
//
// We assert directly on tasks.json after driving the handler with the same
// key sequence a user would press. handleTaskCreate also calls
// task.InstallScheduler under the hood, which shells out to `systemctl --user`
// — that step is flaky in CI containers without a user systemd manager.
// Independent of whether InstallScheduler succeeds:
//   - on success, LoadTasksForCurrentRepo + SetTasks reload the (already-saved)
//     toggle from disk;
//   - on failure, handleTaskCreate rolls back the new task via RemoveTask, but
//     saveContentPaneState has already persisted the toggle.
//
// Either way the on-disk Enabled bit must reflect the user's toggle. Without
// the fix it would still be `true` on disk because saveContentPaneState never
// ran. We redirect HOME so scheduler writes land in a tempdir, which keeps the
// test from polluting the developer's real systemd-user units even on the
// platforms where the systemctl call would succeed against a real session.
func TestHandleContentPaneFocus_PendingCreateFlushesDirtyTaskState(t *testing.T) {
	h := newTestHome(t)
	// Keep any task.InstallScheduler/RemoveScheduler file writes inside the
	// test tempdir so a real user-systemd doesn't end up with stale units
	// pointing into deleted tempdirs after the test exits.
	t.Setenv("HOME", t.TempDir())

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
	tp := h.contentPane.TaskPane()
	tp.SetTasks(loaded)
	h.contentPane.SetMode(ui.ContentModeTasks)
	tp.SetFocus(true)

	// User presses 'x' to toggle the task off — dirty in memory, not yet on disk.
	_, _, consumed := h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	require.True(t, consumed, "'x' must route through the focus handler")
	require.True(t, tp.IsDirty(), "toggle must mark the pane dirty")
	require.False(t, tp.GetTasks()[0].Enabled, "in-memory state reflects the toggle")

	diskBefore, err := task.LoadTasks()
	require.NoError(t, err)
	require.True(t, diskBefore[0].Enabled,
		"disk must still hold the pre-toggle value until something flushes the pane")

	// User opens the inline create form with 'n' and fills it in.
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	require.True(t, tp.IsCreating(), "'n' must open the inline create form")

	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new-task")})
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do other thing")})
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyTab}) // -> cron
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("* * * * *")})
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyTab}) // -> path
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyTab}) // -> program
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyTab}) // -> save button

	// Submit. This sets pendingCreate inside TaskPane and then the focus
	// handler's HasPendingCreate branch runs — which is the code path the fix
	// modifies. We don't care whether handleTaskCreate's downstream
	// InstallScheduler ultimately succeeds; we only care that the toggle is on
	// disk by the time the dust settles.
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyEnter})

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

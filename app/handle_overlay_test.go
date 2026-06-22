package app

import (
	"os"
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
// key sequence a user would press. handleTaskCreate's daemon reload poke is
// stubbed by newTestHome, so the only side effect under test is the disk
// write: the on-disk Enabled bit must reflect the user's toggle. Without the
// fix it would still be `true` on disk because saveContentPaneState never ran.
func TestHandleContentPaneFocus_PendingCreateFlushesDirtyTaskState(t *testing.T) {
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
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyTab}) // -> trigger selector (cron stays selected)
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyTab}) // -> cron value
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("* * * * *")})
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do other thing")})
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyTab}) // -> target session
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyTab}) // -> path
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyTab}) // -> program
	_, _, _ = h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyTab}) // -> save button

	// Submit. This sets pendingCreate inside TaskPane and then the focus
	// handler's HasPendingCreate branch runs — which is the code path the fix
	// modifies. We only care that the toggle is on disk by the time the dust
	// settles.
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

// TestHandleContentPaneFocus_ValidationFailureLeavesTaskPaneStale is the
// regression guard for #934.
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
func TestHandleContentPaneFocus_ValidationFailureLeavesTaskPaneStale(t *testing.T) {
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
	tp := h.contentPane.TaskPane()
	tp.SetTasks(loaded)
	h.sidebar.SetTasks(loaded)
	h.contentPane.SetMode(ui.ContentModeTasks)
	tp.SetFocus(true)
	h.errBox.SetSize(500, 1)

	// User presses 'x' to toggle the task off — dirty in memory, not yet saved.
	_, _, consumed := h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	require.True(t, consumed, "'x' must route through the focus handler")
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

	// Pressing Esc releases focus, which triggers saveContentPaneState. The
	// UpdateTask write fails; the handler surfaces it via handleError.
	_, cmd, consumed := h.handleContentPaneFocus(tea.KeyMsg{Type: tea.KeyEsc})
	require.True(t, consumed, "Esc must route through the focus handler")
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
	require.Len(t, h.sidebar.GetTasks(), 1)
	assert.True(t, h.sidebar.GetTasks()[0].Enabled,
		"sidebar must agree with the TaskPane (both reflect disk)")

	// (c) State is not left silently "saved": reloading cleared dirty, but the
	// error surfaced above means the user knows the edit was dropped. A
	// lingering dirty flag would point at edits the reload already discarded.
	assert.False(t, tp.IsDirty(),
		"reloading from disk clears dirty; the dropped edit is communicated via the error, not a dangling dirty flag")
}

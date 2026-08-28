package ui

// Pane-level guards for the project compare-and-swap every task mutation must
// carry (#3230) — split from task_pane_test.go for the file-length lint (#1145).

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTaskPaneConsumeDirtyPinsLoadedProjectBinding is the pane-level guard for
// #3230: every edit the pane emits must carry a project compare-and-swap built
// from the record the pane LOADED, so the daemon refuses the patch if another
// client rebound the task while the pane was open. A zero-value expectation is
// exactly the bug — it disables the daemon-side check (task.ProjectExpectation
// zero value means "no expectation"), which is what the TUI sent before this.
//
// The fixture retargets project_path through the edit form on purpose: the
// expectation must pin the ORIGINAL stored path (what the user's authorization
// was based on), never the edited value — pinning the edited value would make
// the CAS compare the new binding against itself and always pass.
func TestTaskPaneConsumeDirtyPinsLoadedProjectBinding(t *testing.T) {
	oldRepo, newRepo := newGitRepo(t), newGitRepo(t)
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "* * * * *",
		ProjectPath: oldRepo,
		Program:     "claude",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.EnterEditSelected()
	require.True(t, tp.IsEditing())

	tp.editPath.SetValue(newRepo)
	require.True(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}))
	require.False(t, tp.IsEditing(), "a successful save closes the form")

	dirty := tp.ConsumeDirty()
	require.Len(t, dirty, 1)
	assert.True(t, dirty[0].Expect.Enforce,
		"the edit must enforce the project CAS — an unenforced expectation silently disables the daemon-side check (#3230)")
	assert.Equal(t, oldRepo, dirty[0].Expect.ProjectPath,
		"the expectation must pin the path the pane loaded, not the edited value")
}

// TestTaskPaneDeleteAfterEditQueuesLoadedRecord covers the composite the Codex
// review caught on #3230's PR: retarget a task's project through the edit form,
// then delete it before the pane is saved. The queued deletion must carry the
// record as LOADED — the edited copy's new path was never persisted, so
// pinning it would make the daemon compare that phantom binding against the
// still-stored old path and refuse the delete with no concurrent writer at all.
func TestTaskPaneDeleteAfterEditQueuesLoadedRecord(t *testing.T) {
	oldRepo, newRepo := newGitRepo(t), newGitRepo(t)
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "* * * * *",
		ProjectPath: oldRepo,
		Program:     "claude",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.EnterEditSelected()
	require.True(t, tp.IsEditing())
	tp.editPath.SetValue(newRepo)
	require.True(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}))
	require.False(t, tp.IsEditing(), "a successful save closes the form")

	// Delete the just-edited task while its edit is still unsaved.
	require.True(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")}))

	deleted := tp.ConsumeDeleted()
	require.Len(t, deleted, 1)
	assert.Equal(t, "abc", deleted[0].ID)
	assert.Equal(t, oldRepo, deleted[0].ProjectPath,
		"the queued deletion must carry the LOADED binding — the edited path was never stored, so pinning it falsely refuses the delete")
	assert.Empty(t, tp.ConsumeDirty(),
		"the deleted task's pending edit must not also be dispatched as an update")
}

// TestTaskPaneConsumeDirtyExpectBaselineAdvancesAfterSave: after a save is
// acknowledged, a later edit's expectation must pin the NEW stored binding —
// the baseline the daemon now holds — or every follow-up edit of a just-moved
// task would be refused as stale.
func TestTaskPaneConsumeDirtyExpectBaselineAdvancesAfterSave(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{ID: "a", Name: "A", ProjectPath: "/repo/one", Enabled: true}})
	tp.SetFocus(true)

	// Toggle, consume, acknowledge — the daemon committed the edit.
	require.True(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	first := tp.ConsumeDirty()
	require.Len(t, first, 1)
	assert.Equal(t, "/repo/one", first[0].Expect.ProjectPath)
	tp.AcknowledgeSavedEdit("a")

	// Toggle again: the new baseline is the acknowledged record.
	require.True(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	second := tp.ConsumeDirty()
	require.Len(t, second, 1)
	assert.True(t, second[0].Expect.Enforce)
	assert.Equal(t, "/repo/one", second[0].Expect.ProjectPath,
		"the post-save baseline still pins the stored binding")
}

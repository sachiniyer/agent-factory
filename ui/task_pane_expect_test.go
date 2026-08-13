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

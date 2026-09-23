package app

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #4798 through the app: a held draft of a task another writer deleted used to
// fail its save with "not found" on every overlay close, forever. The save's
// own reload now shows the task is gone, so the pane drops the draft, the save
// reports no error, and the next close sends nothing.
func TestSaveContentPaneStateReconcilesDraftOfDeletedTask(t *testing.T) {
	h := newTestHome(t)
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	demo := task.Task{
		ID: "demo-4798", Name: "demo", Prompt: "p", CronExpr: "0 3 * * *",
		ProjectPath: repo.Root, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	require.NoError(t, task.AddTask(demo))
	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	sp := h.automations.TaskPane()
	sp.SetTasks(loaded)
	h.store.SetTasks(loaded)

	sp.SetFocus(true)
	require.True(t, sp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	sp.SetFocus(false)
	require.True(t, sp.IsDirty(), "precondition: an edit is held")

	// `af tasks remove` from a shell, before the draft is saved.
	require.NoError(t, task.RemoveTask(demo.ID, task.ProjectExpectation{}))
	var updates []string
	t.Cleanup(SetTaskUpdaterForTest(func(id string, _ task.TaskUpdate, _ task.ProjectExpectation) error {
		updates = append(updates, id)
		return fmt.Errorf("task with id %q not found", id)
	}))

	require.NoError(t, h.saveContentPaneState(),
		"a draft the reload dropped must not also be reported as a failed save")
	assert.Nil(t, h.recovery, "the draft is gone, so no recovery screen may claim it is retained")
	assert.Equal(t, []string{demo.ID}, updates)
	assert.Empty(t, sp.GetTasks())
	assert.False(t, sp.IsDirty())
	assert.Equal(t, `Discarded unsaved edits to "demo" — the task was deleted`, sp.TakeDiscardedDraftNotice())

	require.NoError(t, h.saveContentPaneState())
	assert.Len(t, updates, 1, "a later close must not retry the save")
}

// The background refresh reaches the same state without a save: the draft is
// dropped the first time the poll's list lacks its task.
func TestRefreshTasksDropsDraftOfDeletedTask(t *testing.T) {
	h := newTestHome(t)
	sp := h.automations.TaskPane()
	a := task.Task{ID: "a", Name: "demo", Prompt: "p", CronExpr: "0 0 * * *", Enabled: true}
	b := task.Task{ID: "b", Name: "b", Prompt: "p", CronExpr: "0 0 * * *", Enabled: true}
	sp.SetTasks([]task.Task{a, b})
	h.store.SetTasks([]task.Task{a, b})
	sp.SetFocus(true)
	require.True(t, sp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	sp.SetFocus(false)

	require.True(t, h.refreshTasks([]task.Task{b}, nil))
	assert.Equal(t, []string{"b"}, taskIDs(sp.GetTasks()))
	assert.False(t, sp.IsDirty())
	assert.Equal(t, `Discarded unsaved edits to "demo" — the task was deleted`, sp.TakeDiscardedDraftNotice())
	assert.False(t, h.refreshTasks([]task.Task{b}, nil), "the pane matches disk again")
}

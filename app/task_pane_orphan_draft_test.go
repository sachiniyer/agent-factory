package app

import (
	"errors"
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin #4798 through the app: a held draft of a task
// another writer deleted used to fail its save with "not found" on every
// overlay close, forever. Only a positive not-found from the update drops the
// draft; every other failure keeps it and reports it exactly as before.

const orphanDraftNotice = `Discarded unsaved edits to "demo" — the task was deleted`

// homeWithHeldDraft returns a home whose task pane holds an unsaved edit
// (Enabled toggled off) to a task named "demo". When onDisk is false the task
// was never in the store, as if another writer had already removed it.
func homeWithHeldDraft(t *testing.T, onDisk bool) (*home, *ui.TaskPane, task.Task) {
	t.Helper()
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
	loaded := []task.Task{demo}
	if onDisk {
		require.NoError(t, task.AddTask(demo))
		loaded, err = task.LoadTasksForCurrentRepo()
		require.NoError(t, err)
	}
	sp := h.automations.TaskPane()
	sp.SetTasks(loaded)
	h.store.SetTasks(loaded)
	sp.SetFocus(true)
	require.True(t, sp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	sp.SetFocus(false)
	require.True(t, sp.IsDirty(), "precondition: an edit is held")
	return h, sp, demo
}

// The daemon's answer arrives as text over the RPC; the in-process writer's is
// a *task.NotFoundError. Both are a positive not-found: the save drops the
// draft, reports nothing, shows no "changes are retained" screen, and the next
// close sends no update.
func TestSaveContentPaneStateDiscardsDraftOnPositiveNotFound(t *testing.T) {
	type taskUpdater = func(string, task.TaskUpdate, task.ProjectExpectation) error
	for name, deleteAndFail := range map[string]func(*testing.T, task.Task) taskUpdater{
		"daemon message": func(t *testing.T, demo task.Task) taskUpdater {
			require.NoError(t, task.RemoveTask(demo.ID, task.ProjectExpectation{}))
			return func(id string, _ task.TaskUpdate, _ task.ProjectExpectation) error {
				return fmt.Errorf("task with id %q not found", id)
			}
		},
		"store error": func(t *testing.T, demo task.Task) taskUpdater {
			require.NoError(t, task.RemoveTask(demo.ID, task.ProjectExpectation{}))
			return func(id string, update task.TaskUpdate, expect task.ProjectExpectation) error {
				_, err := task.UpdateTask(id, update, expect)
				return err
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, sp, demo := homeWithHeldDraft(t, true)
			updater := deleteAndFail(t, demo)
			var updates []string
			t.Cleanup(SetTaskUpdaterForTest(func(id string, u task.TaskUpdate, e task.ProjectExpectation) error {
				updates = append(updates, id)
				return updater(id, u, e)
			}))

			require.NoError(t, h.saveContentPaneState(),
				"a draft proved deleted must not also be reported as a failed save")
			assert.Nil(t, h.recovery, "the draft is gone, so no recovery screen may claim it is retained")
			assert.Equal(t, []string{demo.ID}, updates)
			assert.NotContains(t, taskIDs(sp.GetTasks()), demo.ID)
			assert.False(t, sp.IsDirty())
			assert.Equal(t, orphanDraftNotice, sp.TakeDiscardedDraftNotice())

			require.NoError(t, h.saveContentPaneState())
			assert.Len(t, updates, 1, "a later close must not retry the save")
		})
	}
}

// A generic failure is not a not-found, even when the task is absent from the
// store — the recovery scene's shape. The draft stays, the error surfaces, and
// the recovery screen says the changes are retained, exactly as before #4798.
func TestSaveContentPaneStateKeepsDraftOnGenericFailure(t *testing.T) {
	h, sp, demo := homeWithHeldDraft(t, false)
	t.Cleanup(SetTaskUpdaterForTest(func(string, task.TaskUpdate, task.ProjectExpectation) error {
		return errors.New("The daemon refused this save.")
	}))

	err := h.saveContentPaneState()
	require.ErrorContains(t, err, "The daemon refused this save.")
	assert.True(t, sp.IsDirty(), "the draft must stay retryable")
	require.Equal(t, []string{demo.ID}, taskIDs(sp.GetTasks()), "absence from the reload must not drop the draft")
	assert.False(t, sp.GetTasks()[0].Enabled)
	require.NotNil(t, h.recovery)
	assert.Equal(t, "Cannot save task", h.recovery.condition)
	assert.Empty(t, sp.TakeDiscardedDraftNotice())
}

// Anything short of the daemon's own not-found for this task keeps the draft:
// a not-found naming another task, and a not-found carried by a response the
// client could not confirm came from the daemon.
func TestSaveContentPaneStateKeepsDraftWithoutPositiveNotFound(t *testing.T) {
	for name, failure := range map[string]error{
		"other task": fmt.Errorf("task with id %q not found", "someone-else"),
		"unconfirmed response": fmt.Errorf("%w: task with id %q not found",
			&apiclient.UnconfirmedHTTPResponseError{Route: "/rpc/UpdateTask", Status: 404}, "demo-4798"),
		"transport": &apiclient.TransportError{Err: fmt.Errorf("task with id %q not found", "demo-4798")},
	} {
		t.Run(name, func(t *testing.T) {
			h, sp, demo := homeWithHeldDraft(t, false)
			t.Cleanup(SetTaskUpdaterForTest(func(string, task.TaskUpdate, task.ProjectExpectation) error {
				return failure
			}))

			require.Error(t, h.saveContentPaneState())
			assert.True(t, sp.IsDirty())
			assert.Equal(t, []string{demo.ID}, taskIDs(sp.GetTasks()))
			assert.Empty(t, sp.TakeDiscardedDraftNotice())
		})
	}
}

// Quitting right after the save drops a draft would exit before the snapshot
// poll raises the notice, so quit shows it and stays; the next quit goes.
func TestHandleQuitShowsDiscardedDraftNoticeBeforeExiting(t *testing.T) {
	h, sp, demo := homeWithHeldDraft(t, true)
	require.NoError(t, task.RemoveTask(demo.ID, task.ProjectExpectation{}))
	t.Cleanup(SetTaskUpdaterForTest(func(id string, _ task.TaskUpdate, _ task.ProjectExpectation) error {
		return fmt.Errorf("task with id %q not found", id)
	}))

	_, _ = h.handleQuit()
	require.False(t, h.quitting, "the first quit must stop to show the notice")
	text, failure := h.errBox.RetainedNotice()
	assert.Contains(t, text, `Discarded unsaved edits to "demo"`)
	assert.False(t, failure, "a dropped draft is a notice, not a failure")
	assert.Empty(t, sp.TakeDiscardedDraftNotice(), "the notice is raised once")

	_, _ = h.handleQuit()
	assert.True(t, h.quitting, "the next quit goes through")
}

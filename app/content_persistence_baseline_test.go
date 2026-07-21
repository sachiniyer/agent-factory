package app

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2324: a successful daemon update followed by a failed reload must still
// advance the TaskPane's diff baseline. Otherwise toggling the same field back
// to its original value looks like an empty diff and the second edit is lost.
func TestSaveContentPaneStateAdvancesSuccessfulEditWhenReloadFails(t *testing.T) {
	h, pane := taskPaneWithEnabledTask(t)
	var updates []task.TaskUpdate
	t.Cleanup(SetTaskUpdaterForTest(func(_ string, update task.TaskUpdate) error {
		updates = append(updates, update)
		return nil
	}))

	require.True(t, pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	err := h.saveContentPaneState()
	require.ErrorContains(t, err, "failed to reload tasks after save")
	require.Len(t, updates, 1)
	require.NotNil(t, updates[0].Enabled)
	assert.False(t, *updates[0].Enabled)

	// The first write committed false. Toggling back to true must therefore be
	// a real second patch even though true was the pane's initial value.
	require.True(t, pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	err = h.saveContentPaneState()
	require.ErrorContains(t, err, "failed to reload tasks after save")
	require.Len(t, updates, 2,
		"a stale pre-save baseline incorrectly drops the edit that returns to the original value")
	require.NotNil(t, updates[1].Enabled)
	assert.True(t, *updates[1].Enabled)
}

// A failed write and failed reload is the opposite edge of #2324: the pane
// must not advance its baseline or clear its only retry signal. The user-facing
// error aborts focus release/quit, so keeping the edit dirty lets that retry
// persist exactly the patch that did not commit.
func TestSaveContentPaneStateKeepsFailedEditDirtyWhenReloadFails(t *testing.T) {
	h, pane := taskPaneWithEnabledTask(t)
	attempts := 0
	t.Cleanup(SetTaskUpdaterForTest(func(_ string, update task.TaskUpdate) error {
		attempts++
		require.NotNil(t, update.Enabled)
		assert.False(t, *update.Enabled)
		return fmt.Errorf("daemon update failed")
	}))

	require.True(t, pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	err := h.saveContentPaneState()
	require.ErrorContains(t, err, "failed to save task")
	require.ErrorContains(t, err, "failed to reload tasks after save")
	assert.True(t, pane.IsDirty(), "an unpersisted edit needs to remain retryable when reload cannot recover it")

	err = h.saveContentPaneState()
	require.ErrorContains(t, err, "failed to save task")
	assert.Equal(t, 2, attempts, "the second save must retry the unpersisted patch")
}

func taskPaneWithEnabledTask(t *testing.T) (*home, taskPaneSaveSurface) {
	t.Helper()
	h := newTestHome(t)
	pane := h.automations.TaskPane()
	pane.SetTasks([]task.Task{{ID: "task-2324", Name: "task", Enabled: true}})
	pane.SetFocus(true)

	// saveContentPaneState's production reload resolves the current Git repo.
	// A fresh non-repo directory makes that final read fail deterministically,
	// after the daemon update seam has reported its independent outcome.
	t.Chdir(t.TempDir())
	return h, pane
}

// taskPaneSaveSurface keeps the helper's return type limited to the production
// methods the regression drives, rather than exposing TaskPane internals here.
type taskPaneSaveSurface interface {
	HandleKeyPress(tea.KeyMsg) bool
	IsDirty() bool
}

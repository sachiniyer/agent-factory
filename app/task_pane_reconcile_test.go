package app

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin #4487 through the app: the TaskPane reload runs
// whenever the rail's does, and the pane keeps what the user is holding.

func taskIDs(tasks []task.Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	return ids
}

// #4257, and #4488 against it: with an edit retained by a failed save, a delete
// that fails must leave its task in the pane exactly once — on the first
// failure and on the next — without retrying the removal behind the user's
// back. The pane used to skip the reload while an edit was retained, so the task
// vanished, and restoring it by hand appended a copy per failure (#4283).
func TestSaveContentPaneState_RepeatedFailedDeleteDuringFailedEditShowsTaskOnce(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(500, 1)
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	keep := task.Task{
		ID: "keep-4257", Name: "keep", Prompt: "p", CronExpr: "* * * * *",
		ProjectPath: repo.Root, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	gone := task.Task{
		ID: "gone-4257", Name: "gone", Prompt: "p", CronExpr: "* * * * *",
		ProjectPath: repo.Root, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
	require.NoError(t, task.AddTask(keep))
	require.NoError(t, task.AddTask(gone))
	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	tp := h.automations.TaskPane()
	tp.SetTasks(loaded)
	h.store.SetTasks(loaded)

	var updates, removes []string
	restoreUpdater := SetTaskUpdaterForTest(func(id string, _ task.TaskUpdate, _ task.ProjectExpectation) error {
		updates = append(updates, id)
		return fmt.Errorf("update daemon RPC failure")
	})
	t.Cleanup(restoreUpdater)
	restoreRemover := SetTaskRemoverForTest(func(id string, _ task.ProjectExpectation) error {
		removes = append(removes, id)
		return fmt.Errorf("remove daemon RPC failure")
	})
	t.Cleanup(restoreRemover)

	press := func(k string) { _, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}) }
	esc := func() { _, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc}) }

	for attempt := 1; attempt <= 2; attempt++ {
		// The overlay opens on the rail's task in its edit form; Esc steps
		// back to the list.
		_, _ = h.showTasksOverlay()
		esc()
		require.Equal(t, stateTasks, h.state)
		tp.SelectTask(0)
		if attempt == 1 {
			press("x") // keep: its save fails, so this edit is retained from here on
		}
		press("j")
		press("D")
		_, _ = h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
		require.Equal(t, []string{"keep-4257"}, taskIDs(tp.GetTasks()),
			"attempt %d: the queued deletion hides its row until the save", attempt)

		esc() // closes the overlay and saves; both writes fail
		require.Equal(t, stateDefault, h.state)
		require.Equal(t, []string{"keep-4257", "gone-4257"}, taskIDs(tp.GetTasks()),
			"attempt %d: a task whose deletion failed is still on disk, so the pane must show it exactly once", attempt)
		assert.False(t, tp.GetTasks()[0].Enabled, "attempt %d: the failed edit must be retained", attempt)
		assert.True(t, tp.IsDirty(), "attempt %d: the failed edit must stay queued for retry", attempt)
		assert.Equal(t, []string{"keep-4257", "gone-4257"}, taskIDs(h.store.GetTasks()))
		require.NotNil(t, h.recovery)
		assert.Contains(t, h.recovery.detail, "failed to remove task")
		h.recovery = nil
	}
	assert.Equal(t, []string{"gone-4257", "gone-4257"}, removes, "each removal runs once, when the user asked for it")
	assert.Equal(t, []string{"keep-4257", "keep-4257"}, updates)

	// The daemon recovers. The next save lands the retained edit and removes
	// nothing the user did not ask to remove again.
	restoreUpdater()
	restoreRemover()
	_, _ = h.showTasksOverlay()
	esc()
	esc()
	require.Equal(t, stateDefault, h.state)
	assert.False(t, tp.IsDirty())
	assert.Equal(t, []string{"keep-4257", "gone-4257"}, taskIDs(tp.GetTasks()))
	disk, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	require.Equal(t, []string{"keep-4257", "gone-4257"}, taskIDs(disk))
	assert.False(t, disk[0].Enabled, "the retained edit must land once the daemon recovers")
}

// #4487: a background refresh reaches the pane while a failed edit is retained,
// instead of leaving the whole list stale until that edit saves.
func TestRefreshTasksReconcilesPaneAroundRetainedEdit(t *testing.T) {
	h := newTestHome(t)
	sp := h.automations.TaskPane()
	a := task.Task{ID: "a", Name: "a", Prompt: "p", CronExpr: "0 0 * * *", Enabled: true}
	sp.SetTasks([]task.Task{a})
	sp.SetFocus(true)
	require.True(t, sp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	sp.SetFocus(false)
	require.True(t, sp.IsDirty(), "precondition: an edit is held")

	aCLI := a
	aCLI.Prompt = "changed by the CLI"
	b := task.Task{ID: "b", Name: "b", Prompt: "p", CronExpr: "0 0 * * *", Enabled: true}
	fresh := []task.Task{aCLI, b}
	// The rail already shows fresh, so a reported change can only be the pane's.
	h.store.SetTasks([]task.Task{aCLI, b})
	require.True(t, h.refreshTasks(fresh, nil), "the pane's reconcile is a visible change")

	got := sp.GetTasks()
	require.Equal(t, []string{"a", "b"}, taskIDs(got), "a task added out-of-band must reach the pane")
	assert.False(t, got[0].Enabled, "the held edit must survive the refresh")
	assert.True(t, sp.IsDirty())
	assert.False(t, h.refreshTasks([]task.Task{aCLI, b}, nil),
		"a held edit keeps the pane different from disk; that alone must not repaint every poll")
}

// A project switch is a different list, not a reload of this one: nothing the
// user held against the previous project may follow them into the next.
func TestSwitchProjectDiscardsTaskPaneDrafts(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	sp := h.automations.TaskPane()
	sp.SetTasks([]task.Task{
		{ID: "a-edited", Name: "edited", Prompt: "p", CronExpr: "0 0 * * *", Enabled: true},
		{ID: "a-queued", Name: "queued", Prompt: "p", CronExpr: "0 0 * * *", Enabled: true},
	})
	sp.SetFocus(true)
	require.True(t, sp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	require.True(t, sp.DeleteTask("a-queued"))
	sp.SetFocus(false)
	require.True(t, sp.IsDirty(), "precondition: an edit and a deletion are held")

	projectBRoot := initTestGitRepo(t)
	h.switchProject(&config.RepoContext{Root: projectBRoot, ID: config.RepoIDFromRoot(projectBRoot)})
	require.Equal(t, projectBRoot, h.repoRoot, "precondition: the switch happened")

	assert.Empty(t, taskIDs(sp.GetTasks()), "a row from the previous project must not appear in this one")
	assert.False(t, sp.IsDirty(), "nothing held against the previous project may be saved from this one")
	assert.Empty(t, sp.ConsumeDirty())
	assert.Empty(t, sp.ConsumeDeleted())
}

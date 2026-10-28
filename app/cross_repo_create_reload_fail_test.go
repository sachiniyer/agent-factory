package app

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleTaskCreate_CrossRepoReloadFailureStillNamesTheBinding is the #5667
// regression guard. The create committed to another project, then the
// active-scope reload failed. The reload failure alone told the user "saved, do
// not create it again" but never where the task landed, so the next successful
// poll showed a list without it — the invisible-save ambiguity the binding
// notice exists to kill. Both messages must surface.
func TestHandleTaskCreate_CrossRepoReloadFailureStillNamesTheBinding(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}

	repoA := initTestGitRepo(t)
	repoB := initTestGitRepo(t)
	t.Chdir(repoA)
	h.repoRoot = repoA
	h.repoID = config.RepoIDFromRoot(repoA)

	// The create commits, then the store becomes unreadable so the post-create
	// reload fails.
	t.Cleanup(SetTaskAdderForTest(func(tk task.Task) error {
		require.NoError(t, task.AddTask(tk))
		dir, err := config.GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tasks.json"), []byte("{not json"), 0o644))
		return nil
	}))

	tp := h.automations.TaskPane()
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	tp.EnterCreateMode(repoB)
	require.True(t, tp.IsCreating())

	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("cross-reload")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // trigger selector
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // schedule picker
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // prompt
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("reload fails after commit")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // target session
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // path (B's root, prefilled)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // program
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // save
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	got := h.errBox.FullError()
	assert.Contains(t, got, "could not be reloaded", "the reload failure must still surface")
	assert.Contains(t, got, "cross-reload", "the binding notice must name the task")
	assert.Contains(t, got, repoB, "the binding notice must name where the task landed")
	assert.Contains(t, got, "does not appear in this project's list")
}

func TestBoundOutsideActiveScope(t *testing.T) {
	repoA := initTestGitRepo(t)
	repoB := initTestGitRepo(t)
	idA := config.RepoIDFromRoot(repoA)
	assert.False(t, boundOutsideActiveScope(idA, repoA), "the active repo itself is in scope")
	assert.True(t, boundOutsideActiveScope(idA, repoB), "another repo is out of scope")
	assert.True(t, boundOutsideActiveScope("", repoA), "registry mode has no active scope")
}

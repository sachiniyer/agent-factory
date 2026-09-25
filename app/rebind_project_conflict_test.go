package app

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
)

// stubRebindCAS makes the daemon seam run the real compare-and-set rebind and
// answer a precondition refusal the way the HTTP client does, recording the
// expected root each request carried.
func stubRebindCAS(t *testing.T) *[]string {
	t.Helper()
	var expected []string
	old := rebindProjectThroughDaemon
	rebindProjectThroughDaemon = func(projectID, expectedRoot, path string) (config.Project, error) {
		expected = append(expected, expectedRoot)
		project, err := config.RebindProjectIfRoot(projectID, expectedRoot, path)
		var rebound *config.ProjectReboundError
		if errors.As(err, &rebound) {
			return config.Project{}, &apiclient.ProjectReboundError{Detail: err.Error()}
		}
		return project, err
	}
	t.Cleanup(func() { rebindProjectThroughDaemon = old })
	return &expected
}

// TestRebindConflictIsARefusalThatReArmsAgainstTheCurrentRoot pins #4822 on
// the TUI: another client rebinds the project after this picker read it. The
// picker's rebind carries the root it displayed, the daemon refuses it, and the
// picker treats that as a definitive refusal — open, re-armed, naming the root
// the registry holds now — whose next Enter expects that root and lands.
func TestRebindConflictIsARefusalThatReArmsAgainstTheCurrentRoot(t *testing.T) {
	h, id := activeRebindHome(t)
	displayed := h.projectPickerOverlay
	expected := stubRebindCAS(t)
	var recorded string
	projects, err := config.ListProjects()
	require.NoError(t, err)
	for _, p := range projects {
		if p.ID == id {
			recorded = p.Root
		}
	}
	require.NotEmpty(t, recorded)

	elsewhere := initTestGitRepo(t)
	moved, err := config.RebindProject(id, elsewhere) // another client, meanwhile
	require.NoError(t, err)
	mine := initTestGitRepo(t)

	h.Update(submitPickerRebind(t, h, mine)())

	require.Equal(t, []string{recorded}, *expected, "the rebind must carry the root the picker displayed")
	require.Same(t, displayed, h.projectPickerOverlay, "a conflict is a refusal: the picker stays open")
	assert.Equal(t, stateSwitchProject, h.state)
	assert.False(t, displayed.RebindPending(), "a definitive refusal re-arms the form")
	assert.Contains(t, displayed.Render(), "Rebound elsewhere", "the refusal says the project moved")
	assert.Empty(t, h.errBox.FullError(), "a conflict is not reported as an unknown outcome or a failure")
	root, ok := registeredProjectRoot(id)
	require.True(t, ok)
	assert.Equal(t, moved.Root, root, "the refused rebind wrote nothing")

	_, cmd := h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "the re-armed form submits again")
	h.Update(cmd())

	require.Len(t, *expected, 2)
	assert.Equal(t, moved.Root, (*expected)[1], "the retry expects the root the refresh found")
	root, ok = registeredProjectRoot(id)
	require.True(t, ok)
	assert.Equal(t, mine, root, "the retry, made against the current root, lands")
}

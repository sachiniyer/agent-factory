package app

import (
	"errors"
	"os"
	"path/filepath"
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

	// Esc leaves the re-armed form for the list — which the conflict handler
	// rebuilt from the fresh registry read. The row must describe the rebound
	// registration, not the checkout the picker was opened on (#4888 review):
	// an Esc onto a stale row could select a path the registry dropped.
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEsc})
	row, ok := h.projectPickerOverlay.HighlightedProject()
	require.True(t, ok, "the rebuilt list still highlights a row")
	assert.Equal(t, moved.Root, row.Root, "the row's root is where the project is bound now")
	assert.Equal(t, config.RepoIDFromRoot(moved.Root), row.RepoID, "the row carries the rebound repo identity")
	assert.False(t, row.MissingPath, "the rebound checkout exists — the stale row claimed it did not")

	// Re-armed on that fresh row, Enter retries — expecting the root the
	// rebuilt row reports.
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(mine)})
	_, cmd := h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "the re-armed form submits again")
	h.Update(cmd())

	require.Len(t, *expected, 2)
	assert.Equal(t, moved.Root, (*expected)[1], "the retry expects the root the refresh found")
	root, ok = registeredProjectRoot(id)
	require.True(t, ok)
	assert.Equal(t, mine, root, "the retry, made against the current root, lands")
}

// TestRebindConflictWithAVanishedRecordStillRebuildsTheList pins the second
// #4888-review round on the conflict handler: the re-arm and the rebuilt list
// must come from the SAME registry read. When the record is deleted between
// the daemon's refusal and the TUI's re-read, the previous code skipped the
// rebuild entirely — the follow-up root read had nothing to name — leaving a
// row for a registration the registry dropped selectable. The rebuild must run
// regardless; the vanished record's row must be gone, and the form re-arms on
// the daemon's own refusal text.
func TestRebindConflictWithAVanishedRecordStillRebuildsTheList(t *testing.T) {
	h, id := activeRebindHome(t)
	displayed := h.projectPickerOverlay

	old := rebindProjectThroughDaemon
	rebindProjectThroughDaemon = func(projectID, _, path string) (config.Project, error) {
		// The daemon already computed its refusal; before this reply reaches
		// the TUI another client deletes the record outright.
		dir, err := config.ProjectRegistryDir()
		require.NoError(t, err)
		require.NoError(t, os.RemoveAll(filepath.Join(dir, projectID)))
		return config.Project{}, &apiclient.ProjectReboundError{Detail: "rebind project: project " + projectID + " was rebound elsewhere — refresh and retry"}
	}
	t.Cleanup(func() { rebindProjectThroughDaemon = old })

	h.Update(submitPickerRebind(t, h, initTestGitRepo(t))())

	require.Same(t, displayed, h.projectPickerOverlay, "a conflict is a refusal: the picker stays open")
	assert.False(t, displayed.RebindPending(), "a definitive refusal re-arms the form")
	// The daemon's full refusal is longer than the overlay's rendered line
	// width — widen it so the text the form re-armed on is visible.
	displayed.SetWidth(200)
	assert.Contains(t, displayed.Render(), "rebound elsewhere", "the daemon's refusal text re-arms the form")

	// Esc back to the list: the rebuild ran even with the record gone, so no
	// row for the dropped registration can be highlighted or selected.
	h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEsc})
	if row, ok := displayed.HighlightedProject(); ok && row.RegistryID == id {
		t.Fatalf("a deleted record's stale row must not survive the conflict rebuild, got %+v", row)
	}
}

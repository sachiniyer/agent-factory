package app

import (
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// projectPickerMouseHome stands up a home with the project picker open over two
// real git repos, the cursor on the active project (repo A) and the snapshot
// fetcher stubbed so switchProject never dials — or spawns — a real daemon.
// The picker is wired exactly as showProjectPickerOverlay leaves it: state ==
// stateSwitchProject, overlay set, and SetMaxSize(term,term) so View() renders
// and RegisterZones against the live terminal size. Returns the two repo roots
// so a test clicks the row for repo B and asserts the rail re-scoped to it.
//
// The fixture lists exactly two projects, so the picker's navigable rows are
// 0 (repo A), 1 (repo B), and 2 (the trailing "+ Add project…" row).
func projectPickerMouseHome(t *testing.T) (h *home, repoARoot, repoBRoot string) {
	t.Helper()
	h = newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))
	// switchProject cold-starts the incoming project's snapshot off the event
	// loop through home.snapshotFetcher; a real daemon is never available in the
	// app suite, so return an empty success so the switch completes.
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	resizeHome(h, 140, 40)

	repoARoot = initTestGitRepo(t)
	repoBRoot = initTestGitRepo(t)
	require.NotEqual(t, repoARoot, repoBRoot, "the two project repos must differ")
	h.repoRoot = repoARoot
	h.repoID = config.RepoIDFromRoot(repoARoot)

	projects := []overlay.Project{
		{Name: filepath.Base(repoARoot), Root: repoARoot, SessionCount: 1},
		{Name: filepath.Base(repoBRoot), Root: repoBRoot, SessionCount: 0},
	}
	h.projectPickerOverlay = overlay.NewProjectPickerOverlay(projects, repoARoot)
	h.layoutProjectPickerOverlay() // sizes the overlay to the terminal, like relayout does
	h.state = stateSwitchProject
	return h, repoARoot, repoBRoot
}

// TestProjectPickerRegistersOverlayZonesInView is the inverted signature of the
// bug: before the fix, View() registered NO overlay:* zones while the picker was
// open — the missing RegisterZones call that left a click to resolve to a zone
// geographically behind the overlay. View() now registers one overlay:select:N
// zone per visible row (here both projects plus the add row), so a click over a
// row resolves to that row instead of falling through handleModalClick.
func TestProjectPickerRegistersOverlayZonesInView(t *testing.T) {
	h, _, _ := projectPickerMouseHome(t)
	newFakeClock(h)

	_ = h.View()

	// The fixture lists two projects, so the navigable rows are 0, 1, and the
	// trailing add row (index 2). Each must register an overlay:select zone —
	// the sibling pickers' contract the project picker was missing.
	for i := 0; i < 3; i++ {
		_, ok := h.zones.Find(zones.OverlaySelectRow(i))
		require.True(t, ok, "picker row %d must register an overlay:select zone; got %v",
			i, h.zones.IDs())
	}
}

// TestProjectPickerMouseClickSelectsProject is the core fix: a left click on a
// project row selects and switches to it, exactly as pressing Enter on the
// highlighted row does (handleStateSwitchProject -> SelectedProject ->
// switchProject). Before the fix the click was silently swallowed: the state
// stayed stateSwitchProject and the overlay stayed open.
func TestProjectPickerMouseClickSelectsProject(t *testing.T) {
	h, repoARoot, repoBRoot := projectPickerMouseHome(t)
	newFakeClock(h)

	// The cursor starts on repo A (the active project, preselected). Click the
	// repo B row — index 1 in the picker.
	clickZone(t, h, zones.OverlaySelectRow(1))

	assert.Equal(t, stateDefault, h.state, "a project-row click must close the picker")
	assert.Nil(t, h.projectPickerOverlay, "the click must dismiss the picker overlay")
	assert.Equal(t, config.RepoIDFromRoot(repoBRoot), h.repoID,
		"the clicked row's project must become the active scope")
	assert.Equal(t, repoBRoot, h.repoRoot)
	assert.NotEqual(t, repoARoot, h.repoRoot, "the rail must re-scope away from the outgoing project")
}

// TestProjectPickerMouseClickAddRowEntersAddMode: clicking the trailing
// "+ Add project…" row drops into the path-input form, mirroring the keyboard
// (handleListKey's Enter on the add row). The picker stays open so the user can
// type a path; it does not switch.
func TestProjectPickerMouseClickAddRowEntersAddMode(t *testing.T) {
	h, repoARoot, _ := projectPickerMouseHome(t)
	newFakeClock(h)

	// Index 2 is the trailing add row for this two-project fixture.
	clickZone(t, h, zones.OverlaySelectRow(2))

	assert.Equal(t, stateSwitchProject, h.state, "the add-row click keeps the picker open")
	require.NotNil(t, h.projectPickerOverlay, "the picker must stay open in add mode")
	assert.Equal(t, repoARoot, h.repoRoot, "the add-row click must not switch projects")
	// The add-mode form renders its path input prompt; assert it through View.
	require.Contains(t, xansi.Strip(h.View()), "repo path",
		"clicking the add row must drop into the add-project path-input form")
}

// TestProjectPickerBackgroundClickSwallowed: while the picker owns the screen,
// a click that resolves to a non-overlay zone (a tree instance row behind the
// overlay) is swallowed — the state stays stateSwitchProject, the overlay
// stays open, and nothing mutates behind the modal. This is the #1774-class
// invariant the sibling pickers uphold and the project picker must too.
func TestProjectPickerBackgroundClickSwallowed(t *testing.T) {
	h, repoARoot, _ := projectPickerMouseHome(t)
	newFakeClock(h)
	alpha := startedLocalInstance(t, "alpha")
	beta := startedLocalInstance(t, "beta")
	h.store.AddInstance(alpha)
	h.store.AddInstance(beta)
	// Pre-select alpha so a swallowed click has a selection to NOT move; a nil
	// selection would make the "did not move" assertion meaningless.
	h.store.SetSelectedInstance(alpha)
	require.Equal(t, alpha.Title, h.store.GetSelectedInstance().Title)
	require.Equal(t, stateSwitchProject, h.state)

	// beta's tree row lives in the rail, outside the centered overlay, so the
	// click resolves to the tree zone — and is then swallowed by handleModalClick
	// (the picker has no row zone for it), exactly like the confirm modal.
	_ = h.View()
	_, ok := h.zones.Find(zones.TreeInstance(beta.Title))
	require.True(t, ok, "precondition: the tree row zone must exist behind the overlay")
	clickZone(t, h, zones.TreeInstance(beta.Title))

	assert.Equal(t, stateSwitchProject, h.state, "a background click must not close the picker")
	assert.NotNil(t, h.projectPickerOverlay, "the picker must stay open")
	assert.Equal(t, repoARoot, h.repoRoot, "a background click must not switch projects")
	require.NotNil(t, h.store.GetSelectedInstance())
	assert.Equal(t, alpha.Title, h.store.GetSelectedInstance().Title,
		"a background click must not move the selection behind the modal")
}

// TestProjectPickerEnterSwitchesProject is the keyboard regression guard: the
// mouse fix routes clicks through the SAME handleStateSwitchProject path the
// keyboard drives, so Enter on a highlighted project row must still switch and
// close the picker exactly as it did before.
func TestProjectPickerEnterSwitchesProject(t *testing.T) {
	h, repoARoot, repoBRoot := projectPickerMouseHome(t)
	newFakeClock(h)

	// Cursor starts on repo A; walk down to repo B and press Enter through the
	// real keyboard handler.
	_, _ = h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, stateDefault, h.state, "Enter must close the picker")
	assert.Nil(t, h.projectPickerOverlay, "Enter must dismiss the picker overlay")
	assert.Equal(t, config.RepoIDFromRoot(repoBRoot), h.repoID, "Enter must switch to the highlighted project")
	assert.Equal(t, repoBRoot, h.repoRoot)
	assert.NotEqual(t, repoARoot, h.repoRoot)
	_ = cmd
}

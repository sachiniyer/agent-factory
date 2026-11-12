package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// A project selection switches only to the exact registered project its row
// stands for: the same repo id AND the same checkout (#7109, #7116 review).
// These tests drive the entry points a user actually reaches — the Projects
// section's Enter, the Ctrl+P picker, and a mouse click on a Projects row —
// rather than repoContextForSelection directly, so a row that drops its
// identity anywhere on the way to the switch is caught.

// projectSelectionHome is a home scoped to a real active repository whose
// project switch completes without a daemon: the incoming project's snapshot
// and the cross-repo list both answer empty, and no root_agents opt-in adds
// rows of its own. The active repository is load-bearing, not decoration: the
// rail draws no Projects section for a single project (layout.Grid), so a
// registry with one project and no active one leaves nothing to focus.
func projectSelectionHome(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	active := initTestGitRepo(t)
	h.repoRoot, h.repoID = active, config.RepoIDFromRoot(active)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return nil, nil
	}))
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}
	resizeHome(h, 120, 40)
	return h
}

// setupReplacedCheckoutFixture registers a checkout, then replaces it with a
// fresh repository initialized at the same absolute root. RepoFromPath derives
// a repo id from the identity root's PATH, so the replacement carries exactly
// the id the registration recorded; only the registry's checkout marker, which
// lives in the old .git, tells the two apart. Must run after newTestHome, which
// points AGENT_FACTORY_HOME at the test's own registry.
func setupReplacedCheckoutFixture(t *testing.T) (root string, rec config.Project) {
	t.Helper()
	root = initTestGitRepo(t)
	rec, err := config.RegisterProject(root)
	require.NoError(t, err)
	require.NotEmpty(t, rec.CheckoutID, "fixture: a fresh registration records a checkout marker")
	require.NotEmpty(t, rec.RepoID, "fixture: a fresh registration records its repo id")

	require.NoError(t, os.RemoveAll(filepath.Join(rec.Root, ".git")))
	runGit(t, rec.Root, "init")

	replacement, err := config.RepoFromPath(rec.Root)
	require.NoError(t, err)
	require.Equal(t, rec.RepoID, replacement.ID,
		"fixture: a replacement at the same path must share the recorded repo id, or a repo-id check alone would already refuse it")
	matches, err := config.ProjectCheckoutMatches(rec.Root, rec.CheckoutID)
	require.NoError(t, err)
	require.False(t, matches, "fixture: the replacement must not carry the registered checkout marker")
	return rec.Root, rec
}

// moveProjectsCursorTo walks the Projects section's cursor with the keyboard
// until it rests on the row match accepts, so the test reaches the row the way
// a user does and depends on no selection helper.
func moveProjectsCursorTo(t *testing.T, h *home, match func(ui.SidebarProject) bool) ui.SidebarProject {
	t.Helper()
	for i := 0; i <= len(h.projects.Projects()); i++ {
		if proj, ok := h.projects.SelectedProject(); ok && match(proj) {
			return proj
		}
		_, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyDown})
		require.True(t, consumed, "Down must be consumed by the focused Projects section")
	}
	t.Fatalf("no Projects row matched; rows: %+v", h.projects.Projects())
	return ui.SidebarProject{}
}

// TestProjectsSectionEnterRefusesReplacementCheckoutAtSameRoot is Codex P1
// 4544477245 on #7116: a registered checkout replaced by another repository
// initialized at the same root must be refused, not switched to. The row comes
// from the real registry through buildProjectList, so this also pins that the
// row's checkout identity survives into the Projects section.
func TestProjectsSectionEnterRefusesReplacementCheckoutAtSameRoot(t *testing.T) {
	h := projectSelectionHome(t)
	root, rec := setupReplacedCheckoutFixture(t)
	beforeRepoID, beforeRoot := h.repoID, h.repoRoot
	require.NotEqual(t, rec.RepoID, beforeRepoID, "fixture: the active project must not be the replaced one")

	h.refreshSidebarProjects()
	h.relayout()
	h.focusRegion(layout.RegionProjects)
	require.Equal(t, layout.RegionProjects, h.ring.Active())
	proj := moveProjectsCursorTo(t, h, func(p ui.SidebarProject) bool { return p.Root == root })
	require.Equal(t, rec.RepoID, proj.RepoID, "fixture: the row carries the registration's repo id")

	_, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, consumed)
	assert.Equal(t, beforeRepoID, h.repoID, "Enter on a replaced checkout must not re-scope the rail onto the replacement")
	assert.Equal(t, beforeRoot, h.repoRoot)
	text, failure := h.errBox.RetainedNotice()
	assert.True(t, failure, "the refusal is reported, not swallowed")
	assert.Contains(t, text, "af projects rebind", "the refusal names the repair")
}

// TestProjectPickerRefusesReplacementCheckoutAtSameRoot is the same P1 through
// the Ctrl+P picker, whose rows already carry RegistryCheckoutID for rebind.
func TestProjectPickerRefusesReplacementCheckoutAtSameRoot(t *testing.T) {
	h := projectSelectionHome(t)
	root, rec := setupReplacedCheckoutFixture(t)
	beforeRepoID, beforeRoot := h.repoID, h.repoRoot

	h.projectPickerOverlay = overlay.NewProjectPickerOverlay([]overlay.Project{
		{Name: filepath.Base(beforeRoot), Root: beforeRoot, RepoID: beforeRepoID, SessionCount: 1},
		{
			Name: filepath.Base(root), Root: root, RepoID: rec.RepoID,
			RegistryID: rec.ID, RegistryRoot: rec.Root, RegistryCheckoutID: rec.CheckoutID,
		},
	}, beforeRoot)
	h.layoutProjectPickerOverlay()
	h.state = stateSwitchProject

	_, _ = h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyDown})
	highlighted, ok := h.projectPickerOverlay.HighlightedProject()
	require.True(t, ok)
	require.Equal(t, root, highlighted.Root, "fixture: the cursor rests on the replaced row")
	_, _ = h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, beforeRepoID, h.repoID, "the picker must not switch onto a replaced checkout")
	assert.Equal(t, beforeRoot, h.repoRoot)
	text, failure := h.errBox.RetainedNotice()
	assert.True(t, failure, "the refusal is reported, not swallowed")
	assert.Contains(t, text, "af projects rebind", "the refusal names the repair")
}

// TestProjectsSectionEnterSwitchesRegisteredCheckout is the guard beside the
// refusal: a registration whose checkout is still the one at its root switches
// exactly as before, so checking the marker refuses only a real replacement.
func TestProjectsSectionEnterSwitchesRegisteredCheckout(t *testing.T) {
	h := projectSelectionHome(t)
	rec, err := config.RegisterProject(initTestGitRepo(t))
	require.NoError(t, err)
	require.NotEmpty(t, rec.CheckoutID)
	require.NotEqual(t, rec.RepoID, h.repoID)

	h.refreshSidebarProjects()
	h.relayout()
	h.focusRegion(layout.RegionProjects)
	moveProjectsCursorTo(t, h, func(p ui.SidebarProject) bool { return p.Root == rec.Root })

	_, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, consumed)
	assert.Equal(t, rec.RepoID, h.repoID, "a registered checkout that is still in place switches")
	assert.Equal(t, rec.Root, h.repoRoot)
}

// projectRowClickPoints returns one click point per rendered Projects row, top
// to bottom, found by hit-testing each line of the section rather than by
// looking a row's zone id up. Two rows at one root are exactly the case where
// a root-keyed id cannot name the row, so the test must not depend on one.
func projectRowClickPoints(t *testing.T, h *home) []layout.Point {
	t.Helper()
	section := zoneRect(t, h, zones.ProjectsBG)
	var points []layout.Point
	for y := section.Y; y < section.Y+section.H; y++ {
		x := section.X + 1
		if id, _, ok := h.zones.Resolve(x, y); ok && strings.HasPrefix(id, "projects:row:") {
			points = append(points, layout.Point{X: x, Y: y})
		}
	}
	return points
}

// TestProjectsSectionClickSelectsTheClickedRowOfTwoSharingARoot is Codex P1
// 4544477251 on #7116: discovery can produce two rows at one root — a stale
// registration that recorded another identity there (a former bare-worktree
// checkout) and the live repository now at that path. A click must select and
// carry the identity of exactly the row clicked: the stale row is refused as
// itself, and the healthy row is not made unclickable by its stale sibling.
func TestProjectsSectionClickSelectsTheClickedRowOfTwoSharingARoot(t *testing.T) {
	h := projectSelectionHome(t)
	newFakeClock(h)
	root := initTestGitRepo(t)
	liveID := config.RepoIDFromRoot(root)
	staleID := config.RepoIDFromRoot(filepath.Join(root, "former-bare.git"))
	require.NotEqual(t, liveID, staleID)
	beforeRepoID, beforeRoot := h.repoID, h.repoRoot

	h.projects.SetProjects([]ui.SidebarProject{
		{Name: filepath.Base(beforeRoot), Root: beforeRoot, RepoID: beforeRepoID, SessionCount: 1, Active: true},
		{Name: filepath.Base(root), Root: root, RepoID: staleID},
		{Name: filepath.Base(root), Root: root, RepoID: liveID},
	})
	h.relayout()
	points := projectRowClickPoints(t, h)
	require.Len(t, points, 3, "fixture: every Projects row is on screen")

	// The stale row first: selected as itself and refused, so nothing switches.
	_ = combineCmds(press(h, points[1].X, points[1].Y), release(h, points[1].X, points[1].Y))
	sel, ok := h.projects.SelectedProject()
	require.True(t, ok)
	assert.Equal(t, staleID, sel.RepoID, "a click on the stale row selects the stale row")
	assert.Equal(t, beforeRepoID, h.repoID, "the stale row's identity does not match the checkout there, so it is refused")
	assert.Equal(t, beforeRoot, h.repoRoot)

	// The healthy row second: it must switch, carrying its own identity rather
	// than the first row at the same root.
	_ = combineCmds(press(h, points[2].X, points[2].Y), release(h, points[2].X, points[2].Y))
	assert.Equal(t, liveID, h.repoID, "a click on the healthy row switches to it, whatever its sibling recorded")
	assert.Equal(t, root, h.repoRoot)
}

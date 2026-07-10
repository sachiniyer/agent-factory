package app

import (
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectsSection_TabFocusRingIncludesProjects is the #1588 follow-up
// contract: the bottom Projects section is its own Tab-focusable region, a peer
// of the automations section positioned BELOW it. Forward Tab cycles
// tree → panes (in order) → automations → projects → (wrap) tree, and Shift-Tab
// reverses it — so the user Tabs INTO Projects like automations, and the ring
// still wraps back to the tree (the #1558 stuck-ring guard).
func TestProjectsSection_TabFocusRingIncludesProjects(t *testing.T) {
	h := paneTestHome(t)
	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, 3, h.store.NumOpenPanes())
	require.Equal(t, 3, h.lastLayout.PaneCount(), "200 cols fits three panes")
	require.True(t, h.lastLayout.ProjectsVisible, "the Projects section is a visible focus-ring peer")
	panes := h.store.OpenPanes()

	h.focusRegion(layout.RegionTree)
	forward := []string{
		layout.PaneRegion(panes[0].ID()),
		layout.PaneRegion(panes[1].ID()),
		layout.PaneRegion(panes[2].ID()),
		layout.RegionAutomations,
		layout.RegionProjects,
		layout.RegionTree,
	}
	for _, want := range forward {
		pressTab(t, h, false)
		require.Equal(t, want, h.ring.Active(),
			"forward Tab: tree → panes → automations → projects → tree")
	}
	require.True(t, h.projects.Focused() == false, "the ring rested on the tree, not projects")

	backward := []string{
		layout.RegionProjects,
		layout.RegionAutomations,
		layout.PaneRegion(panes[2].ID()),
		layout.PaneRegion(panes[1].ID()),
		layout.PaneRegion(panes[0].ID()),
		layout.RegionTree,
	}
	for _, want := range backward {
		pressTab(t, h, true)
		require.Equal(t, want, h.ring.Active(), "Shift-Tab reverses the same ring")
	}
}

// TestProjectsSection_FocusedSelectSwitches: with the Projects section focused,
// moving the cursor onto a project row and pressing Enter re-scopes the rail to
// that project — reusing the #1547 switchProject path (via switchToProjectRoot),
// not a duplicate switch. This is the primary "Tab into it and pick a project"
// interaction.
func TestProjectsSection_FocusedSelectSwitches(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	resizeHome(h, 100, 30)

	repoBRoot := initTestGitRepo(t)
	require.NotEqual(t, h.repoRoot, repoBRoot)

	// Active project first, repo B second; Tab into Projects and step the cursor
	// down onto repo B.
	h.projects.SetProjects([]ui.SidebarProject{
		{Name: filepath.Base(h.repoRoot), Root: h.repoRoot, SessionCount: 1, Active: true},
		{Name: filepath.Base(repoBRoot), Root: repoBRoot, SessionCount: 0},
	})
	h.focusRegion(layout.RegionProjects)
	require.Equal(t, layout.RegionProjects, h.ring.Active())

	// Cursor keys move the section's selection (owned by the focused section).
	_, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyDown})
	require.True(t, consumed, "Down is consumed by the focused Projects section")
	proj, ok := h.projects.SelectedProject()
	require.True(t, ok)
	require.Equal(t, repoBRoot, proj.Root, "cursor rests on repo B")

	// Enter switches to it.
	_, _, consumed = h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, consumed)
	assert.Equal(t, config.RepoIDFromRoot(repoBRoot), h.repoID, "Enter switches to the cursor's project")
	assert.Equal(t, repoBRoot, h.repoRoot)
}

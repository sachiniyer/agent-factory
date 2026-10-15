package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/overlay"
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
	h.relayout() // Project rows determine whether this section is visible.
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

// TestProjectsSection_CursorSurvivesRefreshReorder is the #1590 review fix: with
// the cursor on project B, a background refresh that inserts/reorders projects
// ahead of B must NOT slide the cursor onto a different project — Enter still
// switches to B, tracked by identity (repo root) rather than row index.
func TestProjectsSection_CursorSurvivesRefreshReorder(t *testing.T) {
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

	// Rows sorted so B is the second row; move the cursor onto B by identity.
	h.projects.SetProjects([]ui.SidebarProject{
		{Name: "aaa-active", Root: h.repoRoot, SessionCount: 1, Active: true},
		{Name: filepath.Base(repoBRoot), Root: repoBRoot, SessionCount: 0},
	})
	h.relayout() // Project rows determine whether this section is visible.
	h.focusRegion(layout.RegionProjects)
	require.True(t, h.projects.SelectByRoot(repoBRoot))
	sel, _ := h.projects.SelectedProject()
	require.Equal(t, repoBRoot, sel.Root)

	// A refresh inserts two projects that sort BEFORE B, shifting its row index.
	h.projects.SetProjects([]ui.SidebarProject{
		{Name: "aaa-active", Root: h.repoRoot, SessionCount: 1, Active: true},
		{Name: "aab-inserted", Root: "/repos/inserted-1", SessionCount: 4},
		{Name: "aac-inserted", Root: "/repos/inserted-2", SessionCount: 2},
		{Name: filepath.Base(repoBRoot), Root: repoBRoot, SessionCount: 0},
	})

	// The cursor must still be on B (not whatever now occupies B's old row).
	sel, ok := h.projects.SelectedProject()
	require.True(t, ok)
	require.Equal(t, repoBRoot, sel.Root, "cursor tracks B by identity across the reorder")

	// Enter switches to B — not the project that slid into B's former index.
	_, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, consumed)
	assert.Equal(t, config.RepoIDFromRoot(repoBRoot), h.repoID, "Enter still switches to B after the reorder")
	assert.Equal(t, repoBRoot, h.repoRoot)
}

// TestProjectsSection_SnapshotRefreshUpdatesRowsLive is the #1590 finding-2 fix:
// the always-visible Projects section refreshes its per-repo counts from the
// cross-repo snapshot the background poll fetches, so a session created/removed
// in ANOTHER repo updates the rows WITHOUT a project switch. Before the fix the
// pane's counts only refreshed at startup + after a switch, so they went stale.
func TestProjectsSection_SnapshotRefreshUpdatesRowsLive(t *testing.T) {
	h := newTestHome(t)
	// The pane's rows come from the poll's carried all-repos data, not this
	// fetcher — keep it inert so a stray call can't interfere.
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return nil, nil
	}))
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}
	h.repoRoot = "/repos/active"

	mk := func(root string) session.InstanceData {
		d := session.InstanceData{Title: "s", CreatedAt: time.Now()}
		d.Worktree.RepoPath = root
		return d
	}
	repoB := "/repos/other"
	rowByRoot := func() map[string]ui.SidebarProject {
		out := map[string]ui.SidebarProject{}
		for _, r := range h.projects.Projects() {
			out[r.Root] = r
		}
		return out
	}

	// A poll whose cross-repo list has two sessions in repo B updates the rows.
	h.Update(snapshotFetchedMsg{
		repoID:   h.repoID,
		allRepos: []session.InstanceData{mk(repoB), mk(repoB)},
	})
	rows := rowByRoot()
	require.Contains(t, rows, repoB, "a session in another repo appears without a switch")
	assert.Equal(t, 2, rows[repoB].SessionCount, "counts reflect the cross-repo snapshot")
	require.Contains(t, rows, h.repoRoot, "the active project is always listed")
	assert.True(t, rows[h.repoRoot].Active, "the active project stays marked")

	// A later poll with a changed count updates the visible rows live.
	h.Update(snapshotFetchedMsg{
		repoID:   h.repoID,
		allRepos: []session.InstanceData{mk(repoB), mk(repoB), mk(repoB)},
	})
	assert.Equal(t, 3, rowByRoot()[repoB].SessionCount,
		"the count updates on the next poll — no project switch needed")

	// A poll error leaves the last-known rows intact (no blanking on a hiccup).
	h.Update(snapshotFetchedMsg{
		repoID:      h.repoID,
		allReposErr: assert.AnError,
	})
	assert.Equal(t, 3, rowByRoot()[repoB].SessionCount,
		"a failed cross-repo read keeps the last-known counts")
}

// TestProjectsSection_FailedSnapshotMergesFreshRegistryRows pins the #4888
// round-7 review finding: a failed SESSION half does not make the registry
// half of the same build disposable. When a rebind moved a record between
// polls, discarding the fresh registry union would leave the moved record's
// old root on screen for as long as the poll stays unhealthy — the sidebar
// must merge fresh registry truth into its last-known rows (registry fields
// replace registry fields; session rows and counts survive as last-known),
// the same merge the picker's SetRebindConflictPreserving applies.
func TestProjectsSection_FailedSnapshotMergesFreshRegistryRows(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = "/repos/active"
	h.setSidebarProjects([]overlay.Project{
		{Name: "alpha", Root: "/old/alpha", RepoID: "repo-old", RegistryID: "prj_aaa", RegistryRoot: "/old/alpha", SessionCount: 4, InPlaceCount: 1},
		{Name: "sessions", Root: "/repos/sessions", RepoID: "repo-sess", SessionCount: 3},
	}, false)

	// The poll's session half failed; its registry half read fine and shows
	// the alpha record rebound to a different repo (RepoID changed).
	changed := h.applySidebarProjects([]overlay.Project{
		{Name: "alpha", Root: "/new/alpha", RepoID: "repo-new", RegistryID: "prj_aaa", RegistryRoot: "/new/alpha"},
	}, false, errors.New("daemon snapshot unavailable"))
	require.True(t, changed, "fresh registry truth still reaches the section")

	rows := map[string]ui.SidebarProject{}
	for _, r := range h.projects.Projects() {
		rows[r.RepoID] = r
	}
	require.Contains(t, rows, "repo-new")
	assert.Equal(t, "/new/alpha", rows["repo-new"].Root,
		"the moved record's row refreshes to where the registry binds it now")
	assert.Zero(t, rows["repo-new"].SessionCount,
		"the moved registration must not inherit counts tallied against the old repo")
	require.Contains(t, rows, "repo-old")
	assert.Equal(t, 4, rows["repo-old"].SessionCount,
		"the old repo's sessions keep a session-only row with its last-known tallies")
	assert.Equal(t, 1, rows["repo-old"].InPlaceCount)
	require.Contains(t, rows, "repo-sess")
	assert.Equal(t, 3, rows["repo-sess"].SessionCount,
		"an unrelated session-derived row survives the failed fetch")
}

// TestProjectsSection_DegradedRegistryKeepsRegistryRows pins the mirror image
// of TestProjectsSection_FailedSnapshotMergesFreshRegistryRows (#4888 review):
// the session snapshot LANDED but the registry read degraded, so the fresh
// list holds live session rows and no registry records. Installing wholesale
// would drop every registry-backed row behind a read that cannot prove
// absence; the merge preserves them and grafts a same-repo session row onto
// the stale identity so live counts win. The preserved row's own counts clear
// though — the landed snapshot is the count authority and proved them zero.
func TestProjectsSection_DegradedRegistryKeepsRegistryRows(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = "/repos/active"
	h.setSidebarProjects([]overlay.Project{
		{Name: "alpha", Root: "/repos/alpha", RepoID: "repo-alpha", RegistryID: "prj_aaa", RegistryRoot: "/repos/alpha", RegistryCheckoutID: "chk_old", SessionCount: 4},
		{Name: "gone", Root: "/repos/gone", RepoID: "repo-gone", RegistryID: "prj_ggg", RegistryRoot: "/repos/gone", SessionCount: 2},
	}, false)

	// The poll's registry half failed; its session half read fine and reports
	// live counts — alpha's sessions grew, and the repo-gone row has no live
	// counterpart.
	changed := h.applySidebarProjects([]overlay.Project{
		{Name: "alpha", Root: "/repos/alpha", RepoID: "repo-alpha", SessionCount: 5},
	}, true, nil)
	require.True(t, changed, "fresh session truth still reaches the section")

	rows := map[string]ui.SidebarProject{}
	for _, r := range h.projects.Projects() {
		rows[r.RepoID] = r
	}
	require.Contains(t, rows, "repo-alpha")
	assert.Equal(t, 5, rows["repo-alpha"].SessionCount,
		"the live session row wins the count — the stale registry row must not shadow it")
	require.Contains(t, rows, "repo-gone",
		"absence unproven under a degraded read — the registry row stays")
	assert.Equal(t, 0, rows["repo-gone"].SessionCount,
		"the landed session snapshot is the count authority — an uncovered repo's tally clears (#4888 review)")

	// And the graft restored alpha's registration identity: the shadow list
	// keeps the rebind affordance the pane rows cannot carry.
	var alpha *overlay.Project
	for i, r := range h.sidebarProjects {
		if r.RepoID == "repo-alpha" {
			alpha = &h.sidebarProjects[i]
		}
	}
	require.NotNil(t, alpha)
	assert.Equal(t, "prj_aaa", alpha.RegistryID,
		"the grafted row carries the registration identity, not a stale duplicate")
}

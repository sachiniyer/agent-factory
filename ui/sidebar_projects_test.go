package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/store"
)

func projectsTestSidebar(t *testing.T) *Sidebar {
	t.Helper()
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
	s.SetSize(40, 40)
	return s
}

// projectRowVisible reports whether any non-header Projects row is currently in
// the visible list.
func projectRowVisible(s *Sidebar) bool {
	for _, it := range s.visibleItems {
		if it.Kind == SectionProjects && !it.IsHeader && it.ItemIndex >= 0 {
			return true
		}
	}
	return false
}

// TestSidebar_ProjectsSectionAtBottomDefaultCollapsed: once a project list is
// pushed, the Projects section renders as the very LAST header on the rail
// (below Instances and Archived), collapsed by default — no project rows show
// until it is expanded.
func TestSidebar_ProjectsSectionAtBottomDefaultCollapsed(t *testing.T) {
	s := projectsTestSidebar(t)
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	addTestInstance(s, archTestInstance(t, "put-away", session.Archived))
	s.SetProjects([]SidebarProject{
		{Name: "alpha", Root: "/repos/alpha", SessionCount: 2, Active: true},
		{Name: "beta", Root: "/repos/beta", SessionCount: 1},
	})
	// Expand Archived too so all three headers coexist and ordering is exercised.
	s.ClickHeaderKind(SectionArchived)

	var headerKinds []SidebarSectionKind
	for _, it := range s.visibleItems {
		if it.IsHeader {
			headerKinds = append(headerKinds, it.Kind)
		}
	}
	require.Equal(t, []SidebarSectionKind{SectionInstances, SectionArchived, SectionProjects}, headerKinds,
		"the Projects section header must be pinned at the very bottom of the rail")

	// Default collapsed: no project rows visible until expanded.
	require.False(t, projectRowVisible(s), "the Projects section must start collapsed")

	view := s.View()
	assert.Contains(t, view, "Projects (2)", "the Projects header carries the project count")
	assert.NotContains(t, view, "alpha", "collapsed Projects section shows no project rows")
}

// TestSidebar_ProjectsSectionHiddenWithoutList: with no project list pushed the
// section does not render at all (unit-test parity with the pre-Projects rail).
func TestSidebar_ProjectsSectionHiddenWithoutList(t *testing.T) {
	s := projectsTestSidebar(t)
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	for _, it := range s.visibleItems {
		assert.NotEqual(t, SectionProjects, it.Kind, "no Projects section renders before a list is pushed")
	}
	assert.NotContains(t, s.View(), "Projects")
}

// TestSidebar_ProjectsExpandListsProjectsAndMarksActive: expanding the section
// reveals one row per pushed project — each with its name, session count, and,
// for the active project, a distinct marker.
func TestSidebar_ProjectsExpandListsProjectsAndMarksActive(t *testing.T) {
	s := projectsTestSidebar(t)
	s.SetProjects([]SidebarProject{
		{Name: "alpha", Root: "/repos/alpha", SessionCount: 3, Active: true},
		{Name: "beta", Root: "/repos/beta", SessionCount: 0},
	})

	s.ClickHeaderKind(SectionProjects)
	require.True(t, projectRowVisible(s), "expanding the Projects section reveals the project rows")

	// Both project rows resolve, in the pushed order.
	var roots []string
	for _, it := range s.visibleItems {
		if it.Kind == SectionProjects && !it.IsHeader {
			roots = append(roots, s.projects[it.ItemIndex].Root)
		}
	}
	require.Equal(t, []string{"/repos/alpha", "/repos/beta"}, roots)

	view := s.View()
	assert.Contains(t, view, "alpha", "project rows render their name")
	assert.Contains(t, view, "beta")
	assert.Contains(t, view, "(3)", "project rows render their session count")
	assert.Contains(t, view, "●", "the active project carries a distinct marker")
}

// TestSidebar_GetSelectedProjectResolvesRow: with the cursor on a project row,
// GetSelectedProject returns that project (the hook the app reads to switch),
// and returns false everywhere else.
func TestSidebar_GetSelectedProjectResolvesRow(t *testing.T) {
	s := projectsTestSidebar(t)
	s.SetProjects([]SidebarProject{
		{Name: "alpha", Root: "/repos/alpha", SessionCount: 1, Active: true},
		{Name: "beta", Root: "/repos/beta", SessionCount: 2},
	})

	// On the collapsed header: not a project row.
	_, ok := s.GetSelectedProject()
	require.False(t, ok, "a header selection is not a project row")

	s.ClickHeaderKind(SectionProjects)
	// Land the cursor on the second project row and resolve it.
	for i, it := range s.visibleItems {
		if it.Kind == SectionProjects && !it.IsHeader && s.projects[it.ItemIndex].Root == "/repos/beta" {
			s.selectedIdx = i
			break
		}
	}
	proj, ok := s.GetSelectedProject()
	require.True(t, ok, "the cursor on a project row must resolve it")
	assert.Equal(t, "/repos/beta", proj.Root)
	assert.Equal(t, "beta", proj.Name)
}

// TestSidebar_NavReachesProjectsAtTail: Down at the rail tail auto-expands the
// Projects section and steps into it; Up back out auto-collapses it again
// (#1518 symmetry, extended to Projects).
func TestSidebar_NavReachesProjectsAtTail(t *testing.T) {
	s := projectsTestSidebar(t)
	liveInst := archTestInstance(t, "live-one", session.Ready)
	addAgentShellTabs(liveInst)
	addTestInstance(s, liveInst)
	s.SetProjects([]SidebarProject{
		{Name: "alpha", Root: "/repos/alpha", SessionCount: 1, Active: true},
	})
	require.False(t, projectRowVisible(s), "Projects starts collapsed before the boundary walk")

	s.SetSelectedInstance(0)
	s.Down() // live Agent tab
	s.Down() // live Terminal tab — the last live tab stop
	require.True(t, s.GetSelection().IsTab)

	s.Down()
	sel := s.GetSelection()
	require.Equal(t, SectionProjects, sel.Kind, "Down at the live tail auto-opens Projects and reaches its rows")
	require.False(t, sel.IsHeader)
	require.True(t, projectRowVisible(s), "Down at the tail must expand the Projects section")

	s.Up()
	sel = s.GetSelection()
	require.Equal(t, SectionInstances, sel.Kind, "Up from the first project row returns to the live tabs")
	require.True(t, sel.IsTab)
	require.False(t, projectRowVisible(s), "Up back into the live instances auto-collapses the Projects section")
}

// TestSidebar_NavCrossesArchivedThenProjects: with both trailing sections
// present, Down chains live tabs → Archived rows → Projects rows, and Up unwinds
// the same path, auto-collapsing each section as the cursor leaves it.
func TestSidebar_NavCrossesArchivedThenProjects(t *testing.T) {
	s := projectsTestSidebar(t)
	liveInst := archTestInstance(t, "live-one", session.Ready)
	addAgentShellTabs(liveInst)
	archivedInst := archTestInstance(t, "put-away", session.Archived)
	addTestInstance(s, liveInst)
	addTestInstance(s, archivedInst)
	s.SetProjects([]SidebarProject{
		{Name: "alpha", Root: "/repos/alpha", SessionCount: 1, Active: true},
	})
	s.SetSelectedInstance(0)

	s.Down() // live Agent tab
	s.Down() // live Terminal tab
	s.Down() // auto-open Archived → first archived row
	require.Equal(t, SectionArchived, s.GetSelection().Kind)

	s.Down() // auto-open Projects → first project row
	require.Equal(t, SectionProjects, s.GetSelection().Kind, "Down from the archived tail reaches the Projects rows")
	require.True(t, projectRowVisible(s))

	s.Up() // back into the archived row; Projects collapses behind the cursor
	require.Equal(t, SectionArchived, s.GetSelection().Kind, "Up from Projects returns to the archived row")
	require.False(t, projectRowVisible(s), "leaving Projects upward auto-collapses it")
}

// TestSidebar_ProjectsZonesRegistered: the Projects header gets its own distinct
// zone id, and — once expanded — each project row registers a root-keyed
// clickable zone.
func TestSidebar_ProjectsZonesRegistered(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
	reg := zones.NewRegistry()
	s.SetZoneRegistry(reg)
	s.SetRect(layout.Rect{X: 0, Y: 0, W: 40, H: 40})
	s.SetProjects([]SidebarProject{
		{Name: "alpha", Root: "/repos/alpha", SessionCount: 1, Active: true},
	})

	reg.Reset()
	_ = s.String()
	_, okHeader := reg.Find(zones.TreeHeaderProjects)
	require.True(t, okHeader, "the Projects header must register its own distinct zone")
	assert.NotEqual(t, zones.TreeHeader, zones.TreeHeaderProjects)
	assert.NotEqual(t, zones.TreeHeaderArchived, zones.TreeHeaderProjects)

	// Collapsed → no project-row zone yet.
	_, okRow := reg.Find(zones.TreeProject("/repos/alpha"))
	require.False(t, okRow, "a collapsed Projects section registers no project-row zone")

	// Expand and re-render: the project row registers a root-keyed zone.
	s.ClickHeaderKind(SectionProjects)
	reg.Reset()
	_ = s.String()
	_, okRow = reg.Find(zones.TreeProject("/repos/alpha"))
	require.True(t, okRow, "an expanded project row must register a clickable TreeProject zone")
	root, ok := zones.TreeProjectRoot(zones.TreeProject("/repos/alpha"))
	require.True(t, ok)
	assert.Equal(t, "/repos/alpha", root)
}

// TestSidebar_ProjectRowRenderTruncatesNarrow guards the #646 overflow class:
// a long project name at a narrow rail truncates rather than pushing the row
// past the allocation.
func TestSidebar_ProjectRowRenderTruncatesNarrow(t *testing.T) {
	s := projectsTestSidebar(t)
	s.SetSize(12, 20)
	s.SetProjects([]SidebarProject{
		{Name: "a-very-long-project-name-that-overflows", Root: "/repos/x", SessionCount: 1, Active: true},
	})
	s.ClickHeaderKind(SectionProjects)
	view := s.View()
	for i, line := range strings.Split(view, "\n") {
		require.LessOrEqualf(t, lipgloss.Width(line), 12, "line %d must not exceed the rail width", i)
	}
}

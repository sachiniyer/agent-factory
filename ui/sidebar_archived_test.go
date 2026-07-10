package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/store"
)

func archTestInstance(t *testing.T, title string, status session.Status) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(status != session.Archived)
	inst.SetStatusForTest(status)
	return inst
}

// TestSidebar_ArchivedPartitionedIntoFolder (#1028): a live session renders under
// Instances and an archived one under a separate "Archived" folder at the bottom
// (collapsed by default, so its row is hidden until expanded). The counts in the
// two headers reflect the partition.
func TestSidebar_ArchivedPartitionedIntoFolder(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	addTestInstance(s, archTestInstance(t, "put-away", session.Archived))
	s.SetSize(40, 40)

	// Two section headers now exist: Instances and Archived.
	var headers []SidebarItem
	for _, it := range s.visibleItems {
		if it.IsHeader {
			headers = append(headers, it)
		}
	}
	require.Len(t, headers, 2, "an Archived folder header must appear once a session is archived")
	assert.Equal(t, SectionInstances, headers[0].Kind)
	assert.Equal(t, SectionArchived, headers[1].Kind, "the Archived folder is pinned last")

	// The Archived folder starts collapsed, so its archived row is not visible.
	for _, it := range s.visibleItems {
		if it.Kind == SectionArchived && !it.IsHeader {
			t.Fatal("the Archived folder must start collapsed (no archived rows visible)")
		}
	}

	// Header labels carry the partitioned counts.
	view := s.View()
	assert.Contains(t, view, "Instances (1)", "Instances counts only live sessions")
	assert.Contains(t, view, "Archived (1)")
}

// TestSidebar_RestoringRowRehomedToInstances (#1210): a row mid-restore
// (OpRestoring overlay, liveness still Archived) renders under the live Instances
// section, not the Archived folder — the eager re-home the archive epic owed
// restore. Its liveness deliberately stays Archived so the snapshot reconcile
// still sees the Archived→live transition and runs its rebuild/re-Start (#1203).
func TestSidebar_RestoringRowRehomedToInstances(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	restoring := archTestInstance(t, "coming-back", session.Archived)
	restoring.SetInFlightOpForTest(session.OpRestoring)
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	addTestInstance(s, restoring)
	s.SetSize(40, 40)

	require.Equal(t, session.LiveArchived, restoring.GetLiveness(),
		"the eager re-home must leave liveness Archived so the reconcile rebuild still fires (#1203)")
	require.False(t, restoring.ShownArchived(), "a mid-restore row is not shown as archived")

	view := s.View()
	assert.Contains(t, view, "Instances (2)",
		"a mid-restore row counts under Instances, not Archived (#1210)")
	assert.NotContains(t, view, "Archived (",
		"the Archived folder is absent while the only archived-liveness row is restoring")
}

// TestSidebar_NoArchivedFolderWhenEmpty (#1028): with nothing archived, the
// Archived folder header is not shown at all.
func TestSidebar_NoArchivedFolderWhenEmpty(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	s.SetSize(40, 40)

	for _, it := range s.visibleItems {
		if it.Kind == SectionArchived {
			t.Fatal("the Archived folder must be hidden when no session is archived")
		}
	}
	assert.NotContains(t, s.View(), "Archived")
}

// TestSidebar_ArchivedRowSelectableWhenExpanded (#1028): expanding the Archived
// folder reveals the archived row, and GetSelectedInstance resolves it (so the
// restore action and the Enter fence can read the selected archived session).
func TestSidebar_ArchivedRowSelectableWhenExpanded(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
	addTestInstance(s, archTestInstance(t, "put-away", session.Archived))
	s.SetSize(40, 40)

	// Move onto the Archived header and expand it.
	for i, it := range s.visibleItems {
		if it.Kind == SectionArchived && it.IsHeader {
			s.selectedIdx = i
			break
		}
	}
	s.ExpandSection()

	// The archived row is now visible; select it and resolve the instance.
	found := false
	for i, it := range s.visibleItems {
		if it.Kind == SectionArchived && !it.IsHeader {
			s.selectedIdx = i
			found = true
			break
		}
	}
	require.True(t, found, "expanding the Archived folder must reveal the archived row")

	inst := s.GetSelectedInstance()
	require.NotNil(t, inst, "an archived row must resolve to its instance")
	assert.Equal(t, "put-away", inst.Title)

	// The row renders with the distinct archived marker — the ▧ glyph, not a
	// name-eating "[archived] " text prefix (#1225) — and keeps its NAME visible.
	view := s.View()
	assert.True(t, strings.Contains(view, "▧"), "archived rows render the distinct ▧ marker")
	assert.True(t, strings.Contains(view, "put-away"), "archived rows keep their name visible")
	assert.False(t, strings.Contains(view, "[archived]"), "archived rows must not carry the name-eating text prefix (#1225)")
}

func TestSidebar_MoveCursorToArchivedInstance(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	liveInst := archTestInstance(t, "live-one", session.Ready)
	archivedInst := archTestInstance(t, "put-away", session.Archived)
	addTestInstance(s, liveInst)
	addTestInstance(s, archivedInst)
	s.SetSize(40, 40)

	s.SetSelectedInstance(0)
	require.Same(t, liveInst, s.GetSelectedInstance())

	s.ClickHeaderKind(SectionArchived)
	require.True(t, archivedRowVisible(s), "archived row must be visible after expanding Archived")

	s.proj.SelectInstance(archivedInst)
	s.syncFromStore()

	sel := s.rawSelection()
	assert.Equal(t, SectionArchived, sel.Kind, "cursor should move to the archived row")
	assert.False(t, sel.IsHeader)
	assert.Same(t, archivedInst, s.GetSelectedInstance())
	assert.Same(t, archivedInst, s.proj.GetSelectedInstance(),
		"sync must not reassert the previous live cursor row over the archived selection")
}

func TestSidebar_NavCrossesBetweenLiveTabsAndArchivedRows(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	liveInst := archTestInstance(t, "live-one", session.Ready)
	addAgentShellTabs(liveInst)
	archivedInst := archTestInstance(t, "put-away", session.Archived)
	addTestInstance(s, liveInst)
	addTestInstance(s, archivedInst)
	s.SetSize(40, 40)

	s.expandSectionKind(SectionArchived)
	require.True(t, archivedRowVisible(s), "archived row must be visible for the boundary walk")
	s.SetSelectedInstance(0)

	s.Down() // live Agent tab
	s.Down() // live Terminal tab, the last live tab stop
	sel := s.GetSelection()
	require.True(t, sel.IsTab)
	require.Equal(t, SectionInstances, sel.Kind)
	require.Equal(t, 1, sel.TabIndex)

	s.Down()
	sel = s.GetSelection()
	require.Equal(t, SectionArchived, sel.Kind, "Down after the last live tab reaches archived rows")
	require.False(t, sel.IsHeader)
	require.Same(t, archivedInst, s.GetSelectedInstance())

	s.Up()
	sel = s.GetSelection()
	require.Equal(t, SectionInstances, sel.Kind, "Up from the first archived row returns to live tabs")
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.TabIndex)
	require.Same(t, liveInst, s.GetSelectedInstance())
}

func TestSidebar_NavSkipsNonExpandableLiveRowsBeforeArchived(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())

	liveInst := archTestInstance(t, "live-one", session.Ready)
	addAgentShellTabs(liveInst)
	deletingInst := archTestInstance(t, "going-away", session.Deleting)
	archivedInst := archTestInstance(t, "put-away", session.Archived)
	addTestInstance(s, liveInst)
	addTestInstance(s, deletingInst)
	addTestInstance(s, archivedInst)
	s.SetSize(40, 40)
	s.expandSectionKind(SectionArchived)

	s.SetSelectedInstance(0)
	s.Down() // live Agent tab
	s.Down() // live Terminal tab, the last live tab stop
	require.True(t, s.GetSelection().IsTab)

	s.Down()
	sel := s.GetSelection()
	require.Equal(t, SectionArchived, sel.Kind,
		"Down after the last live tab skips non-expandable live rows and reaches archived rows")
	require.False(t, sel.IsHeader)
	require.Same(t, archivedInst, s.GetSelectedInstance())

	s.Up()
	sel = s.GetSelection()
	require.Equal(t, SectionInstances, sel.Kind,
		"Up from archived skips non-expandable live rows and returns to the last live tab")
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.TabIndex)
	require.Same(t, liveInst, s.GetSelectedInstance())

	s.SetSelectedInstance(1)
	sel = s.GetSelection()
	require.Equal(t, SectionInstances, sel.Kind)
	require.False(t, sel.IsTab, "precondition: explicit selection can rest on the deleting live title")
	require.Same(t, deletingInst, s.GetSelectedInstance())

	s.Down()
	sel = s.GetSelection()
	require.Equal(t, SectionArchived, sel.Kind,
		"Down from a non-expandable live title reaches the next selectable row")
	require.False(t, sel.IsHeader)
	require.Same(t, archivedInst, s.GetSelectedInstance())

	s.Up()
	sel = s.GetSelection()
	require.Equal(t, SectionInstances, sel.Kind)
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.TabIndex)
	require.Same(t, liveInst, s.GetSelectedInstance())
}

func archivedRowVisible(s *Sidebar) bool {
	for _, it := range s.visibleItems {
		if it.Kind == SectionArchived && !it.IsHeader && it.ItemIndex >= 0 {
			return true
		}
	}
	return false
}

// TestSidebar_ArchivedZonesRegistered (#1028 mouse P2): the Archived folder
// header gets its OWN zone id (distinct from the Instances header, so a click
// toggles the right folder), and — once expanded — an archived row registers a
// clickable TreeInstance zone so the mouse can select/act on it.
func TestSidebar_ArchivedZonesRegistered(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
	reg := zones.NewRegistry()
	s.SetZoneRegistry(reg)
	s.SetRect(layout.Rect{X: 0, Y: 0, W: 40, H: 40})

	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	addTestInstance(s, archTestInstance(t, "put-away", session.Archived))

	reg.Reset()
	_ = s.String()

	// Both headers register, on DISTINCT ids (no collision).
	_, okInst := reg.Find(zones.TreeHeader)
	require.True(t, okInst, "the Instances header zone must be registered")
	_, okArch := reg.Find(zones.TreeHeaderArchived)
	require.True(t, okArch, "the Archived folder header must get its own distinct zone")
	assert.NotEqual(t, zones.TreeHeader, zones.TreeHeaderArchived, "header zone ids must differ")

	// Collapsed by default → the archived row is not rendered, so no row zone yet.
	_, okRow := reg.Find(zones.TreeInstance("put-away"))
	require.False(t, okRow, "a collapsed Archived folder registers no archived-row zone")

	// Expand the Archived folder and re-render: the archived row now has a
	// clickable select zone, keyed by its title like a live instance row.
	s.ClickHeaderKind(SectionArchived)
	reg.Reset()
	_ = s.String()

	_, okRow = reg.Find(zones.TreeInstance("put-away"))
	require.True(t, okRow, "an expanded archived row must register a clickable TreeInstance zone")
	// The live instance's zone is still present (a click there selects it).
	_, okLive := reg.Find(zones.TreeInstance("live-one"))
	require.True(t, okLive)
}

// TestSidebar_ClickHeaderKindTogglesCorrectFolder (#1028 mouse P2): toggling the
// Archived header must collapse/expand the Archived folder ONLY, leaving the
// Instances section untouched — the behavior the distinct header zones enable.
func TestSidebar_ClickHeaderKindTogglesCorrectFolder(t *testing.T) {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	s := NewSidebar(&spin, false, store.NewProjection())
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	addTestInstance(s, archTestInstance(t, "put-away", session.Archived))
	s.SetSize(40, 40)

	instExpanded := func() bool { return s.sections[0].Expanded }
	archExpanded := func() bool { return s.sections[1].Expanded }
	require.True(t, instExpanded())
	require.False(t, archExpanded(), "Archived starts collapsed")

	// Toggle the Archived header: only the Archived folder flips.
	s.ClickHeaderKind(SectionArchived)
	assert.True(t, archExpanded(), "clicking the Archived header must expand the Archived folder")
	assert.True(t, instExpanded(), "the Instances section must be untouched")

	// Toggle the Instances header: only Instances flips.
	s.ClickHeader()
	assert.False(t, instExpanded(), "clicking the Instances header toggles Instances")
	assert.True(t, archExpanded(), "the Archived folder must be untouched")
}

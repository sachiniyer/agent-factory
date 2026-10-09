package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

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

// TestPartitionByArchived_ArchivedSortedNewestFirst (#1605): the archived group
// is re-sorted newest-created first (the inverse of the oldest-first live order),
// while the live partition keeps the projection order it arrives in. Instances
// come in oldest-first (LessInstanceOrder), so the archived indices must return
// reversed and the live indices in place.
func TestPartitionByArchived_ArchivedSortedNewestFirst(t *testing.T) {
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	mk := func(title string, status session.Status, ageMin int) *session.Instance {
		inst := archTestInstance(t, title, status)
		inst.CreatedAt = base.Add(time.Duration(ageMin) * time.Minute)
		return inst
	}

	// Oldest-first order, as the projection hands them over: two live, three
	// archived interleaved by creation time.
	instances := []*session.Instance{
		mk("live-old", session.Ready, 0),
		mk("arch-old", session.Archived, 1),
		mk("arch-mid", session.Archived, 2),
		mk("live-new", session.Ready, 3),
		mk("arch-new", session.Archived, 4),
	}

	live, archived := partitionByArchived(instances)

	// Live partition keeps the incoming (oldest-first) order untouched.
	require.Equal(t, []int{0, 3}, live, "live rows keep projection order")

	// Archived indices come back newest-created first: arch-new (4), arch-mid (2),
	// arch-old (1).
	gotTitles := make([]string, len(archived))
	for i, idx := range archived {
		gotTitles[i] = instances[idx].Title
	}
	require.Equal(t, []string{"arch-new", "arch-mid", "arch-old"}, gotTitles,
		"archived rows sort newest-created first")
}

// TestPartitionByArchived_EqualCreatedAtTieBreaksByTitle (#1605): identical
// CreatedAt values fall back to a Title order so the sort is total and never
// jitters between identical snapshots.
func TestPartitionByArchived_EqualCreatedAtTieBreaksByTitle(t *testing.T) {
	same := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	mk := func(title string) *session.Instance {
		inst := archTestInstance(t, title, session.Archived)
		inst.CreatedAt = same
		return inst
	}
	instances := []*session.Instance{mk("bravo"), mk("alpha"), mk("charlie")}

	_, archived := partitionByArchived(instances)
	gotTitles := make([]string, len(archived))
	for i, idx := range archived {
		gotTitles[i] = instances[idx].Title
	}
	require.Equal(t, []string{"alpha", "bravo", "charlie"}, gotTitles,
		"equal CreatedAt breaks the tie by Title ascending")
}

// TestSidebar_ArchivedPartitionedIntoFolder (#1028): a live session renders under
// Instances and an archived one under a separate "Archived" folder at the bottom
// (collapsed by default, so its row is hidden until expanded). The counts in the
// two headers reflect the partition.
func TestSidebar_ArchivedPartitionedIntoFolder(t *testing.T) {
	s := NewSidebar(store.NewProjection())

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
	assert.Contains(t, view, "Sessions (1)", "the header counts only live sessions")
	assert.Contains(t, view, "Archived (1)")
}

// TestSidebar_RestoringRowRehomedToInstances (#1210): a row mid-restore
// (OpRestoring overlay, liveness still Archived) renders under the live Instances
// section, not the Archived folder — the eager re-home the archive epic owed
// restore. Its liveness deliberately stays Archived so the snapshot reconcile
// still sees the Archived→live transition and runs its rebuild/re-Start (#1203).
func TestSidebar_RestoringRowRehomedToInstances(t *testing.T) {
	s := NewSidebar(store.NewProjection())

	restoring := archTestInstance(t, "coming-back", session.Archived)
	restoring.SetInFlightOpForTest(session.OpRestoring)
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	addTestInstance(s, restoring)
	s.SetSize(40, 40)

	require.Equal(t, session.LiveArchived, restoring.GetLiveness(),
		"the eager re-home must leave liveness Archived so the reconcile rebuild still fires (#1203)")
	require.False(t, restoring.ShownArchived(), "a mid-restore row is not shown as archived")

	view := s.View()
	assert.Contains(t, view, "Sessions (2)",
		"a mid-restore row counts under the live Sessions section, not Archived (#1210)")
	assert.NotContains(t, view, "Archived (",
		"the Archived folder is absent while the only archived-liveness row is restoring")
}

// TestSidebar_NoArchivedFolderWhenEmpty (#1028): with nothing archived, the
// Archived folder header is not shown at all.
func TestSidebar_NoArchivedFolderWhenEmpty(t *testing.T) {
	s := NewSidebar(store.NewProjection())
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
	s := NewSidebar(store.NewProjection())
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
	s := NewSidebar(store.NewProjection())

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
	s := NewSidebar(store.NewProjection())

	liveInst := archTestInstance(t, "live-one", session.Ready)
	addAgentShellTabs(liveInst)
	archivedInst := archTestInstance(t, "put-away", session.Archived)
	addTestInstance(s, liveInst)
	addTestInstance(s, archivedInst)
	s.SetSize(40, 40)

	require.False(t, archivedRowVisible(s), "Archived starts collapsed before the boundary walk")
	s.SetSelectedInstance(0)

	s.Down() // live Agent tab
	s.Down() // live Terminal tab, the last live tab stop
	sel := s.GetSelection()
	require.True(t, sel.IsTab)
	require.Equal(t, SectionInstances, sel.Kind)
	require.Equal(t, 1, sel.TabIndex)

	s.Down()
	sel = s.GetSelection()
	require.Equal(t, SectionArchived, sel.Kind,
		"Down after the last live tab auto-opens Archived and reaches archived rows")
	require.False(t, sel.IsHeader)
	require.True(t, archivedRowVisible(s), "Down at the live boundary must expand Archived")
	require.Same(t, archivedInst, s.GetSelectedInstance())

	s.Up()
	sel = s.GetSelection()
	require.Equal(t, SectionInstances, sel.Kind, "Up from the first archived row returns to live tabs")
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.TabIndex)
	require.Same(t, liveInst, s.GetSelectedInstance())
	require.False(t, archivedRowVisible(s),
		"Up back into the live instances must auto-collapse the Archived section (#1518 symmetry)")
}

// TestSidebar_NavUpFromArchivedAutoCollapses is the focused mirror of the #1518
// auto-open: Down at the live tail auto-expands the Archived folder, and Up back
// into the live instances auto-collapses it again.
func TestSidebar_NavUpFromArchivedAutoCollapses(t *testing.T) {
	s := NewSidebar(store.NewProjection())

	liveInst := archTestInstance(t, "live-one", session.Ready)
	addAgentShellTabs(liveInst)
	archivedInst := archTestInstance(t, "put-away", session.Archived)
	addTestInstance(s, liveInst)
	addTestInstance(s, archivedInst)
	s.SetSize(40, 40)

	require.False(t, archivedRowVisible(s), "Archived starts collapsed")
	s.SetSelectedInstance(0)

	// Nav down to the tail auto-expands Archived and lands on the archived row.
	s.Down() // live Agent tab
	s.Down() // live Terminal tab, the last live tab stop
	s.Down()
	require.Equal(t, SectionArchived, s.GetSelection().Kind, "Down at the tail enters Archived")
	require.True(t, archivedRowVisible(s), "Down at the live boundary auto-expands Archived")

	// Nav back up into the live instances auto-collapses Archived again.
	s.Up()
	sel := s.GetSelection()
	require.Equal(t, SectionInstances, sel.Kind, "Up returns to the live instances")
	require.True(t, sel.IsTab)
	require.Same(t, liveInst, s.GetSelectedInstance())
	require.False(t, archivedRowVisible(s), "Up back into live must auto-collapse Archived")
}

func TestSidebar_NavSkipsNonExpandableLiveRowsBeforeArchived(t *testing.T) {
	s := NewSidebar(store.NewProjection())

	liveInst := archTestInstance(t, "live-one", session.Ready)
	addAgentShellTabs(liveInst)
	deletingInst := archTestInstance(t, "going-away", session.Deleting)
	archivedInst := archTestInstance(t, "put-away", session.Archived)
	addTestInstance(s, liveInst)
	addTestInstance(s, deletingInst)
	addTestInstance(s, archivedInst)
	s.SetSize(40, 40)
	require.False(t, archivedRowVisible(s), "Archived starts collapsed before the boundary walk")

	s.SetSelectedInstance(0)
	s.Down() // live Agent tab
	s.Down() // live Terminal tab, the last live tab stop
	require.True(t, s.GetSelection().IsTab)

	s.Down()
	sel := s.GetSelection()
	require.Equal(t, SectionArchived, sel.Kind,
		"Down after the last live tab skips non-expandable live rows, auto-opens Archived, and reaches archived rows")
	require.False(t, sel.IsHeader)
	require.True(t, archivedRowVisible(s), "Down at the live boundary must expand Archived")
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

	for i, sec := range s.sections {
		if sec.Kind == SectionArchived {
			s.sections[i].Expanded = false
			break
		}
	}
	s.rebuildVisibleItems()
	require.False(t, archivedRowVisible(s), "Archived can be collapsed again before walking from the live tail")

	s.Down()
	sel = s.GetSelection()
	require.Equal(t, SectionArchived, sel.Kind,
		"Down from a non-expandable live title auto-opens Archived and reaches the next selectable row")
	require.False(t, sel.IsHeader)
	require.True(t, archivedRowVisible(s), "Down from the non-expandable live tail must expand Archived")
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
	s := NewSidebar(store.NewProjection())
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
	s := NewSidebar(store.NewProjection())
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

// TestSidebar_RowVerbTarget_ExpandedArchivedHeader pins the #4755 divergence:
// pressing `l` on "▶ Archived (1)" expands the folder WITHOUT moving the tree
// cursor — the cursor stays on the section header (GetSelectedInstance nil)
// while the store's sticky display selection keeps the archived row marked
// with ▾. A selection-scoped row verb (`r`, `D`, `c`) and the footer must act
// on the row the user SEES as selected: the bound resting row.
func TestSidebar_RowVerbTarget_ExpandedArchivedHeader(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	archivedInst := archTestInstance(t, "put-away", session.Archived)
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	addTestInstance(s, archivedInst)
	s.SetSize(40, 40)

	// The display binding points at the archived session (bound while it was
	// live — archiving mutates the row in place, so the pointer survives).
	s.proj.SelectInstance(archivedInst)
	// The issue's `l` on "▶ Archived (1)": cursor parks on the header, the
	// folder opens.
	s.ClickHeaderKind(SectionArchived)

	sel := s.GetSelection()
	require.True(t, sel.IsHeader, "precondition: cursor rests on the section header")
	require.Equal(t, SectionArchived, sel.Kind)
	require.Nil(t, s.GetSelectedInstance(),
		"precondition: the cursor selection resolves nil on a header")
	require.True(t, archivedRowVisible(s),
		"precondition: the archived row renders under the expanded header")
	require.Same(t, archivedInst, s.proj.GetSelectedInstance(),
		"precondition: the display binding still marks the archived row")

	assert.Same(t, archivedInst, s.RowVerbTarget(),
		"the ▾-marked row is the row a selection-scoped verb must act on")
}

// TestSidebar_RowVerbTarget_CursorRowWins: when the cursor is ON the archived
// row, the row verb target is that row — the resting-binding fallback must
// never override the cursor's own selection (here the display binding still
// names a different, live session).
func TestSidebar_RowVerbTarget_CursorRowWins(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	liveInst := archTestInstance(t, "live-one", session.Ready)
	archivedInst := archTestInstance(t, "put-away", session.Archived)
	addTestInstance(s, liveInst)
	addTestInstance(s, archivedInst)
	s.SetSize(40, 40)

	s.SetSelectedInstance(0)
	require.Same(t, liveInst, s.proj.GetSelectedInstance(),
		"precondition: the display binding names the live session")

	// Park the cursor on the archived row directly (an archived row never
	// pushes itself into the store binding — pushSelection only writes live
	// SectionInstances rows — so the binding keeps the live session).
	s.ClickHeaderKind(SectionArchived)
	parked := false
	for i, it := range s.visibleItems {
		if it.Kind == SectionArchived && !it.IsHeader {
			s.selectedIdx = i
			parked = true
			break
		}
	}
	require.True(t, parked, "the expanded folder must expose the archived row")
	require.Same(t, archivedInst, s.GetSelectedInstance())

	assert.Same(t, archivedInst, s.RowVerbTarget(),
		"the cursor's own row wins over a stale live binding")
}

// TestSidebar_RowVerbTarget_InvisibleBindingIsNil: the resting fallback applies
// only while the bound row is RENDERED — on a collapsed "▶ Archived" header no
// row carries the ▾ marker, so no row verb may resolve.
func TestSidebar_RowVerbTarget_InvisibleBindingIsNil(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	archivedInst := archTestInstance(t, "put-away", session.Archived)
	addTestInstance(s, archivedInst)
	s.SetSize(40, 40)

	s.proj.SelectInstance(archivedInst)
	// SelectInstance's re-pin lands the cursor on the archived row; h/← parks
	// it back on the now-collapsed section header — the row no longer renders.
	s.SetSelectedInstance(0)
	s.CollapseSection()

	sel := s.GetSelection()
	require.True(t, sel.IsHeader)
	require.Equal(t, SectionArchived, sel.Kind)
	require.False(t, archivedRowVisible(s),
		"precondition: the archived row is hidden while the folder is collapsed")

	assert.Nil(t, s.RowVerbTarget(),
		"no visible row is marked, so no row verb may resolve")
}

// TestSidebar_RowVerbTarget_OffscreenBindingIsNil: the resting fallback means
// "the row the ▾ marker shows" — and the marker only exists inside the
// rendered window. When an expanded Archived folder is taller than the
// sidebar, a bound row below the fold is in visibleItems but on no frame's
// screen; the footer and `r`/`D` must not resolve to it (#4755 review).
func TestSidebar_RowVerbTarget_OffscreenBindingIsNil(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	var archivedRows []*session.Instance
	for i := 0; i < 12; i++ {
		inst := archTestInstance(t, fmt.Sprintf("arch-%02d", i), session.Archived)
		archivedRows = append(archivedRows, inst)
		addTestInstance(s, inst)
	}
	// A short allocation: the expanded folder's tail rows fall below the fold.
	s.SetSize(40, 8)

	// Bind the OLDEST archived row — the folder renders newest-first, so it
	// lands at the tail, below the fold — then park the cursor on the folder
	// header, which stays in view while the bound row is rendered off-screen.
	bound := archivedRows[0]
	s.proj.SelectInstance(bound)
	s.ClickHeaderKind(SectionArchived)
	_ = s.View() // one frame establishes the fitted window

	sel := s.GetSelection()
	require.True(t, sel.IsHeader, "precondition: cursor rests on the Archived header")
	boundIdx := -1
	for j, it := range s.visibleItems {
		if isInstanceRow(it) && !it.IsTab {
			if inst := s.proj.GetInstances()[it.ItemIndex]; inst == bound {
				boundIdx = j
				break
			}
		}
	}
	require.GreaterOrEqual(t, boundIdx, 0, "precondition: the bound row is in the expanded list")
	require.True(t, boundIdx < s.renderedStart || boundIdx >= s.renderedEnd,
		"precondition: the bound row (index %d) is outside the rendered window [%d,%d)",
		boundIdx, s.renderedStart, s.renderedEnd)

	assert.Nil(t, s.RowVerbTarget(),
		"a binding whose marker is off-screen must not take row verbs")

	// Rebind the NEWEST archived row — first under the header, inside the
	// window — and the same fallback resolves it again.
	s.proj.SelectInstance(archivedRows[len(archivedRows)-1])
	_ = s.View() // the seq bump re-pins the cursor on the row; re-park below
	for i, it := range s.visibleItems {
		if it.IsHeader && it.Kind == SectionArchived {
			s.selectedIdx = i
			break
		}
	}
	_ = s.View()
	assert.Same(t, archivedRows[len(archivedRows)-1], s.RowVerbTarget(),
		"the bound row inside the rendered window still resolves")
}

// TestSidebar_RowVerbTarget_LiveBindingNotAdopted: a header never carries
// live-row verbs — the store's sticky binding is the WORKSPACE selection (the
// panes keep showing it while the cursor tours headers), not a row the user
// sees as selected. Only a resting binding composes with a header cursor.
func TestSidebar_RowVerbTarget_LiveBindingNotAdopted(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	liveInst := archTestInstance(t, "live-one", session.Ready)
	addTestInstance(s, liveInst)
	addTestInstance(s, archTestInstance(t, "put-away", session.Archived))
	s.SetSize(40, 40)

	s.SetSelectedInstance(0)
	require.Same(t, liveInst, s.proj.GetSelectedInstance())

	// Cursor on the expanded Instances header.
	s.ClickHeader()
	s.ClickHeader()

	sel := s.GetSelection()
	require.True(t, sel.IsHeader)
	require.Equal(t, SectionInstances, sel.Kind)
	assert.Nil(t, s.RowVerbTarget(),
		"a live display binding must not leak row verbs onto a section header")
}

// TestSidebar_RowVerbTarget_ForeignHeaderIsNil: the fallback must scope to the
// section the cursor's header owns (#4755 review). A bound archived row keeps
// its ▾ marker while the cursor tours the Sessions header — but that header
// has no relation to the Archived folder, so `r`/`D` must not reach the bound
// row through it.
func TestSidebar_RowVerbTarget_ForeignHeaderIsNil(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	archivedInst := archTestInstance(t, "put-away", session.Archived)
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	addTestInstance(s, archivedInst)
	s.SetSize(40, 40)

	s.proj.SelectInstance(archivedInst)
	s.ClickHeaderKind(SectionArchived) // expanded, cursor parked on its header
	require.Same(t, archivedInst, s.RowVerbTarget(),
		"precondition: the Archived header adopts its bound row")

	// Move the cursor onto the Sessions header — the bound row still renders
	// its marker, but under a different section.
	for i, it := range s.visibleItems {
		if it.IsHeader && it.Kind == SectionInstances {
			s.selectedIdx = i
			break
		}
	}
	_ = s.View()
	sel := s.GetSelection()
	require.True(t, sel.IsHeader)
	require.Equal(t, SectionInstances, sel.Kind)

	assert.Nil(t, s.RowVerbTarget(),
		"the Sessions header must not adopt the Archived folder's bound row")
}

// TestSidebar_RowVerbTarget_FencedRestingRowAdopted: a resting row whose
// lifecycle verb is fenced — startup-unknown Lost, so LifecycleAction() is
// None but CanKill() still holds — is still the row the ▾ marker shows as
// selected. The fallback must adopt it (classified by resting LIVENESS, not
// by the restore verb) so the header still reaches the actions the row can
// take; each handler applies its own capability gate (#4755 review).
func TestSidebar_RowVerbTarget_FencedRestingRowAdopted(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	lostInst := archTestInstance(t, "lost-one", session.Lost)
	lostInst.MarkStartupStateUnknown()
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	addTestInstance(s, lostInst)
	s.SetSize(40, 40)

	require.Equal(t, session.LifecycleActionNone, lostInst.LifecycleAction(),
		"precondition: startup-unknown fences the lifecycle verb")
	require.True(t, lostInst.CanKill(), "precondition: the row still takes D")

	s.proj.SelectInstance(lostInst)
	s.syncFromStore() // the seq bump re-pins the cursor onto the row
	require.Same(t, lostInst, s.GetSelectedInstance(),
		"precondition: cursor sits on the resting row")
	require.Same(t, lostInst, s.RowVerbTarget())

	// Park the cursor on the Sessions header — the section that renders a
	// lost row — while its marker stays bound.
	for i, it := range s.visibleItems {
		if it.IsHeader && it.Kind == SectionInstances {
			s.selectedIdx = i
			break
		}
	}
	_ = s.View()
	sel := s.GetSelection()
	require.True(t, sel.IsHeader)
	require.Equal(t, SectionInstances, sel.Kind)

	assert.Same(t, lostInst, s.RowVerbTarget(),
		"a fenced-but-resting bound row keeps its row verbs on its own header")
}

// TestSidebar_RowVerbTarget_StaleWindowAfterExpand: the fitted window is only
// valid for the row list it was computed against. Expanding the collapsed
// Archived folder rebuilds visibleItems — the bound row re-enters the list at
// an index beyond the previous frame's renderedEnd — and the same key update
// asks RowVerbTarget for a verdict BEFORE String() re-fits. A bound row must
// not be refused off the stale window: the fallback composes with the
// CURRENT list until the next render establishes fresh bounds (#4755 review).
func TestSidebar_RowVerbTarget_StaleWindowAfterExpand(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	archivedInst := archTestInstance(t, "put-away", session.Archived)
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	addTestInstance(s, archivedInst)
	s.SetSize(40, 40)

	s.proj.SelectInstance(archivedInst)
	s.ClickHeaderKind(SectionArchived) // expanded, cursor parked on its header
	_ = s.View()                       // a frame fixes the window on the open list
	require.Same(t, archivedInst, s.RowVerbTarget(),
		"precondition: the expanded header adopts its bound row")

	s.CollapseSection() // h on the header folds the folder back up
	_ = s.View()        // the collapsed frame re-fits: bounds describe the short list
	s.ExpandSection()   // l: the rebuild re-adds the bound row — before any render
	sel := s.GetSelection()
	require.True(t, sel.IsHeader)
	require.Equal(t, SectionArchived, sel.Kind)

	assert.Same(t, archivedInst, s.RowVerbTarget(),
		"the row the expand just revealed must not be refused on stale bounds")
}

// TestSidebar_ArchiveClearsTabCollapse: treeCollapsed is the user's explicit
// fold of the BOUND live row's tab subtree. Archiving re-homes that row into
// the Archived folder where no tab children render — the stale override then
// only suppresses the ▾ marker, leaving a row the header fallback would adopt
// for `r`/`D` while nothing marks it selected (#4755 review). The archive
// transition must drop the dead override so the bound row keeps its marker.
func TestSidebar_ArchiveClearsTabCollapse(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	inst := archTestInstance(t, "put-away", session.Ready)
	addTestInstance(s, inst)
	s.SetSize(40, 40)

	s.proj.SelectInstance(inst) // cursor + binding on the soon-to-be-archived row
	s.syncFromStore()
	s.treeCollapsed = inst.Title
	require.False(t, s.instanceExpanded(inst),
		"precondition: the collapse override suppresses the ▾ marker")

	// Archive in place, the way the reconcile delivers it: the same instance
	// pointer flips liveness. The cursor clamps onto the Archived header — no
	// live row takes the push — so the binding keeps pointing at it.
	inst.SetStatusForTest(session.Archived)
	s.syncFromStore()

	require.Same(t, inst, s.proj.GetSelectedInstance(),
		"precondition: the archived row stays display-bound")
	require.Empty(t, s.treeCollapsed,
		"a collapse override for a row with no tab subtree must not persist")
	require.True(t, s.instanceExpanded(inst),
		"the bound archived row renders its ▾ marker again")

	s.ClickHeaderKind(SectionArchived)
	assert.Same(t, inst, s.RowVerbTarget(),
		"the marker-visible bound row resolves on its own header")
}

// TestSidebar_RowVerbTarget_CollapsedMarkerNotAdopted: the header fallback
// adopts the bound row because the user SEES it selected — the ▾ marker is
// the evidence. A bound resting row whose marker is suppressed (a live row's
// tabs folded with h/←, then lost) renders ▸ like every unexpanded row, and
// no header may lend it `r`/`D` (#4755 review).
func TestSidebar_RowVerbTarget_CollapsedMarkerNotAdopted(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	lostInst := archTestInstance(t, "lost-one", session.Lost)
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	addTestInstance(s, lostInst)
	s.SetSize(40, 40)

	s.proj.SelectInstance(lostInst)
	s.syncFromStore()
	require.Same(t, lostInst, s.proj.GetSelectedInstance())

	// The fold the user set while the row was live survives the transition.
	s.treeCollapsed = lostInst.Title
	require.False(t, s.instanceExpanded(lostInst),
		"precondition: the bound row renders ▸, not the ▾ marker")

	// Park the cursor on the Sessions header — the section a lost row
	// renders under.
	for i, it := range s.visibleItems {
		if it.IsHeader && it.Kind == SectionInstances {
			s.selectedIdx = i
			break
		}
	}
	_ = s.View()
	sel := s.GetSelection()
	require.True(t, sel.IsHeader)
	require.Equal(t, SectionInstances, sel.Kind)

	assert.Nil(t, s.RowVerbTarget(),
		"a bound row without its ▾ marker is not visibly selected — no verbs")
}

// TestSidebar_RowVerbTarget_ResizeRefits: the fitted window assumes the
// allocation it was computed against. A resize moves the fold without any
// rebuild — a bound row that was on screen can cross it — so SetSize marks
// the window stale and the fallback refits before answering (#4755 review).
func TestSidebar_RowVerbTarget_ResizeRefits(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	addTestInstance(s, archTestInstance(t, "live-one", session.Ready))
	var archivedRows []*session.Instance
	for i := 0; i < 12; i++ {
		inst := archTestInstance(t, fmt.Sprintf("arch-%02d", i), session.Archived)
		archivedRows = append(archivedRows, inst)
		addTestInstance(s, inst)
	}
	bound := archivedRows[0] // oldest → tail of the newest-first folder
	s.SetSize(40, 40)

	s.proj.SelectInstance(bound)
	s.ClickHeaderKind(SectionArchived)
	_ = s.View()
	require.Same(t, bound, s.RowVerbTarget(),
		"precondition: the bound tail row fits at the large height")

	// Shrink the rail: the bound tail row is now below the fold, with no
	// rebuild and no intervening frame to re-fit the window.
	s.SetSize(40, 6)
	assert.Nil(t, s.RowVerbTarget(),
		"a resize that scrolls the bound row off must drop it immediately")
}

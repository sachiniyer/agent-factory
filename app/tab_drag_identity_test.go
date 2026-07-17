package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A drag captures its target at press and acts on it at release — a human-time
// window in which another client can permute the roster underneath the gesture.
// The instance half of this was already hardened (the drop re-resolves it by
// title — TestMouse_DragDropReresolvesInstanceAfterProjectionSwap); these are the
// TAB half, which was still a raw ordinal.
//
// The window is newly reachable: #1813 is what first lets an out-of-band reorder
// reach a running TUI, and there is no TUI reorder gesture, so every reorder a
// drag sees is out-of-band. Assertions read the tab's ID, never its index.

// dragRosterHome builds a home with alpha holding [agent, shell, a, b] and one
// open agent pane, and returns the pane's region for use as a drop target.
func dragRosterHome(t *testing.T) (*home, *session.Instance, string) {
	t.Helper()
	h, alpha, _ := mouseTestHome(t)
	newFakeClock(h)
	alpha.SetStatusForTest(session.Running)
	_, err := alpha.AddWebTab("http://localhost:3000", "a")
	require.NoError(t, err)
	_, err = alpha.AddWebTab("http://localhost:3001", "b")
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"},
		[]string{alpha.GetTabs()[2].Name, alpha.GetTabs()[3].Name})
	paneAgent := openTestPane(t, h, alpha, 0)
	return h, alpha, layout.PaneRegion(paneAgent.ID())
}

// reorderLastTwoTabs swaps the instance's last two tabs through the REAL snapshot
// reconcile — an out-of-band reorder from another client.
func reorderLastTwoTabs(t *testing.T, h *home, inst *session.Instance) {
	t.Helper()
	data := inst.ToInstanceData()
	last := len(data.Tabs) - 1
	data.Tabs[last-1], data.Tabs[last] = data.Tabs[last], data.Tabs[last-1]
	require.True(t, h.reconcileSnapshot([]session.InstanceData{data}))
}

// TestMouse_DragDropFollowsTabAcrossConcurrentReorder: the user grabs tab "a",
// another client reorders mid-drag, and the drop must open "a" — the tab the
// gesture actually grabbed and whose label the drag ghost is rendering — not
// whatever slid into the ordinal.
func TestMouse_DragDropFollowsTabAcrossConcurrentReorder(t *testing.T) {
	h, alpha, regionAgent := dragRosterHome(t)

	tab := zoneRect(t, h, zones.TreeTab(alpha.Title, 2))
	body := zoneRect(t, h, zones.PaneBody(regionAgent))
	press(h, tab.X, tab.Y)
	motion(h, body.X+3, body.Y+4)
	require.NotNil(t, h.tabDrag)
	require.True(t, h.tabDrag.active)
	wantID := alpha.GetTabs()[2].ID
	require.NotEmpty(t, wantID)

	reorderLastTwoTabs(t, h, alpha)
	require.Equal(t, []string{"b", "a"},
		[]string{alpha.GetTabs()[2].Name, alpha.GetTabs()[3].Name}, "roster permuted mid-drag")

	release(h, body.X+3, body.Y+4)

	dropped := h.store.OpenPanes()[h.store.NumOpenPanes()-1]
	assert.Equal(t, wantID, alpha.GetTabs()[dropped.Tab()].ID,
		"the drop opens the tab the user actually grabbed")
	assert.Equal(t, "a", alpha.GetTabs()[dropped.Tab()].Name)
}

// TestMouse_TabClickFollowsTabAcrossConcurrentReorder: a click is a press and a
// release with no motion, so it carries the same staleness over a shorter window
// — and it selects the tree's tab, which is what `w` then closes.
func TestMouse_TabClickFollowsTabAcrossConcurrentReorder(t *testing.T) {
	h, alpha, _ := dragRosterHome(t)

	tab := zoneRect(t, h, zones.TreeTab(alpha.Title, 2))
	press(h, tab.X, tab.Y)
	require.NotNil(t, h.tabDrag)
	require.False(t, h.tabDrag.active, "no motion: still a click")
	wantID := alpha.GetTabs()[2].ID

	reorderLastTwoTabs(t, h, alpha)

	release(h, tab.X, tab.Y)

	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab, "the click lands on a tab row")
	assert.Equal(t, wantID, alpha.GetTabs()[sel.TabIndex].ID,
		"the click selects the tab the user actually clicked")
	assert.Equal(t, wantID, alpha.GetTabs()[h.store.ActiveTab()].ID,
		"and the active tab agrees, so a following w closes the tab on screen")
}

// TestMouse_DragDropOfConcurrentlyClosedTabIsNoOp: when the grabbed tab is GONE
// by release, no target is the right answer. Falling back to the captured ordinal
// would open a different tab under the guise of being helpful — the failure the
// id-keying exists to prevent.
func TestMouse_DragDropOfConcurrentlyClosedTabIsNoOp(t *testing.T) {
	h, alpha, regionAgent := dragRosterHome(t)
	require.Equal(t, 1, h.store.NumOpenPanes())

	tab := zoneRect(t, h, zones.TreeTab(alpha.Title, 2))
	body := zoneRect(t, h, zones.PaneBody(regionAgent))
	press(h, tab.X, tab.Y)
	motion(h, body.X+3, body.Y+4)
	require.NotNil(t, h.tabDrag)

	// Another client closes "a" — the dragged tab — leaving [agent, shell, b], so
	// the captured ordinal 2 still resolves, now to "b".
	data := alpha.ToInstanceData()
	data.Tabs = append(data.Tabs[:2], data.Tabs[3])
	require.True(t, h.reconcileSnapshot([]session.InstanceData{data}))
	require.Equal(t, 3, alpha.TabCount())
	require.Equal(t, "b", alpha.GetTabs()[2].Name, "the ordinal is still in range, and names a DIFFERENT tab")

	release(h, body.X+3, body.Y+4)

	assert.Equal(t, 1, h.store.NumOpenPanes(),
		"the grabbed tab is gone: the drop opens nothing rather than a different tab")
}

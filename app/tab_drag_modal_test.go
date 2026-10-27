package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// A tab-row press captures m.tabDrag in stateDefault; the gesture acts at the
// later release. A modal opened between the two (a keyboard key like `?`
// pressed while the mouse button is held) does NOT clear m.tabDrag — only
// release/wheel/second-button do — so the release used to dispatch the click
// (SelectTabRow + focusRegionClick) or the active drop (openOrFocusPane)
// underneath the overlay. These exercise the stateDefault guard
// handleTabDragRelease now mirrors from handleTabDragPress: a release under an
// open modal drops the stale drag and swallows the event, so nothing mutates
// behind a modal (#1774 class, introduced in 3205c81c8 alongside the gesture).

// TestMouse_TabDragReleaseBehindModalDoesNotClick: a tree-tab click path taken
// behind a help overlay must not select the tab or move focus — the release is
// swallowed and the stale drag is dropped so the next gesture starts clean.
func TestMouse_TabDragReleaseBehindModalDoesNotClick(t *testing.T) {
	h, alpha, _ := mouseTestHome(t)
	newFakeClock(h)
	_ = h.View()
	require.False(t, h.sidebar.GetSelection().IsTab, "precondition: starts on instance row")
	h.focusRegion(layout.RegionAutomations)
	require.Equal(t, layout.RegionAutomations, h.ring.Active(), "precondition: focus off the tree")

	tab := zoneRect(t, h, zones.TreeTab(alpha.Title, 1))

	// A press in stateDefault captures a drag without touching selection/focus.
	press(h, tab.X, tab.Y)
	require.NotNil(t, h.tabDrag)
	require.False(t, h.tabDrag.active)
	assert.False(t, h.sidebar.GetSelection().IsTab, "press does not select the tab")
	assert.Equal(t, layout.RegionAutomations, h.ring.Active(), "press does not move focus")

	// A keyboard key opens the help overlay mid-drag. Update's deferred
	// clearStaleClickTrackerAfter clears the double-click tracker, but NOT
	// m.tabDrag — the captured drag survives the modal.
	_, _ = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	require.Equal(t, stateHelp, h.state, "the help overlay opened")
	require.NotNil(t, h.tabDrag, "modal open does NOT clear the captured drag")

	// Release behind the modal — the click path must not run under the overlay.
	release(h, tab.X, tab.Y)

	assert.Equal(t, stateHelp, h.state, "the release must not disrupt the modal")
	assert.Nil(t, h.tabDrag, "the release drops the stale drag state")
	sel := h.sidebar.GetSelection()
	assert.False(t, sel.IsTab, "release must not select a tab behind the open modal")
	assert.Equal(t, layout.RegionAutomations, h.ring.Active(),
		"release must not move focus to the tree behind the modal")
}

// TestMouse_TabDragActiveDropBehindModalDoesNotOpenPane: an active drag (motion
// past the threshold) released over a pane body while a help overlay is open
// must not call openOrFocusPane — no pane is opened behind the modal.
func TestMouse_TabDragActiveDropBehindModalDoesNotOpenPane(t *testing.T) {
	h, alpha, _ := mouseTestHome(t)
	newFakeClock(h)
	_ = h.View()

	paneAgent := openTestPane(t, h, alpha, 0)
	regionAgent := layout.PaneRegion(paneAgent.ID())
	require.Equal(t, 1, h.store.NumOpenPanes())

	tab := zoneRect(t, h, zones.TreeTab(alpha.Title, 1))
	body := zoneRect(t, h, zones.PaneBody(regionAgent))

	// Press tab 1 in stateDefault, then motion past the threshold activates the drag.
	press(h, tab.X, tab.Y)
	require.NotNil(t, h.tabDrag)
	require.False(t, h.tabDrag.active)

	motion(h, body.X+3, body.Y+4)
	require.True(t, h.tabDrag.active, "motion past threshold activates the drag")

	// A keyboard key opens the help overlay mid-drag; the drag stays captured and active.
	_, _ = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	require.Equal(t, stateHelp, h.state, "help overlay opened mid-drag")
	require.NotNil(t, h.tabDrag, "modal open does NOT clear the captured drag")
	require.True(t, h.tabDrag.active, "drag remains active under the modal")

	// Release over the pane body — the active-drop path must not run.
	release(h, body.X+3, body.Y+4)

	assert.Equal(t, stateHelp, h.state, "the release must not disrupt the modal")
	assert.Nil(t, h.tabDrag, "release drops the stale drag state")
	assert.Equal(t, 1, h.store.NumOpenPanes(),
		"the active drop must not open a pane behind the open help modal")
	panes := h.store.OpenPanes()
	require.Len(t, panes, 1)
	assert.Same(t, paneAgent, panes[0], "the only open pane is the original one")
}

// TestMouse_TabDragMotionUnderModalStillReleasesSafely: handleTabDragMotion is
// ungated on state, so a motion arriving under an overlay can still promote a
// captured-but-inactive drag to active. The release guard must keep the drop
// inert in that case too, so the motion-promoted path can never open a pane
// behind a modal.
func TestMouse_TabDragMotionUnderModalStillReleasesSafely(t *testing.T) {
	h, alpha, _ := mouseTestHome(t)
	newFakeClock(h)
	_ = h.View()

	paneAgent := openTestPane(t, h, alpha, 0)
	regionAgent := layout.PaneRegion(paneAgent.ID())
	require.Equal(t, 1, h.store.NumOpenPanes())

	tab := zoneRect(t, h, zones.TreeTab(alpha.Title, 1))
	body := zoneRect(t, h, zones.PaneBody(regionAgent))

	press(h, tab.X, tab.Y)
	require.NotNil(t, h.tabDrag)
	require.False(t, h.tabDrag.active)

	// The modal opens BEFORE the drag crosses the threshold.
	_, _ = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	require.Equal(t, stateHelp, h.state)
	require.False(t, h.tabDrag.active, "pre-threshold drag is still inactive under the modal")

	// Motion under the overlay promotes the inactive drag to active.
	motion(h, body.X+3, body.Y+4)
	require.True(t, h.tabDrag.active, "motion ungated on state promotes the drag under the modal")

	release(h, body.X+3, body.Y+4)

	assert.Equal(t, stateHelp, h.state, "the release must not disrupt the modal")
	assert.Nil(t, h.tabDrag, "release drops the stale drag state")
	assert.Equal(t, 1, h.store.NumOpenPanes(),
		"the drop must not open a pane behind the modal even when motion promoted the drag under the overlay")
	panes := h.store.OpenPanes()
	require.Len(t, panes, 1)
	assert.Same(t, paneAgent, panes[0])
}

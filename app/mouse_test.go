package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/overlay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// Root mouse routing (#1024 PR 6, closes #1025): every gesture in the RFC
// §2.5 table, driven hermetically — the click coordinates are derived FROM
// the zone registry the last View() rebuilt (never hardcoded), so a layout
// change moves the tests with it or fails loudly.
// ----------------------------------------------------------------------------

// fakeClock drives the double-click detector deterministically.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }
func newFakeClock(h *home) *fakeClock {
	c := &fakeClock{now: time.Unix(1000, 0)}
	h.mouseClock = c.Now
	return c
}

// mouseTestHome is a home with two started instances and two tasks at a
// split-capable size, selection on "alpha", tree focused.
func mouseTestHome(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	h.store.AddInstance(instanceWithFakeBackend(t, "alpha"))
	h.store.AddInstance(instanceWithFakeBackend(t, "beta"))
	tasks := []task.Task{
		{ID: "task-aaa", Name: "nightly-sweep", CronExpr: "0 3 * * *", Enabled: true},
		{ID: "task-bbb", Name: "log-watch", WatchCmd: "tail -f x", Enabled: true},
	}
	h.store.SetTasks(tasks)
	h.automations.TaskPane().SetTasks(tasks)
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 140, 40)
	return h
}

// zoneRect renders a frame (rebuilding the registry) and returns id's rect.
func zoneRect(t *testing.T, h *home, id string) layout.Rect {
	t.Helper()
	_ = h.View()
	r, ok := h.zones.Find(id)
	require.True(t, ok, "zone %q must be registered; frame has %v", id, h.zones.IDs())
	return r
}

// press injects a left press at (x, y) through the root mouse router.
func press(h *home, x, y int) tea.Cmd {
	_, cmd := h.handleMouse(tea.MouseMsg{
		X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	})
	return cmd
}

// clickZone renders, resolves id's rect, and clicks its top-left cell.
func clickZone(t *testing.T, h *home, id string) tea.Cmd {
	t.Helper()
	r := zoneRect(t, h, id)
	return press(h, r.X, r.Y)
}

// wheel injects a wheel event at (x, y).
func wheel(h *home, x, y int, up bool) {
	b := tea.MouseButtonWheelDown
	if up {
		b = tea.MouseButtonWheelUp
	}
	_, _ = h.handleMouse(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: b})
}

// TestMouse_ClickInstanceRowSelects: clicking a tree instance row selects it
// (retargeting pane A) and focuses the tree, from wherever focus was.
func TestMouse_ClickInstanceRowSelects(t *testing.T) {
	h := mouseTestHome(t)
	newFakeClock(h)
	h.focusRegion(layout.RegionPaneA)

	clickZone(t, h, zones.TreeInstance("beta"))

	require.NotNil(t, h.store.GetSelectedInstance())
	assert.Equal(t, "beta", h.store.GetSelectedInstance().Title,
		"clicking an instance row retargets pane A")
	assert.Equal(t, layout.RegionTree, h.ring.Active(), "the click focuses the tree")
}

// TestMouse_ClickTreeTabRowSelectsTab: clicking a tab child row drives the
// store's active tab, exactly like landing the tree cursor on it.
func TestMouse_ClickTreeTabRowSelectsTab(t *testing.T) {
	h := mouseTestHome(t)
	newFakeClock(h)

	clickZone(t, h, zones.TreeTab("alpha", 1))

	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab, "the cursor lands on the tab row")
	assert.Equal(t, 1, sel.TabIndex)
	assert.Equal(t, 1, h.store.ActiveTab(), "the click drives pane A's active tab")
}

// TestMouse_ClickArrowTogglesExpansion: the ▸/▾ arrow collapses/expands the
// instance's tab children without attaching or moving pane A off it.
func TestMouse_ClickArrowTogglesExpansion(t *testing.T) {
	h := mouseTestHome(t)
	clock := newFakeClock(h)

	// alpha is selected and expanded: its tab rows have zones.
	_ = zoneRect(t, h, zones.TreeTab("alpha", 0))

	clickZone(t, h, zones.TreeArrow("alpha"))
	_ = h.View()
	_, ok := h.zones.Find(zones.TreeTab("alpha", 0))
	assert.False(t, ok, "arrow click collapses the expanded selection")

	clock.advance(time.Second) // not a double click
	clickZone(t, h, zones.TreeArrow("alpha"))
	_ = h.View()
	_, ok = h.zones.Find(zones.TreeTab("alpha", 0))
	assert.True(t, ok, "second arrow click re-expands")

	// Arrow on a collapsed non-selected instance selects it, which
	// auto-expands its subtree.
	clock.advance(time.Second)
	clickZone(t, h, zones.TreeArrow("beta"))
	assert.Equal(t, "beta", h.store.GetSelectedInstance().Title)
	_ = h.View()
	_, ok = h.zones.Find(zones.TreeTab("beta", 0))
	assert.True(t, ok, "selecting via the arrow expands the new selection")
}

// TestMouse_DoubleClickInstanceAttaches: two quick clicks on a tree row
// attach it full-screen through the exact seam Enter uses; a slow second
// click stays a plain re-select.
func TestMouse_DoubleClickInstanceAttaches(t *testing.T) {
	resetDetachWatchdog(t)
	h := mouseTestHome(t)
	clock := newFakeClock(h)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	var attachedLabel string
	swapAttachOverlayCallbackFn(t, func(m *home, label, traceSuffix string, rem bool, _ func() (chan struct{}, error)) tea.Cmd {
		attachedLabel = label
		return m.attachOverlayCallback(label, traceSuffix, rem, func() (chan struct{}, error) {
			ch := make(chan struct{})
			close(ch)
			return ch, nil
		})
	})

	// Slow clicks: select only, no attach.
	clickZone(t, h, zones.TreeInstance("beta"))
	clock.advance(time.Second)
	clickZone(t, h, zones.TreeInstance("beta"))
	assert.Empty(t, attachedLabel, "clicks slower than the double-click interval must not attach")

	// Fast second click: attach, same path as Enter on the selection.
	clock.advance(time.Second)
	clickZone(t, h, zones.TreeInstance("beta"))
	clock.advance(50 * time.Millisecond)
	clickZone(t, h, zones.TreeInstance("beta"))
	assert.Equal(t, "handleEnter-sidebar", attachedLabel,
		"double-click attaches through the exact Enter seam")
	endDetachWatchdog()

	// Double-click on a TAB row attaches that tab (the terminal path).
	attachedLabel = ""
	clock.advance(time.Second)
	clickZone(t, h, zones.TreeTab("beta", 1))
	clock.advance(50 * time.Millisecond)
	clickZone(t, h, zones.TreeTab("beta", 1))
	assert.Equal(t, "handleEnter-terminal", attachedLabel,
		"double-click on a terminal tab row attaches that tab")
	endDetachWatchdog()
}

// TestMouse_ClickPaneHeaderFocusesPane: header clicks pick the focused pane —
// in a split, either A or B.
func TestMouse_ClickPaneHeaderFocusesPane(t *testing.T) {
	h := mouseTestHome(t)
	newFakeClock(h)
	pressKey(t, h, "s")
	require.True(t, h.lastLayout.SplitActive)

	clickZone(t, h, zones.PaneHeader(layout.RegionPaneB))
	assert.Equal(t, layout.RegionPaneB, h.ring.Active(), "pane B header click focuses pane B")

	clickZone(t, h, zones.PaneHeader(layout.RegionPaneA))
	assert.Equal(t, layout.RegionPaneA, h.ring.Active(), "pane A header click focuses pane A")
}

// TestMouse_PaneBodyClickFocusesThenAttaches: a body click on an unfocused
// pane focuses it; a click on the already-focused pane's body attaches (the
// §2.5 "click a focused pane body → attach" row).
func TestMouse_PaneBodyClickFocusesThenAttaches(t *testing.T) {
	resetDetachWatchdog(t)
	h := mouseTestHome(t)
	clock := newFakeClock(h)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	var attachedLabel string
	swapAttachOverlayCallbackFn(t, func(m *home, label, traceSuffix string, rem bool, _ func() (chan struct{}, error)) tea.Cmd {
		attachedLabel = label
		return m.attachOverlayCallback(label, traceSuffix, rem, func() (chan struct{}, error) {
			ch := make(chan struct{})
			close(ch)
			return ch, nil
		})
	})

	// Tree has focus: the first body click only focuses pane A. Click below
	// the header row so the header zone can't win the hit test.
	body := zoneRect(t, h, zones.PaneBody(layout.RegionPaneA))
	press(h, body.X+2, body.Y+4)
	assert.Equal(t, layout.RegionPaneA, h.ring.Active(), "body click on an unfocused pane focuses it")
	assert.Empty(t, attachedLabel, "the focusing click must not attach")

	// A later click on the focused pane's body attaches its binding.
	clock.advance(time.Second)
	body = zoneRect(t, h, zones.PaneBody(layout.RegionPaneA))
	press(h, body.X+2, body.Y+4)
	assert.Equal(t, "handleEnter-sidebar", attachedLabel,
		"click on the focused pane body attaches, like Enter")
	endDetachWatchdog()

	// Same contract for the pinned pane: focus it by header, then click its
	// body — the attach must take the pane-B path.
	attachedLabel = ""
	clock.advance(time.Second)
	pressKey(t, h, "s")
	require.True(t, h.lastLayout.SplitActive)
	clickZone(t, h, zones.PaneHeader(layout.RegionPaneB))
	require.Equal(t, layout.RegionPaneB, h.ring.Active())
	clock.advance(time.Second)
	bodyB := zoneRect(t, h, zones.PaneBody(layout.RegionPaneB))
	press(h, bodyB.X+2, bodyB.Y+4)
	assert.Equal(t, "handleEnter-paneB", attachedLabel,
		"click on the focused pane B body attaches the pinned binding")
	endDetachWatchdog()
}

// TestMouse_ClickTaskRowFocusesAndSelects: a task-row click focuses the
// automations strip and selects that task; a double click opens its editor.
func TestMouse_ClickTaskRowFocusesAndSelects(t *testing.T) {
	h := mouseTestHome(t)
	clock := newFakeClock(h)

	clickZone(t, h, zones.AutoTask("task-bbb"))
	assert.Equal(t, layout.RegionAutomations, h.ring.Active(), "the click focuses the strip")
	sp := h.automations.TaskPane()
	sel, ok := sp.SelectedTask()
	require.True(t, ok)
	assert.Equal(t, "task-bbb", sel.ID, "the click selects the row's task")
	assert.False(t, sp.IsEditing(), "a single click must not open the editor")

	// Double click (on the expanded manager's row this time): open the editor.
	clock.advance(time.Second)
	clickZone(t, h, zones.AutoTask("task-bbb"))
	clock.advance(50 * time.Millisecond)
	clickZone(t, h, zones.AutoTask("task-bbb"))
	assert.True(t, sp.IsEditing(), "double-click opens the task editor")
}

// TestMouse_ClickStripBackgroundFocusesAutomations: clicking the strip
// anywhere off a task row focuses (and expands) the automations region.
func TestMouse_ClickStripBackgroundFocusesAutomations(t *testing.T) {
	h := mouseTestHome(t)
	newFakeClock(h)

	clickZone(t, h, zones.AutoStrip) // top-left: the title line, no task row
	assert.Equal(t, layout.RegionAutomations, h.ring.Active())
	assert.True(t, h.lastLayout.AutomationsExpanded, "focusing the strip expands it in place")
}

// TestMouse_ClickStatusHintRunsAction: clicking a status-bar hint presses its
// key through the full handleKeyPress path. With the automations strip
// focused the bar advertises "tab focus"; the click must cycle the ring
// exactly like pressing Tab.
func TestMouse_ClickStatusHintRunsAction(t *testing.T) {
	h := mouseTestHome(t)
	newFakeClock(h)
	h.focusRegion(layout.RegionAutomations)

	clickZone(t, h, zones.StatusHint("tab"))
	assert.Equal(t, layout.RegionTree, h.ring.Active(),
		"clicking the 'tab focus' hint cycles the focus ring, like the key")
}

// TestMouse_WheelScrollsRegionUnderCursor: the wheel drives whatever region
// is under the cursor — tree cursor movement, pane capture scroll, task
// selection — regardless of where the focus ring points (before this PR it
// always scrolled the content pane).
func TestMouse_WheelScrollsRegionUnderCursor(t *testing.T) {
	h := mouseTestHome(t)
	newFakeClock(h)
	require.False(t, h.sidebar.GetSelection().IsTab)

	// Over the tree: the cursor walks down (alpha → its first tab row).
	tree := zoneRect(t, h, zones.TreeBG)
	wheel(h, tree.X+1, tree.Y+3, false)
	assert.True(t, h.sidebar.GetSelection().IsTab,
		"wheel over the tree moves the tree cursor, like j/k")

	// Over the automations strip: the task selection moves — even though the
	// strip is NOT focused — and the tree cursor stays put.
	preSel := h.sidebar.GetSelection()
	strip := zoneRect(t, h, zones.AutoStrip)
	wheel(h, strip.X+1, strip.Y, false)
	sel, ok := h.automations.TaskPane().SelectedTask()
	require.True(t, ok)
	assert.Equal(t, "task-bbb", sel.ID, "wheel over the strip moves the task selection")
	assert.Equal(t, preSel, h.sidebar.GetSelection(), "the tree cursor must not move")
	assert.Equal(t, layout.RegionTree, h.ring.Active(), "wheel never moves focus")

	// Over pane A's body: the pane enters scroll mode (the capture is the
	// hermetic fake backend's).
	body := zoneRect(t, h, zones.PaneBody(layout.RegionPaneA))
	wheel(h, body.X+2, body.Y+4, true)
	assert.True(t, h.paneA.IsInScrollMode(), "wheel over the pane scrolls the pane")
	assert.Equal(t, "task-bbb", mustSelectedTaskID(t, h),
		"pane scroll must not leak into the task selection")
}

func mustSelectedTaskID(t *testing.T, h *home) string {
	t.Helper()
	sel, ok := h.automations.TaskPane().SelectedTask()
	require.True(t, ok)
	return sel.ID
}

// TestMouse_ConfirmOverlayClicks: the kill confirmation's y/n words act as
// their keys — n cancels, y confirms (row flips to Deleting) — and while the
// modal is up, clicks on background zones are swallowed.
func TestMouse_ConfirmOverlayClicks(t *testing.T) {
	h := mouseTestHome(t)
	clock := newFakeClock(h)
	alpha := h.store.GetInstanceByTitle("alpha")

	// Open the kill dialog and click "n or esc to cancel".
	_, _ = h.handleKill()
	require.Equal(t, stateConfirm, h.state)

	// Background zones are registered but must be inert behind the modal.
	clickZone(t, h, zones.TreeInstance("beta"))
	assert.Equal(t, stateConfirm, h.state, "a background click must not close the modal")
	assert.Equal(t, "alpha", h.store.GetSelectedInstance().Title,
		"a background click must not move the selection behind the modal")

	clock.advance(time.Second)
	clickZone(t, h, zones.OverlayConfirmNo)
	assert.Equal(t, stateDefault, h.state, "clicking n cancels the dialog")
	assert.NotEqual(t, session.Deleting, alpha.GetStatus(), "cancel must not kill")

	// Re-open and click "y to confirm".
	clock.advance(time.Second)
	_, _ = h.handleKill()
	require.Equal(t, stateConfirm, h.state)
	cmd := clickZone(t, h, zones.OverlayConfirmYes)
	assert.Equal(t, stateDefault, h.state)
	assert.Equal(t, session.Deleting, alpha.GetStatus(),
		"clicking y runs the confirm action, like pressing y")
	assert.NotNil(t, cmd, "the confirm action's startKillMsg must be forwarded")
}

// TestMouse_SelectionOverlayRowClick: clicking a program row selects and
// submits it, like ↓ + enter.
func TestMouse_SelectionOverlayRowClick(t *testing.T) {
	h := mouseTestHome(t)
	newFakeClock(h)
	items := []string{"claude", "aider", "codex"}
	h.selectionOverlay = overlay.NewSelectionOverlay("Select Program", items)
	h.selectionOverlay.SetWidth(60)
	h.state = stateSelectProgram

	clickZone(t, h, zones.OverlaySelectRow(2))
	assert.Equal(t, stateNew, h.state, "a row click submits the selection")
}

// TestMouse_SearchOverlayRowClick: clicking a search result selects it and
// closes the overlay, like ↓ + enter.
func TestMouse_SearchOverlayRowClick(t *testing.T) {
	h := mouseTestHome(t)
	newFakeClock(h)
	_, _ = h.showSearchOverlay()
	require.Equal(t, stateSearch, h.state)

	clickZone(t, h, zones.OverlaySearchRow(1))
	assert.Equal(t, stateDefault, h.state, "a row click submits and closes the search")
	assert.Equal(t, "beta", h.sidebar.GetSelectedInstance().Title,
		"the clicked result becomes the selection")
}

// TestMouse_MissAndFallbackAreInert: presses outside every zone, non-left
// buttons, and releases must all no-op; below the hard minimum there are no
// zones at all.
func TestMouse_MissAndFallbackAreInert(t *testing.T) {
	h := mouseTestHome(t)
	newFakeClock(h)
	_ = h.View()

	// Motion / release / right-button events are ignored.
	_, cmd := h.handleMouse(tea.MouseMsg{X: 1, Y: 1, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
	assert.Nil(t, cmd)
	_, cmd = h.handleMouse(tea.MouseMsg{X: 1, Y: 1, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	assert.Nil(t, cmd)
	_, cmd = h.handleMouse(tea.MouseMsg{X: 1, Y: 1, Action: tea.MouseActionPress, Button: tea.MouseButtonRight})
	assert.Nil(t, cmd)

	// A press past the frame edge resolves nothing and changes nothing.
	before := h.ring.Active()
	press(h, h.termWidth+5, h.termHeight+5)
	assert.Equal(t, before, h.ring.Active())

	// Below the hard minimum the fallback banner renders and registers nothing.
	resizeHome(h, layout.HardMinWidth-1, layout.HardMinHeight-1)
	_ = h.View()
	assert.Empty(t, h.zones.IDs(), "the fallback banner has no clickable zones")
	press(h, 1, 1) // must not panic
}

// TestMouse_ZoneInventoryAtFullLayout pins the expected zone families all
// registering together in a split layout with tasks — the "each pane
// registers the expected zones" contract at the frame level.
func TestMouse_ZoneInventoryAtFullLayout(t *testing.T) {
	h := mouseTestHome(t)
	newFakeClock(h)
	pressKey(t, h, "s")
	require.True(t, h.lastLayout.SplitActive)
	_ = h.View()

	for _, id := range []string{
		zones.TreeBG,
		zones.TreeHeader,
		zones.TreeInstance("alpha"),
		zones.TreeInstance("beta"),
		zones.TreeArrow("alpha"),
		zones.TreeTab("alpha", 0),
		zones.PaneBody(layout.RegionPaneA),
		zones.PaneHeader(layout.RegionPaneA),
		zones.PaneBody(layout.RegionPaneB),
		zones.PaneHeader(layout.RegionPaneB),
		zones.AutoStrip,
		zones.AutoTask("task-aaa"),
		zones.StatusHint("q"),
	} {
		_, ok := h.zones.Find(id)
		assert.True(t, ok, "expected zone %q in the full-layout frame; got %v", id, h.zones.IDs())
	}
}

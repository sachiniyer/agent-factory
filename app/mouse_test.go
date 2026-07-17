package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// ----------------------------------------------------------------------------
// Root mouse routing (#1024 R4, closes #1025): the RFC §2.5 gesture table,
// driven hermetically — click coordinates are derived FROM the zone registry
// the last View() rebuilt (never hardcoded), so a layout change moves the
// tests with it or fails loudly.
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

// mouseTestHome is a home with two started (mock tmux) instances and two
// tasks at a real layout size, selection on the first instance.
func mouseTestHome(t *testing.T) (*home, *session.Instance, *session.Instance) {
	t.Helper()
	h := newTestHome(t)
	alpha := startedLocalInstance(t, "alpha")
	beta := startedLocalInstance(t, "beta")
	h.store.AddInstance(alpha)
	h.store.AddInstance(beta)
	tasks := []task.Task{
		{ID: "task-aaa", Name: "nightly-sweep", CronExpr: "0 3 * * *", Enabled: true},
		{ID: "task-bbb", Name: "log-watch", WatchCmd: "tail -f x", Enabled: true},
	}
	h.store.SetTasks(tasks)
	h.automations.TaskPane().SetTasks(tasks)
	h.sidebar.SetSelectedInstance(0)
	resizeHome(h, 140, 40)
	return h, alpha, beta
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

// release injects a left release at (x, y) through the root mouse router.
func release(h *home, x, y int) tea.Cmd {
	_, cmd := h.handleMouse(tea.MouseMsg{
		X: x, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft,
	})
	return cmd
}

// motion injects left-button motion at (x, y).
func motion(h *home, x, y int) tea.Cmd {
	_, cmd := h.handleMouse(tea.MouseMsg{
		X: x, Y: y, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft,
	})
	return cmd
}

func combineCmds(a, b tea.Cmd) tea.Cmd {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	default:
		return tea.Batch(a, b)
	}
}

// clickZone renders, resolves id's rect, and clicks its top-left cell.
func clickZone(t *testing.T, h *home, id string) tea.Cmd {
	t.Helper()
	r := zoneRect(t, h, id)
	return combineCmds(press(h, r.X, r.Y), release(h, r.X, r.Y))
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
// (retargeting the workspace binding) and focuses the tree, from wherever
// focus was.
func TestMouse_ClickInstanceRowSelects(t *testing.T) {
	h, _, beta := mouseTestHome(t)
	newFakeClock(h)
	h.focusRegion(layout.RegionAutomations)

	clickZone(t, h, zones.TreeInstance(beta.Title))

	require.NotNil(t, h.store.GetSelectedInstance())
	assert.Equal(t, beta.Title, h.store.GetSelectedInstance().Title,
		"clicking an instance row retargets the selection")
	assert.Equal(t, layout.RegionTree, h.ring.Active(), "the click focuses the tree")
}

// TestMouse_ClickTreeTabRowSelectsTab: clicking a tab child row drives the
// store's active tab, exactly like landing the tree cursor on it.
func TestMouse_ClickTreeTabRowSelectsTab(t *testing.T) {
	h, alpha, _ := mouseTestHome(t)
	newFakeClock(h)

	clickZone(t, h, zones.TreeTab(alpha.Title, 1))

	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab, "the cursor lands on the tab row")
	assert.Equal(t, 1, sel.TabIndex)
	assert.Equal(t, 1, h.store.ActiveTab(), "the click drives the active tab")
}

func TestMouse_DoubleClickTreeTabRowEntersInteractive(t *testing.T) {
	h, alpha, _ := mouseTestHome(t)
	clock := newFakeClock(h)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	fakes, _ := stubLiveTermFactory(t)

	clickZone(t, h, zones.TreeTab(alpha.Title, 1))
	clock.advance(50 * time.Millisecond)
	clickZone(t, h, zones.TreeTab(alpha.Title, 1))

	p := h.store.FindOpenPane(alpha, 1)
	require.NotNil(t, p, "double-clicking a tab row must enter that tab's pane target")
	assert.Equal(t, p, h.focusedOpenPane())
	_, cmd := h.Update(enterInteractiveMsg{pane: p})
	runHermeticCmd(t, h, cmd, 0)
	assert.True(t, h.interactive, "tab-row double-click still replays the Enter path")
	require.Len(t, *fakes, 1)
}

func TestMouse_SubThresholdTabJitterSelectsPressedTab(t *testing.T) {
	h, alpha, _ := mouseTestHome(t)
	newFakeClock(h)
	require.Equal(t, 0, h.store.ActiveTab())

	pressTab := zoneRect(t, h, zones.TreeTab(alpha.Title, 1))
	neighbor := zoneRect(t, h, zones.TreeTab(alpha.Title, 0))
	require.Less(t,
		manhattanDistance(pressTab.X, pressTab.Y, neighbor.X, neighbor.Y),
		tabDragThresholdCells,
		"test must release below the drag promotion threshold")

	press(h, pressTab.X, pressTab.Y)
	motion(h, neighbor.X, neighbor.Y)
	require.NotNil(t, h.tabDrag)
	require.False(t, h.tabDrag.active, "one-cell neighboring-row jitter is still a click")
	release(h, neighbor.X, neighbor.Y)

	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex, "sub-threshold jitter selects the press-origin tab")
	assert.Equal(t, 1, h.store.ActiveTab())
}

func TestMouse_DragTreeTabToPaneOpensSplitAppend(t *testing.T) {
	h, alpha, _ := mouseTestHome(t)
	newFakeClock(h)
	paneAgent := openTestPane(t, h, alpha, 0)
	regionAgent := layout.PaneRegion(paneAgent.ID())
	require.Equal(t, 1, h.store.NumOpenPanes())

	tab := zoneRect(t, h, zones.TreeTab(alpha.Title, 1))
	body := zoneRect(t, h, zones.PaneBody(regionAgent))

	press(h, tab.X, tab.Y)
	motion(h, body.X+3, body.Y+4)
	require.NotNil(t, h.tabDrag)
	assert.True(t, h.tabDrag.active, ">=2-cell motion promotes the candidate to a drag")
	assert.Equal(t, regionAgent, h.tabDragDropTargetRegion(), "pane under cursor is the drop target")
	assert.Contains(t, h.View(), "Dragging", "active drag renders a status affordance")

	release(h, body.X+3, body.Y+4)

	require.Nil(t, h.tabDrag)
	require.Equal(t, 2, h.store.NumOpenPanes(), "drop opens a split pane")
	panes := h.store.OpenPanes()
	require.Len(t, panes, 2)
	assert.Same(t, paneAgent, panes[0], "drop uses s/S append order, not adjacent replacement")
	paneTerminal := panes[1]
	assert.Same(t, alpha, paneTerminal.Instance())
	assert.Equal(t, 1, paneTerminal.Tab())
	assert.Equal(t, layout.PaneRegion(paneTerminal.ID()), h.ring.Active(), "the dropped tab's pane takes focus")
	assert.Equal(t, 1, h.store.ActiveTab(), "drop selects the dragged tab in the sidebar")
	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex)
}

func TestMouse_DragDropReresolvesInstanceAfterProjectionSwap(t *testing.T) {
	h, alpha, _ := mouseTestHome(t)
	newFakeClock(h)
	paneAgent := openTestPane(t, h, alpha, 0)
	regionAgent := layout.PaneRegion(paneAgent.ID())

	tab := zoneRect(t, h, zones.TreeTab(alpha.Title, 1))
	body := zoneRect(t, h, zones.PaneBody(regionAgent))
	press(h, tab.X, tab.Y)
	motion(h, body.X+3, body.Y+4)
	require.NotNil(t, h.tabDrag)
	require.True(t, h.tabDrag.active)

	rebuilt := instanceWithFakeBackend(t, alpha.Title)
	rebuilt.AddTabForTest("agent", session.TabKindAgent)
	rebuilt.AddTabForTest("shell", session.TabKindShell)
	require.True(t, h.store.ReplaceInstanceByTitle(alpha.Title, rebuilt))
	require.False(t, h.store.ContainsInstance(alpha), "the press-time pointer is now orphaned")
	require.Same(t, rebuilt, h.store.GetInstanceByTitle(alpha.Title))
	require.Same(t, rebuilt, paneAgent.Instance(), "existing open panes follow the projection swap")

	release(h, body.X+3, body.Y+4)

	require.Equal(t, 2, h.store.NumOpenPanes())
	dropped := h.store.FindOpenPane(rebuilt, 1)
	require.NotNil(t, dropped, "drop must bind to the current same-title instance")
	assert.Nil(t, h.store.FindOpenPane(alpha, 1), "drop must not bind an orphaned press-time pointer")
	assert.Same(t, rebuilt, dropped.Instance())
	assert.Equal(t, layout.PaneRegion(dropped.ID()), h.ring.Active())
}

func TestMouse_DragTreeTabOutsidePaneCancels(t *testing.T) {
	h, alpha, _ := mouseTestHome(t)
	newFakeClock(h)
	openTestPane(t, h, alpha, 0)
	require.False(t, h.sidebar.GetSelection().IsTab)
	require.Equal(t, 0, h.store.ActiveTab())

	tab := zoneRect(t, h, zones.TreeTab(alpha.Title, 1))
	auto := zoneRect(t, h, zones.AutoBG)

	press(h, tab.X, tab.Y)
	motion(h, auto.X+1, auto.Y)
	require.NotNil(t, h.tabDrag)
	require.True(t, h.tabDrag.active)
	assert.Empty(t, h.tabDragDropTargetRegion(), "non-pane regions are not valid drop targets")

	release(h, auto.X+1, auto.Y)

	assert.Nil(t, h.tabDrag)
	assert.Equal(t, 1, h.store.NumOpenPanes(), "invalid drop must not open a pane")
	assert.Equal(t, 0, h.store.ActiveTab(), "invalid drop must not select the dragged tab")
	assert.False(t, h.sidebar.GetSelection().IsTab, "invalid drop is a no-op for selection")
	assert.Nil(t, h.panePreviewTxn, "invalid drop must not create a preview")
}

func TestMouse_DragTerminationsResetDoubleClickTracker(t *testing.T) {
	cases := []struct {
		name      string
		terminate func(t *testing.T, h *home, alpha *session.Instance, tab layout.Rect)
		wantPanes int
	}{
		{
			name: "active invalid drop cancel",
			terminate: func(t *testing.T, h *home, _ *session.Instance, tab layout.Rect) {
				t.Helper()
				auto := zoneRect(t, h, zones.AutoBG)
				press(h, tab.X, tab.Y)
				motion(h, auto.X+1, auto.Y)
				require.NotNil(t, h.tabDrag)
				require.True(t, h.tabDrag.active)
				release(h, auto.X+1, auto.Y)
			},
			wantPanes: 0,
		},
		{
			name: "valid drop complete",
			terminate: func(t *testing.T, h *home, alpha *session.Instance, tab layout.Rect) {
				t.Helper()
				paneAgent := openTestPane(t, h, alpha, 0)
				body := zoneRect(t, h, zones.PaneBody(layout.PaneRegion(paneAgent.ID())))
				press(h, tab.X, tab.Y)
				motion(h, body.X+3, body.Y+4)
				require.NotNil(t, h.tabDrag)
				require.True(t, h.tabDrag.active)
				release(h, body.X+3, body.Y+4)
			},
			wantPanes: 2,
		},
		{
			name: "inactive wheel cancel",
			terminate: func(t *testing.T, h *home, _ *session.Instance, tab layout.Rect) {
				t.Helper()
				auto := zoneRect(t, h, zones.AutoBG)
				press(h, tab.X, tab.Y)
				require.NotNil(t, h.tabDrag)
				require.False(t, h.tabDrag.active)
				wheel(h, auto.X+1, auto.Y, false)
			},
			wantPanes: 0,
		},
		{
			name: "inactive other-button cancel",
			terminate: func(t *testing.T, h *home, _ *session.Instance, tab layout.Rect) {
				t.Helper()
				press(h, tab.X, tab.Y)
				require.NotNil(t, h.tabDrag)
				require.False(t, h.tabDrag.active)
				_, _ = h.handleMouse(tea.MouseMsg{
					X: tab.X, Y: tab.Y, Action: tea.MouseActionPress, Button: tea.MouseButtonRight,
				})
			},
			wantPanes: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, alpha, _ := mouseTestHome(t)
			newFakeClock(h)
			require.Zero(t, h.store.NumOpenPanes())

			clickZone(t, h, zones.TreeTab(alpha.Title, 1))
			require.Equal(t, zones.TreeTab(alpha.Title, 1), h.lastClickZone,
				"precondition: first click seeds tracker")

			tab := zoneRect(t, h, zones.TreeTab(alpha.Title, 1))
			tc.terminate(t, h, alpha, tab)
			require.Nil(t, h.tabDrag)
			require.Empty(t, h.lastClickZone, "drag termination clears stale click state")
			require.Equal(t, tc.wantPanes, h.store.NumOpenPanes())
			require.Equal(t, stateDefault, h.state)

			clickZone(t, h, zones.TreeTab(alpha.Title, 1))

			assert.Equal(t, stateDefault, h.state,
				"single click after drag termination must not reuse stale double-click state and enter")
			assert.Equal(t, tc.wantPanes, h.store.NumOpenPanes())
			assert.False(t, h.interactive)
			sel := h.sidebar.GetSelection()
			require.True(t, sel.IsTab)
			assert.Equal(t, 1, sel.TabIndex)
		})
	}
}

func TestMouse_DragAlreadyOpenTabFocusesExistingPane(t *testing.T) {
	h, alpha, _ := mouseTestHome(t)
	newFakeClock(h)
	paneAgent := openTestPane(t, h, alpha, 0)
	paneTerminal := openTestPane(t, h, alpha, 1)
	h.focusRegion(layout.PaneRegion(paneAgent.ID()))
	require.Equal(t, 2, h.store.NumOpenPanes())

	tab := zoneRect(t, h, zones.TreeTab(alpha.Title, 1))
	body := zoneRect(t, h, zones.PaneBody(layout.PaneRegion(paneAgent.ID())))

	press(h, tab.X, tab.Y)
	motion(h, body.X+3, body.Y+4)
	release(h, body.X+3, body.Y+4)

	assert.Equal(t, 2, h.store.NumOpenPanes(), "already-open dragged tab must not duplicate panes")
	assert.Same(t, paneTerminal, h.store.FindOpenPane(alpha, 1))
	assert.Equal(t, layout.PaneRegion(paneTerminal.ID()), h.ring.Active(),
		"drop reuses the existing #1493 focus path")
}

func TestMouse_TabDragThresholdAndWheelCancel(t *testing.T) {
	h, alpha, _ := mouseTestHome(t)
	newFakeClock(h)
	openTestPane(t, h, alpha, 0)

	tab := zoneRect(t, h, zones.TreeTab(alpha.Title, 1))

	press(h, tab.X, tab.Y)
	motion(h, tab.X+1, tab.Y)
	require.NotNil(t, h.tabDrag)
	assert.False(t, h.tabDrag.active, "one-cell jitter stays a click candidate")

	wheel(h, tab.X, tab.Y, false)

	assert.Nil(t, h.tabDrag, "wheel cancels a pending drag candidate")
	assert.Equal(t, 0, h.store.ActiveTab(), "wheel-cancelled drag must not replay the dragged tab click")
}

// TestMouse_ClickArrowTogglesExpansion: the ▸/▾ arrow collapses/expands the
// instance's tab children without entering it or moving the selection off it.
func TestMouse_ClickArrowTogglesExpansion(t *testing.T) {
	h, alpha, beta := mouseTestHome(t)
	clock := newFakeClock(h)

	// alpha is selected and expanded: its tab rows have zones.
	_ = zoneRect(t, h, zones.TreeTab(alpha.Title, 0))

	clickZone(t, h, zones.TreeArrow(alpha.Title))
	_ = h.View()
	_, ok := h.zones.Find(zones.TreeTab(alpha.Title, 0))
	assert.False(t, ok, "arrow click collapses the expanded selection")

	clock.advance(time.Second) // not a double click
	clickZone(t, h, zones.TreeArrow(alpha.Title))
	_ = h.View()
	_, ok = h.zones.Find(zones.TreeTab(alpha.Title, 0))
	assert.True(t, ok, "second arrow click re-expands")

	// Arrow on a collapsed non-selected instance selects it, which
	// auto-expands its subtree.
	clock.advance(time.Second)
	clickZone(t, h, zones.TreeArrow(beta.Title))
	assert.Equal(t, beta.Title, h.store.GetSelectedInstance().Title)
	_ = h.View()
	_, ok = h.zones.Find(zones.TreeTab(beta.Title, 0))
	assert.True(t, ok, "selecting via the arrow expands the new selection")
}

// TestMouse_DoubleClickTreeRowEntersInteractive: two quick clicks on a tree
// row take the exact Enter path — open (or focus) the selection's pane, then
// enter interactive mode; a slow second click stays a plain re-select.
func TestMouse_DoubleClickTreeRowEntersInteractive(t *testing.T) {
	h, _, beta := mouseTestHome(t)
	clock := newFakeClock(h)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	fakes, _ := stubLiveTermFactory(t)

	// Slow clicks: select only — no pane opens.
	clickZone(t, h, zones.TreeInstance(beta.Title))
	clock.advance(time.Second)
	clickZone(t, h, zones.TreeInstance(beta.Title))
	assert.Zero(t, h.store.NumOpenPanes(), "clicks slower than the double-click interval must not enter")

	// Fast second click: the Enter path — pane opens focused; the deferred
	// activation arrives as enterInteractiveMsg (driven directly: the batched
	// cmd also carries selectionChanged's capture dispatches, which are not
	// hermetic — the TestEnterFromTreeOpensPaneAndEntersInteractive pattern).
	clock.advance(time.Second)
	clickZone(t, h, zones.TreeInstance(beta.Title))
	clock.advance(50 * time.Millisecond)
	clickZone(t, h, zones.TreeInstance(beta.Title))
	p := h.store.FindOpenPane(beta, 0)
	require.NotNil(t, p, "double-click must open the selection's pane, like Enter")
	assert.Equal(t, p, h.focusedOpenPane(), "the opened pane takes focus")
	_, cmd := h.Update(enterInteractiveMsg{pane: p})
	runHermeticCmd(t, h, cmd, 0)
	assert.True(t, h.interactive, "double-click enters interactive mode through the Enter seam")
	require.Len(t, *fakes, 1)
}

// TestMouse_PaneClicksFocusThenInteract: a header click focuses the pane; a
// body click on an unfocused pane focuses it; a click on the already-focused
// pane's body enters it (the §2.5 "click a focused pane body → interactive"
// row).
func TestMouse_PaneClicksFocusThenInteract(t *testing.T) {
	h, alpha, beta := mouseTestHome(t)
	clock := newFakeClock(h)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	_, _ = stubLiveTermFactory(t)
	pa := openTestPane(t, h, alpha, 0)
	pb := openTestPane(t, h, beta, 0) // focused last
	regionA := layout.PaneRegion(pa.ID())
	regionB := layout.PaneRegion(pb.ID())
	require.Equal(t, regionB, h.ring.Active())

	// Header click picks pane A out of the split.
	clickZone(t, h, zones.PaneHeader(regionA))
	assert.Equal(t, regionA, h.ring.Active(), "pane A header click focuses pane A")

	// Body click on the unfocused pane B focuses it — and only focuses.
	clock.advance(time.Second)
	body := zoneRect(t, h, zones.PaneBody(regionB))
	press(h, body.X+2, body.Y+4) // below the header row
	assert.Equal(t, regionB, h.ring.Active(), "body click on an unfocused pane focuses it")
	assert.False(t, h.interactive, "the focusing click must not enter the pane")

	// A later click on the focused pane's body enters it, like Enter.
	clock.advance(time.Second)
	body = zoneRect(t, h, zones.PaneBody(regionB))
	press(h, body.X+2, body.Y+4)
	p := h.focusedOpenPane()
	require.Equal(t, pb, p)
	_, cmd := h.Update(enterInteractiveMsg{pane: p})
	runHermeticCmd(t, h, cmd, 0)
	assert.True(t, h.interactive, "click on the focused pane body enters interactive mode")
}

// TestMouse_ClickTaskRowFocusesAndSelects: a task-row click focuses the rail's
// automations section and selects that task; a double click opens the
// task-manager overlay on it (§2.5).
func TestMouse_ClickTaskRowFocusesAndSelects(t *testing.T) {
	h, _, _ := mouseTestHome(t)
	clock := newFakeClock(h)

	clickZone(t, h, zones.AutoTask("task-bbb"))
	assert.Equal(t, layout.RegionAutomations, h.ring.Active(), "the click focuses the section")
	assert.Equal(t, 1, h.automations.SelectedTaskIndex(), "the click selects the row's task")
	assert.Equal(t, stateDefault, h.state, "a single click must not open the manager")

	clock.advance(time.Second)
	clickZone(t, h, zones.AutoTask("task-bbb"))
	clock.advance(50 * time.Millisecond)
	clickZone(t, h, zones.AutoTask("task-bbb"))
	assert.Equal(t, stateTasks, h.state, "double-click opens the task-manager overlay")
	sel, ok := selectedManagerTask(h)
	require.True(t, ok)
	assert.Equal(t, "task-bbb", sel, "the manager opens preselecting the clicked task")
}

func selectedManagerTask(h *home) (string, bool) {
	sp := h.automations.TaskPane()
	tasks := sp.GetTasks()
	idx := h.automations.SelectedTaskIndex()
	if idx < 0 || idx >= len(tasks) {
		return "", false
	}
	return tasks[idx].ID, true
}

// TestMouse_ClickSectionBackgroundFocusesAutomations: clicking the section
// anywhere off a task row (its title line) just focuses it.
func TestMouse_ClickSectionBackgroundFocusesAutomations(t *testing.T) {
	h, _, _ := mouseTestHome(t)
	newFakeClock(h)

	clickZone(t, h, zones.AutoBG) // top-left: the title line, no task row
	assert.Equal(t, layout.RegionAutomations, h.ring.Active())
	assert.Equal(t, stateDefault, h.state)
}

// TestMouse_ClickStatusHintRunsAction: clicking a status-bar hint presses its
// key through the full handleKeyPress path. With the automations section
// focused the bar advertises "tab focus"; the click must cycle the ring
// exactly like pressing Tab.
func TestMouse_ClickStatusHintRunsAction(t *testing.T) {
	h, _, _ := mouseTestHome(t)
	newFakeClock(h)
	h.focusRegion(layout.RegionAutomations)

	clickZone(t, h, zones.StatusHint("tab"))
	assert.Equal(t, layout.RegionProjects, h.ring.Active(),
		"clicking the 'tab focus' hint cycles the focus ring, like the key (automations → projects)")
}

// TestMouse_WheelScrollsRegionUnderCursor: the wheel drives whatever region
// is under the cursor — tree cursor movement, pane capture scroll, task
// selection — regardless of where the focus ring points (before this PR it
// always scrolled the focused pane). Tree cursor movement also returns focus
// to the tree so the next keyboard action targets the row the wheel selected.
func TestMouse_WheelScrollsRegionUnderCursor(t *testing.T) {
	h, alpha, _ := mouseTestHome(t)
	newFakeClock(h)
	p := openTestPane(t, h, alpha, 0)
	region := layout.PaneRegion(p.ID())
	h.focusRegion(region)
	require.False(t, h.sidebar.GetSelection().IsTab)

	// Over the tree: the cursor walks down (alpha → its first tab row) and
	// focus follows that selection, matching keyboard tree navigation (#1418).
	tree := zoneRect(t, h, zones.TreeBG)
	wheel(h, tree.X+1, tree.Y+3, false)
	assert.True(t, h.sidebar.GetSelection().IsTab,
		"wheel over the tree moves the tree cursor, like j/k")
	assert.Equal(t, layout.RegionTree, h.ring.Active(),
		"wheel over the tree must focus the tree so Enter targets the selected row")

	// Over the automations section: the task cursor moves — even though the
	// section is NOT focused — and the tree cursor stays put.
	h.focusRegion(region)
	preSel := h.sidebar.GetSelection()
	require.Equal(t, 0, h.automations.SelectedTaskIndex())
	auto := zoneRect(t, h, zones.AutoBG)
	wheel(h, auto.X+1, auto.Y, false)
	assert.Equal(t, 1, h.automations.SelectedTaskIndex(),
		"wheel over the section moves the task cursor")
	assert.Equal(t, preSel, h.sidebar.GetSelection(), "the tree cursor must not move")
	assert.Equal(t, region, h.ring.Active(), "automation wheel scroll must not steal pane focus")

	// Over the pane's body: the pane enters capture scroll mode.
	body := zoneRect(t, h, zones.PaneBody(region))
	wheel(h, body.X+2, body.Y+4, true)
	assert.True(t, h.paneWindows[p.ID()].IsInScrollMode(),
		"wheel over the pane scrolls the pane")
	assert.Equal(t, 1, h.automations.SelectedTaskIndex(),
		"pane scroll must not leak into the task selection")
}

// TestMouse_ConfirmOverlayClicks: the kill confirmation's y/n words act as
// their keys — n cancels, y confirms (row flips to Deleting) — and while the
// modal is up, clicks on background zones are swallowed.
func TestMouse_ConfirmOverlayClicks(t *testing.T) {
	h, alpha, beta := mouseTestHome(t)
	clock := newFakeClock(h)

	// Open the kill dialog and click background, then "n or esc to cancel".
	_, _ = h.handleKill()
	require.Equal(t, stateConfirm, h.state)

	clickZone(t, h, zones.TreeInstance(beta.Title))
	assert.Equal(t, stateConfirm, h.state, "a background click must not close the modal")
	assert.Equal(t, alpha.Title, h.store.GetSelectedInstance().Title,
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

// TestMouse_ModalSwallowedClickNoFalseDouble: a click swallowed while a modal
// owns the screen must NOT seed the double-click tracker; otherwise a click on
// the same zone within the double-click window after the modal closes reads as
// a false double click and fires the row's enter action (#1731).
func TestMouse_ModalSwallowedClickNoFalseDouble(t *testing.T) {
	h, _, beta := mouseTestHome(t)
	clock := newFakeClock(h)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	_, _ = stubLiveTermFactory(t)

	// A confirmation modal owns the screen.
	_, _ = h.handleKill()
	require.Equal(t, stateConfirm, h.state)
	require.Empty(t, h.lastClickZone, "precondition: tracker starts clean")

	// Click a background instance row: swallowed by the modal, and it must
	// leave the double-click tracker untouched.
	clickZone(t, h, zones.TreeInstance(beta.Title))
	require.Equal(t, stateConfirm, h.state, "the background click stays swallowed")
	assert.Empty(t, h.lastClickZone,
		"a swallowed modal click must not seed the double-click tracker (#1731)")

	// Dismiss the modal via the keyboard, which does not re-touch the tracker.
	_, _ = h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state)

	// A single click on that same row within the double-click window must stay
	// a plain select — not a double-click enter into interactive mode.
	clock.advance(50 * time.Millisecond)
	clickZone(t, h, zones.TreeInstance(beta.Title))

	assert.Zero(t, h.store.NumOpenPanes(),
		"a swallowed modal click must not make the next click a false double (#1731)")
	assert.False(t, h.interactive, "the post-modal single click must not enter interactive mode")
	assert.Equal(t, stateDefault, h.state)
}

// TestMouse_StaleClickTrackerClearedAcrossModal: a pre-modal click seeds the
// double-click tracker; a modal that opens and closes (driven through Update,
// the real keyboard path) must invalidate that seed so a fast click on the same
// row afterwards stays a single click — not a false double-click enter (#1731).
func TestMouse_StaleClickTrackerClearedAcrossModal(t *testing.T) {
	h, _, beta := mouseTestHome(t)
	clock := newFakeClock(h)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInteractive{}.mask()))
	_, _ = stubLiveTermFactory(t)

	// A real click on beta's row seeds the tracker (and selects beta).
	clickZone(t, h, zones.TreeInstance(beta.Title))
	require.Equal(t, zones.TreeInstance(beta.Title), h.lastClickZone,
		"precondition: the click seeds the double-click tracker")
	require.Equal(t, beta.Title, h.store.GetSelectedInstance().Title)

	// Open the kill confirmation through Update — 'D' first highlights the menu
	// hint and re-emits itself, so it takes two dispatches to reach handleKill.
	_, _ = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	_, _ = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	require.Equal(t, stateConfirm, h.state, "D opens the kill confirmation")
	require.Empty(t, h.lastClickZone,
		"a modal excursion must clear the stale pre-modal click tracker (#1731)")

	// Cancel the confirmation, back to the default state.
	_, _ = h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateDefault, h.state, "esc cancels the confirmation")

	// A fast click on that same row within the double-click window must stay a
	// single select — the pre-modal press must not pair with it.
	clock.advance(50 * time.Millisecond)
	clickZone(t, h, zones.TreeInstance(beta.Title))

	assert.Zero(t, h.store.NumOpenPanes(),
		"a pre-modal click must not survive a modal into a false double (#1731)")
	assert.False(t, h.interactive, "the post-modal single click must not enter interactive mode")
	assert.Equal(t, stateDefault, h.state)
}

// TestMouse_SelectionOverlayRowClick: clicking a program row selects and
// submits it, like ↓ + enter.
func TestMouse_SelectionOverlayRowClick(t *testing.T) {
	h, _, _ := mouseTestHome(t)
	newFakeClock(h)
	// The submit handler maps the row index into tmux.SupportedPrograms, so
	// the overlay must carry the real list (as handleStateNew builds it).
	h.selectionOverlay = overlay.NewSelectionOverlay("Select Program", tmux.SupportedPrograms)
	h.selectionOverlay.SetWidth(60)
	h.state = stateSelectProgram

	clickZone(t, h, zones.OverlaySelectRow(2))
	assert.Equal(t, stateNew, h.state, "a row click submits the selection")
	assert.Equal(t, tmux.SupportedPrograms[2], h.pendingProgram,
		"the clicked row's program is chosen")
}

// TestMouse_SearchOverlayRowClick: clicking a search result selects it and
// closes the overlay, like ↓ + enter.
func TestMouse_SearchOverlayRowClick(t *testing.T) {
	h, _, beta := mouseTestHome(t)
	newFakeClock(h)
	_, _ = h.showSearchOverlay()
	require.Equal(t, stateSearch, h.state)

	clickZone(t, h, zones.OverlaySearchRow(1))
	assert.Equal(t, stateDefault, h.state, "a row click submits and closes the search")
	assert.Equal(t, beta.Title, h.sidebar.GetSelectedInstance().Title,
		"the clicked result becomes the selection")
}

// TestMouse_MissAndFallbackAreInert: presses outside every zone, non-left
// buttons, and releases must all no-op; below the hard minimum there are no
// zones at all.
func TestMouse_MissAndFallbackAreInert(t *testing.T) {
	h, _, _ := mouseTestHome(t)
	newFakeClock(h)
	_ = h.View()

	// Motion / release / right-button events are ignored in nav mode.
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
// registering together in a two-pane layout with tasks — the "each pane
// registers the expected zones" contract at the frame level.
func TestMouse_ZoneInventoryAtFullLayout(t *testing.T) {
	h, alpha, beta := mouseTestHome(t)
	newFakeClock(h)
	pa := openTestPane(t, h, alpha, 0)
	pb := openTestPane(t, h, beta, 0)
	_ = h.View()

	for _, id := range []string{
		zones.TreeBG,
		zones.TreeHeader,
		zones.TreeInstance(alpha.Title),
		zones.TreeInstance(beta.Title),
		zones.TreeArrow(alpha.Title),
		zones.PaneBody(layout.PaneRegion(pa.ID())),
		zones.PaneHeader(layout.PaneRegion(pa.ID())),
		zones.PaneBody(layout.PaneRegion(pb.ID())),
		zones.PaneHeader(layout.PaneRegion(pb.ID())),
		zones.AutoBG,
		zones.AutoTask("task-aaa"),
		zones.StatusHint("q"),
	} {
		_, ok := h.zones.Find(id)
		assert.True(t, ok, "expected zone %q in the full-layout frame; got %v", id, h.zones.IDs())
	}
}

// ----------------------------------------------------------------------------
// Interactive mode × mouse (RFC §2.5 in-pane ownership): events over the live
// pane forward into the embedded terminal (grid) or are suppressed (frame/
// header); host gestures still apply outside the pane and drop the mode when
// they move focus.
// ----------------------------------------------------------------------------

// interactiveMouseHome enters interactive mode on a live pane and returns the
// bound fake attachment.
func interactiveMouseHome(t *testing.T) (*home, *fakeLiveTerm, string) {
	t.Helper()
	h, _, fakes := interactiveTestHome(t)
	enterInteractive(t, h)
	require.True(t, h.interactive)
	require.Len(t, *fakes, 1)
	region := layout.PaneRegion(h.focusedOpenPane().ID())
	newFakeClock(h)
	return h, (*fakes)[0], region
}

// TestMouse_InteractiveForwardsGridEvents: press/wheel/release over the live
// pane's terminal grid forward through SendMouse with GRID-LOCAL coordinates
// (the zone resolve's local point), and never trigger host actions.
func TestMouse_InteractiveForwardsGridEvents(t *testing.T) {
	h, fake, region := interactiveMouseHome(t)

	term := zoneRect(t, h, zones.PaneTerm(region))
	press(h, term.X+5, term.Y+3)
	require.Len(t, fake.mice, 1, "a grid press must forward to the attachment")
	assert.Equal(t, 5, fake.mice[0].x, "coordinates forward grid-local")
	assert.Equal(t, 3, fake.mice[0].y)
	assert.Equal(t, tea.MouseButtonLeft, fake.mice[0].msg.Button)
	assert.True(t, h.interactive, "a forwarded press must not leave the mode")

	// The wheel forwards ONLY when the inner app enabled mouse reporting (#1024
	// wheel fix). With tracking on it owns the wheel, exactly like a click.
	fake.mouseTracking = true
	wheel(h, term.X+5, term.Y+3, true)
	require.Len(t, fake.mice, 2, "with tracking enabled the inner app owns the wheel (§2.5)")
	assert.Equal(t, tea.MouseButtonWheelUp, fake.mice[1].msg.Button)
	assert.False(t, h.paneWindows[h.focusedOpenPane().ID()].IsInScrollMode(),
		"a forwarded wheel must not flip the live pane into host capture scroll")

	_, _ = h.handleMouse(tea.MouseMsg{
		X: term.X + 5, Y: term.Y + 3, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft,
	})
	require.Len(t, fake.mice, 3, "releases forward so the inner app sees complete clicks")
}

// TestMouse_InteractiveWheelWithoutTrackingScrollsScrollback: over the FOCUSED
// live pane, a wheel from a program that has NOT enabled mouse reporting must
// fall through to the host wheel handler and scroll the pane scrollback (tmux
// semantics) — the #1024 regression where the wheel was swallowed at a prompt.
func TestMouse_InteractiveWheelWithoutTrackingScrollsScrollback(t *testing.T) {
	h, fake, region := interactiveMouseHome(t)
	require.False(t, fake.mouseTracking, "the inner app owns no wheel until it enables tracking")

	term := zoneRect(t, h, zones.PaneTerm(region))
	wheel(h, term.X+5, term.Y+3, true)

	assert.Empty(t, fake.mice, "an untracked wheel must not forward into the inner app")
	assert.True(t, h.paneWindows[h.focusedOpenPane().ID()].IsInScrollMode(),
		"an untracked wheel scrolls the pane scrollback instead of being swallowed")
	assert.True(t, h.interactive, "scrolling scrollback must not leave interactive mode")

	// A click still forwards (the fix is scoped to the wheel), proving the pane is
	// still the interactive input target.
	press(h, term.X+5, term.Y+3)
	require.Len(t, fake.mice, 1, "clicks forward unchanged — only the wheel falls through")
	assert.Equal(t, tea.MouseButtonLeft, fake.mice[0].msg.Button)
}

func TestMouse_InteractiveTabDragConsumesTerminalMotionAndDrop(t *testing.T) {
	h, fake, region := interactiveMouseHome(t)
	inst := h.store.GetSelectedInstance()
	require.NotNil(t, inst)

	tab := zoneRect(t, h, zones.TreeTab(inst.Title, 1))
	term := zoneRect(t, h, zones.PaneTerm(region))

	press(h, tab.X, tab.Y)
	motion(h, term.X+5, term.Y+3)
	require.NotNil(t, h.tabDrag)
	require.True(t, h.tabDrag.active)
	assert.Equal(t, region, h.tabDragDropTargetRegion())

	release(h, term.X+5, term.Y+3)

	assert.Empty(t, fake.mice, "active tab drag must run before interactive terminal mouse forwarding")
	assert.False(t, h.interactive, "dropping onto a new pane moves focus and leaves interactive mode")
	p := h.store.FindOpenPane(inst, 1)
	require.NotNil(t, p, "drop over the live pane still opens the dragged tab")
	assert.Equal(t, layout.PaneRegion(p.ID()), h.ring.Active())
}

// TestMouse_InteractiveSuppressesPaneChrome: clicks on the live pane's header
// or frame are suppressed — no host focus/enter action, nothing forwarded
// (they are outside the grid).
func TestMouse_InteractiveSuppressesPaneChrome(t *testing.T) {
	h, fake, region := interactiveMouseHome(t)

	header := zoneRect(t, h, zones.PaneHeader(region))
	press(h, header.X+2, header.Y)
	assert.Empty(t, fake.mice, "chrome clicks must not forward")
	assert.True(t, h.interactive, "chrome clicks must not drop the mode")
	assert.Equal(t, region, h.ring.Active(), "chrome clicks must not move focus")
}

// TestMouse_InteractiveHostGesturesOutsidePane: outside the live pane the
// host gestures apply (§2.5) — a tree click selects, moves focus to the tree,
// and thereby ends interactive mode (the mode's premise is "keystrokes go to
// the FOCUSED pane").
func TestMouse_InteractiveHostGesturesOutsidePane(t *testing.T) {
	h, fake, _ := interactiveMouseHome(t)
	inst := h.store.GetSelectedInstance()
	require.NotNil(t, inst)

	clickZone(t, h, zones.TreeInstance(inst.Title))
	assert.Empty(t, fake.mice, "a tree click is a host gesture, not a forward")
	assert.Equal(t, layout.RegionTree, h.ring.Active(), "the click focuses the tree")
	assert.False(t, h.interactive, "focus moving off the pane ends interactive mode")
}

// TestMouse_InteractiveStatusHintExits: the interactive status bar advertises
// only ctrl+] — clicking it exits to nav mode through the normal key path.
func TestMouse_InteractiveStatusHintExits(t *testing.T) {
	h, _, _ := interactiveMouseHome(t)

	clickZone(t, h, zones.StatusHint("ctrl+]"))
	assert.False(t, h.interactive, "clicking the ctrl+] hint exits interactive mode")
}

// TestMouse_HintClickMatchesKeyGates: keyMsgFromString covers every primary
// key the menu can advertise, so a rendered hint is always clickable.
func TestMouse_HintClickMatchesKeyGates(t *testing.T) {
	for _, binding := range keys.GlobalKeyBindings {
		primary := binding.Keys()[0]
		if primary == "1" { // KeyJumpTab renders no zone by design
			continue
		}
		_, ok := keyMsgFromString(primary)
		assert.True(t, ok, "primary key %q must synthesize a tea.KeyMsg", primary)
	}
}

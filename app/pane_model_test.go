package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// N-pane model (#1088): open/focus/hide panes, the dynamic focus ring, the
// §2.6 pane-count fitting (auto-hide on shrink, restore on grow), capture
// throttling and the attached pause across N panes, attach of the focused
// pane, and the pane bindings following instance removal / same-title swaps.
// ----------------------------------------------------------------------------

// paneTestHome is a home with three started instances at a three-pane-capable
// size, with the selection on "alpha".
func paneTestHome(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	h.store.AddInstance(instanceWithFakeBackend(t, "alpha"))
	h.store.AddInstance(instanceWithFakeBackend(t, "beta"))
	h.store.AddInstance(instanceWithFakeBackend(t, "gamma"))
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 200, 40)
	return h
}

// pressKey drives handleDefaultKeyPress with a raw key string, the full
// dispatch path (menu highlighting re-emit excluded — tests call the handler
// directly like the other model-level suites).
func pressKey(t *testing.T, h *home, key string) {
	t.Helper()
	name, ok := keys.GlobalKeyStringsMap[key]
	require.True(t, ok, "key %q must be mapped", key)
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}, name)
}

// pressTab cycles the focus ring.
func pressTab(t *testing.T, h *home, back bool) {
	t.Helper()
	if back {
		_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab}, keys.KeyShiftTab)
		return
	}
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
}

// visibleTitles flattens the visible panes to "<title>:<tab>" for assertions.
func visibleTitles(h *home) []string {
	out := make([]string, 0, len(h.visiblePanes))
	for _, p := range h.visiblePanes {
		title := ""
		if p.Instance() != nil {
			title = p.Instance().Title
		}
		out = append(out, title)
	}
	return out
}

// TestPane_OpenHideFlow walks the core verb set: s with tree focus opens the
// selection as a focused pane; a second s on another instance opens a second
// pane to the RIGHT; x hides the focused pane, the survivor re-divides the
// full workspace width, and nothing is killed.
func TestPane_OpenHideFlow(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	// s with tree focus: open (alpha, tab 0) as a focused pane.
	require.Equal(t, layout.RegionTree, h.ring.Active())
	pressKey(t, h, "s")
	require.Equal(t, 1, h.store.NumOpenPanes(), "s opens the selection as a pane")
	p1 := h.store.OpenPanes()[0]
	assert.Same(t, alpha, p1.Instance())
	assert.Equal(t, 0, p1.Tab())
	assert.Equal(t, layout.PaneRegion(p1.ID()), h.ring.Active(), "the opened pane takes focus")
	fullWidth := h.lastLayout.Panes[0].W

	// The tree keeps driving the selection without touching the pane.
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.Same(t, beta, h.store.GetSelectedInstance())
	assert.Same(t, alpha, p1.Instance(), "open panes are explicit bindings, not selection-driven")

	// s again: beta opens as a NEW pane to the right of alpha's.
	pressKey(t, h, "s")
	require.Equal(t, 2, h.store.NumOpenPanes())
	assert.Equal(t, []string{"alpha", "beta"}, visibleTitles(h), "new panes open to the right")
	p2 := h.store.OpenPanes()[1]
	assert.Equal(t, layout.PaneRegion(p2.ID()), h.ring.Active())
	assert.Equal(t, 2, h.lastLayout.PaneCount(), "two panes are laid out side by side")
	assert.Less(t, h.lastLayout.Panes[0].W, fullWidth, "panes divide the width")

	// x hides the focused (beta) pane: alpha's pane re-absorbs the full
	// width, focus lands on it, and beta keeps running.
	tabsBefore := len(beta.GetTabs())
	pressKey(t, h, "x")
	require.Equal(t, 1, h.store.NumOpenPanes(), "x hides the focused pane")
	assert.Equal(t, []string{"alpha"}, visibleTitles(h))
	assert.Equal(t, fullWidth, h.lastLayout.Panes[0].W, "the survivor re-divides the full width")
	assert.Equal(t, layout.PaneRegion(p1.ID()), h.ring.Active(), "focus lands on the surviving pane")
	assert.Equal(t, tabsBefore, len(beta.GetTabs()), "hiding kills nothing")
	assert.True(t, beta.TmuxAlive(), "the hidden tab keeps running in tmux")

	// x on the last pane empties the workspace; focus returns to the tree.
	pressKey(t, h, "x")
	require.Zero(t, h.store.NumOpenPanes())
	assert.Equal(t, layout.RegionTree, h.ring.Active())
	assert.Contains(t, h.View(), "no panes open", "the empty workspace advertises the open verb")
}

// TestPane_OpenAlreadyOpenFocuses: s on a tab that is already open as a pane
// focuses that pane instead of duplicating it (§2.3).
func TestPane_OpenAlreadyOpenFocuses(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s")
	p1 := h.store.OpenPanes()[0]

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	require.Equal(t, 2, h.store.NumOpenPanes())

	// Back to alpha (still open in pane 1): s focuses, no third pane.
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	assert.Equal(t, 2, h.store.NumOpenPanes(), "an already-open tab must not open twice")
	assert.Equal(t, layout.PaneRegion(p1.ID()), h.ring.Active(), "s focuses the existing pane")
}

// TestPane_TabDimension: opening from a tree TAB row binds that tab, distinct
// (instance, tab) pairs get distinct panes, and later selection tab jumps
// don't touch open panes.
func TestPane_TabDimension(t *testing.T) {
	h := paneTestHome(t)

	// Walk the cursor onto alpha's second tab row (j: instance → tab 0 → tab 1).
	pressNav(t, h, "j")
	pressNav(t, h, "j")
	require.True(t, h.sidebar.GetSelection().IsTab)
	require.Equal(t, 1, h.store.ActiveTab())

	pressKey(t, h, "s")
	require.Equal(t, 1, h.store.NumOpenPanes())
	terminalPane := h.store.OpenPanes()[0]
	assert.Equal(t, 1, terminalPane.Tab(), "the tree row's tab is what gets bound")

	// Jumping the selection tab must not move the open pane's binding —
	// and s on the OTHER tab of the same instance opens a second pane.
	_, _ = h.handleTabJump(1)
	require.Equal(t, 0, h.store.ActiveTab())
	assert.Equal(t, 1, terminalPane.Tab(), "the pane's tab is bound independently of the selection")

	pressKey(t, h, "s")
	require.Equal(t, 2, h.store.NumOpenPanes(), "each (instance, tab) pair is its own pane")
	assert.Equal(t, 0, h.store.OpenPanes()[1].Tab())
}

// TestPane_FocusRingCyclesNPanes: with three panes open, Tab cycles
// tree → pane 1 → pane 2 → pane 3 → automations and wraps; Shift-Tab
// reverses; with no panes the ring is tree → automations.
func TestPane_FocusRingCyclesNPanes(t *testing.T) {
	h := paneTestHome(t)

	// No panes: the ring is tree → automations → tree.
	for _, want := range []string{layout.RegionAutomations, layout.RegionTree} {
		pressTab(t, h, false)
		require.Equal(t, want, h.ring.Active(), "without panes the ring is tree → automations")
	}

	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, 3, h.store.NumOpenPanes())
	require.Equal(t, 3, h.lastLayout.PaneCount(), "200 cols fits three panes")
	panes := h.store.OpenPanes()

	h.focusRegion(layout.RegionTree)
	forward := []string{
		layout.PaneRegion(panes[0].ID()),
		layout.PaneRegion(panes[1].ID()),
		layout.PaneRegion(panes[2].ID()),
		layout.RegionAutomations,
		layout.RegionTree,
	}
	for _, want := range forward {
		pressTab(t, h, false)
		require.Equal(t, want, h.ring.Active(), "Tab must cycle tree → panes in order → automations")
	}
	backward := []string{
		layout.RegionAutomations,
		layout.PaneRegion(panes[2].ID()),
		layout.PaneRegion(panes[1].ID()),
		layout.PaneRegion(panes[0].ID()),
		layout.RegionTree,
	}
	for _, want := range backward {
		pressTab(t, h, true)
		require.Equal(t, want, h.ring.Active(), "Shift-Tab must cycle the same ring backwards")
	}
}

// TestPane_AutoHideOnShrinkRestoreOnGrow drives the §2.6 pane-count fitting:
// shrinking below what fits auto-hides the least-recently-focused panes —
// bindings retained, focused pane always visible — and growing restores them
// in workspace order.
func TestPane_AutoHideOnShrinkRestoreOnGrow(t *testing.T) {
	h := paneTestHome(t)
	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, []string{"alpha", "beta", "gamma"}, visibleTitles(h))
	panes := h.store.OpenPanes()

	// Focus alpha's pane: recency is now alpha > gamma > beta.
	h.focusRegion(layout.PaneRegion(panes[0].ID()))

	// Two panes fit at 150 cols: beta (least recently focused) auto-hides;
	// the binding stays open and the survivors keep workspace order.
	resizeHome(h, 150, 40)
	require.Equal(t, 2, h.lastLayout.PaneCount())
	assert.Equal(t, []string{"alpha", "gamma"}, visibleTitles(h),
		"the least-recently-focused pane auto-hides first")
	assert.Equal(t, 3, h.store.NumOpenPanes(), "auto-hide retains the binding")
	assert.Equal(t, layout.PaneRegion(panes[0].ID()), h.ring.Active(),
		"the focused pane is never the one auto-hidden")

	// One pane below the multi-pane threshold: only the focused pane stays.
	resizeHome(h, layout.MultiPaneMinWidth-1, 40)
	require.Equal(t, 1, h.lastLayout.PaneCount())
	assert.Equal(t, []string{"alpha"}, visibleTitles(h))
	assert.Equal(t, 3, h.store.NumOpenPanes())

	// Growing back restores every pane, in workspace order.
	resizeHome(h, 200, 40)
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, visibleTitles(h),
		"grow restores the auto-hidden panes in order")
}

// TestPane_OpenBeyondCapacityAutoHidesLRU: opening one more pane than fits
// auto-hides the least-recently-focused pane instead of erroring (§2.6).
func TestPane_OpenBeyondCapacityAutoHidesLRU(t *testing.T) {
	h := paneTestHome(t)
	resizeHome(h, 150, 40) // two panes fit

	for i := 0; i < 2; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, []string{"alpha", "beta"}, visibleTitles(h))

	// Opening gamma at capacity hides alpha (LRU) and shows the new pane.
	h.sidebar.SetSelectedInstance(2)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	require.Equal(t, 3, h.store.NumOpenPanes(), "the third pane opens")
	assert.Equal(t, []string{"beta", "gamma"}, visibleTitles(h),
		"opening beyond capacity auto-hides the least-recently-focused pane")
	gamma := h.store.OpenPanes()[2]
	assert.Equal(t, layout.PaneRegion(gamma.ID()), h.ring.Active(), "the new pane is focused")
}

// TestPane_DegradationLadderWithNPanes drives the resize ladder with panes
// open: minimal mode keeps exactly one pane, fallback renders the banner, and
// growing back restores all bindings.
func TestPane_DegradationLadderWithNPanes(t *testing.T) {
	h := paneTestHome(t)
	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, 3, h.lastLayout.PaneCount())

	resizeHome(h, 59, 14)
	assert.False(t, h.lastLayout.AutomationsVisible, "minimal mode drops the strip")
	assert.Equal(t, 1, h.lastLayout.PaneCount(), "minimal mode keeps a single pane")
	requireViewSized(t, h.View(), 59, 14)

	resizeHome(h, 39, 9)
	require.True(t, h.lastLayout.Fallback)
	assert.Empty(t, h.visiblePanes)
	view := h.View()
	requireViewSized(t, view, 39, 9)
	assert.Contains(t, view, "Terminal too small")

	resizeHome(h, 200, 40)
	assert.Equal(t, 3, h.lastLayout.PaneCount(), "grow restores every open pane")
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, visibleTitles(h))
}

// TestPane_WKeepsTabKillMeaning: `w` with a pane focused still means "kill
// the selection's active tab" (here the unclosable agent tab → friendly
// error), never "hide the pane" — that is `x` (§2.3).
func TestPane_WKeepsTabKillMeaning(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	tabsBefore := len(alpha.GetTabs())

	pressKey(t, h, "s")
	require.Equal(t, 1, h.store.NumOpenPanes())
	pressKey(t, h, "w")

	assert.Equal(t, 1, h.store.NumOpenPanes(), "w must not hide the pane")
	assert.Equal(t, tabsBefore, len(alpha.GetTabs()), "the agent tab is never closed")
	assert.Contains(t, h.errBox.String(), "agent tab", "w on the agent tab surfaces the friendly error")
}

// TestPane_CloseTabRebindsPanes: `t` opens the fresh tab as a pane, and
// killing a middle tab (w) hides its pane while shifting higher-slot panes'
// bindings down so they keep showing the same tab.
func TestPane_CloseTabRebindsPanes(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "rebind")
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	restore := SetTabCreatorForTest(func(title, repoID string) (string, error) {
		return nextShellTabName(inst.GetTabs()), nil
	})
	defer restore()
	_, _ = h.handleNewTab() // agent + shell + shell-2
	require.Equal(t, 3, inst.TabCount())
	require.Equal(t, 1, h.store.NumOpenPanes(), "t opens the fresh tab as a pane")
	require.Equal(t, 2, h.store.OpenPanes()[0].Tab(), "bound to the new last slot")

	// Also open the slot-1 shell pane.
	_, _ = h.openOrFocusPane(inst, 1)
	require.Equal(t, 2, h.store.NumOpenPanes())

	// Kill tab 1: its pane hides; the slot-2 pane re-binds to slot 1.
	h.store.SetActiveTab(1)
	restoreClose := SetTabCloserForTest(func(title, repoID, tabName string) error { return nil })
	defer restoreClose()
	_, _ = h.handleCloseTab()

	require.Equal(t, 2, inst.TabCount())
	require.Equal(t, 1, h.store.NumOpenPanes(), "the killed tab's pane leaves the workspace")
	assert.Equal(t, 1, h.store.OpenPanes()[0].Tab(), "the surviving pane re-binds to the shifted slot")
}

// TestPane_AllPanesPausedWhileAttached extends the #598 gate to N capture
// slots: with panes open and the user attached, selectionChanged must
// dispatch NO capture work.
func TestPane_AllPanesPausedWhileAttached(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s")
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	require.Equal(t, 2, h.store.NumOpenPanes())

	// Age the throttle so it cannot be what suppresses the captures.
	h.lastPaneCapture = make(map[int]time.Time)

	h.attached.Store(true)
	cmd := h.selectionChanged()
	assert.Nil(t, cmd,
		"selectionChanged must return nil while attached: every pane's capture "+
			"is gated behind the attached flag (#598), so nothing may queue "+
			"behind the user's detach key")
}

// TestPane_CaptureThrottled pins the RFC §5.2 contention lever: each pane's
// capture dispatch is floored at paneCaptureMinInterval, so raising that one
// constant degrades every pane's cadence without touching the tick.
func TestPane_CaptureThrottled(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s")
	require.Equal(t, 1, h.lastLayout.PaneCount())

	h.lastPaneCapture = make(map[int]time.Time)
	require.NotNil(t, h.panesRefresh(false), "an aged throttle admits the capture")
	assert.Nil(t, h.panesRefresh(false),
		"a second dispatch inside paneCaptureMinInterval must be swallowed")
}

// TestPane_InstanceRemovedPrunesPanes: when a pane's instance leaves the
// projection (killed here or externally), the next tick closes that pane
// instead of rendering a dead session's last capture forever.
func TestPane_InstanceRemovedPrunesPanes(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s")
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	require.Equal(t, 2, h.store.NumOpenPanes())

	h.store.RemoveInstanceByTitle("alpha")
	_ = h.selectionChanged()
	assert.Equal(t, 1, h.store.NumOpenPanes(),
		"the removed instance's pane must be pruned")
	assert.Equal(t, []string{"beta"}, visibleTitles(h))
}

// TestPane_FollowsSameTitleSwap: a #765 kill+recreate swap (same title,
// rebuilt pointer) re-points open-pane bindings onto the replacement, so open
// panes keep showing the live session.
func TestPane_FollowsSameTitleSwap(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s")
	p := h.store.OpenPanes()[0]
	require.Equal(t, "alpha", p.Instance().Title)

	rebuilt := instanceWithFakeBackend(t, "alpha")
	require.True(t, h.store.ReplaceInstanceByTitle("alpha", rebuilt))
	assert.Same(t, rebuilt, p.Instance(),
		"open-pane bindings must follow a same-title swap (#765 class)")
}

// TestPane_EnterAttachesFocusedPane: Enter attaches the FOCUSED pane's
// binding — a pane's (instance, tab) when a pane has focus, the tree
// selection everywhere else — and detach hands back focus + panes intact.
func TestPane_EnterAttachesFocusedPane(t *testing.T) {
	resetDetachWatchdog(t)
	h := paneTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	// Open alpha's pane, then drive the tree selection to beta.
	pressKey(t, h, "s")
	p := h.store.OpenPanes()[0]
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.Equal(t, "beta", h.store.GetSelectedInstance().Title)

	var attachedLabel string
	swapAttachOverlayCallbackFn(t, func(m *home, label, traceSuffix string, rem bool, _ func() (chan struct{}, error)) tea.Cmd {
		attachedLabel = label
		return m.attachOverlayCallback(label, traceSuffix, rem, func() (chan struct{}, error) {
			ch := make(chan struct{})
			close(ch) // detach immediately — no real PTY
			return ch, nil
		})
	})

	// Focus the pane: Enter must take the pane attach path (alpha, not the
	// beta selection).
	h.focusRegion(layout.PaneRegion(p.ID()))
	_, cmd := h.handleEnter()
	require.NotNil(t, cmd, "the pane attach must run")
	assert.Equal(t, "handleEnter-pane", attachedLabel,
		"Enter with a pane focused attaches that pane's binding")
	endDetachWatchdog()

	// Detach restored everything: focus still on the pane, binding intact.
	assert.Equal(t, layout.PaneRegion(p.ID()), h.ring.Active(), "detach restores focus")
	assert.Equal(t, 1, h.store.NumOpenPanes(), "detach keeps the pane open")
	assert.Equal(t, "alpha", h.store.OpenPanes()[0].Instance().Title)

	// Focus the tree: Enter attaches the selection, as before.
	h.focusRegion(layout.RegionTree)
	_, cmd = h.handleEnter()
	require.NotNil(t, cmd)
	assert.Equal(t, "handleEnter-sidebar", attachedLabel,
		"Enter with tree focus attaches the selection, as before the pane model")
	endDetachWatchdog()
}

// TestPane_HideMiddleFocusesSuccessor: hiding a middle pane lands focus on
// the pane that takes its slot, and the remaining panes keep workspace order.
func TestPane_HideMiddleFocusesSuccessor(t *testing.T) {
	h := paneTestHome(t)
	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	panes := h.store.OpenPanes()

	h.focusRegion(layout.PaneRegion(panes[1].ID()))
	pressKey(t, h, "x")
	assert.Equal(t, []string{"alpha", "gamma"}, visibleTitles(h))
	assert.Equal(t, layout.PaneRegion(h.store.OpenPanes()[1].ID()), h.ring.Active(),
		"focus lands on the pane that took the hidden pane's slot")
}

// TestE2E_PaneFlow drives the real tea.Program through the pane lifecycle:
// s opens a focused pane, the tree walks to another instance and s opens a
// second pane beside it, Tab cycles the ring across both, and x hides the
// focused pane with focus landing on the survivor.
func TestE2E_PaneFlow(t *testing.T) {
	eh := newE2EHarness(t)
	eh.addStartedInstance("alpha")
	eh.addStartedInstance("beta")
	eh.home.sidebar.SetSelectedInstance(0)
	eh.start()

	paneState := func() (count int, titles []string, region string) {
		eh.query(func(h *home) {
			count = h.store.NumOpenPanes()
			titles = visibleTitles(h)
			region = h.ring.Active()
		})
		return
	}

	// s opens the selection (alpha) as a focused pane.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	eh.waitUntil(e2eAsyncTimeout, "s opens alpha's pane focused", func() bool {
		count, titles, region := paneState()
		return count == 1 && len(titles) == 1 && titles[0] == "alpha" && layout.IsPaneRegion(region)
	})

	// The tree walks to beta (j walks alpha → its two tab rows → beta);
	// alpha's pane stays put. Then s opens beta beside it.
	for i := 0; i < 3; i++ {
		eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	eh.waitUntil(e2eAsyncTimeout, "tree selection lands on beta", func() bool {
		var selected string
		eh.query(func(h *home) {
			if s := h.store.GetSelectedInstance(); s != nil {
				selected = s.Title
			}
		})
		return selected == "beta"
	})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	eh.waitUntil(e2eAsyncTimeout, "s opens beta's pane to the right", func() bool {
		count, titles, _ := paneState()
		return count == 2 && len(titles) == 2 && titles[0] == "alpha" && titles[1] == "beta"
	})

	// Both pane headers render side by side.
	var view string
	eh.query(func(h *home) { view = h.View() })
	// Pane 2's tab label is whatever the shared active-tab index resolved to
	// after the tree walk (the index survives instance switches by design),
	// so assert the instance halves only.
	assert.Contains(t, view, "alpha · Preview", "pane 1 header shows its binding")
	assert.Contains(t, view, "beta · ", "pane 2 header shows its binding")

	// Tab from the beta pane wraps via automations/tree back around to the
	// alpha pane; assert the ring visits a pane region again.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.waitUntil(e2eAsyncTimeout, "the focus ring cycles back to a pane", func() bool {
		_, _, region := paneState()
		return layout.IsPaneRegion(region)
	})

	// x hides the focused pane; the survivor keeps focus and the workspace.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	eh.waitUntil(e2eAsyncTimeout, "x hides the focused pane", func() bool {
		count, titles, region := paneState()
		return count == 1 && len(titles) == 1 && layout.IsPaneRegion(region)
	})
}

// TestPane_StoreOpenPanePrimitives unit-tests the store's open-pane list:
// dedupe lookup, ordered append, close, and the recency-ranked visibility
// pick that drives the §2.6 auto-hide.
func TestPane_StoreOpenPanePrimitives(t *testing.T) {
	proj := store.NewProjection()
	a := instanceWithFakeBackend(t, "a")
	b := instanceWithFakeBackend(t, "b")
	proj.AddInstance(a)
	proj.AddInstance(b)

	require.Nil(t, proj.AddOpenPane(nil, 0), "nil instances never open")

	p1 := proj.AddOpenPane(a, 0)
	p2 := proj.AddOpenPane(a, 1)
	p3 := proj.AddOpenPane(b, 0)
	require.Equal(t, 3, proj.NumOpenPanes())
	assert.Same(t, p2, proj.FindOpenPane(a, 1))
	assert.Nil(t, proj.FindOpenPane(b, 1))
	assert.NotEqual(t, p1.ID(), p2.ID(), "pane ids are unique")

	// Visibility: all fit → workspace order regardless of recency.
	proj.TouchOpenPane(p1)
	vis := proj.VisibleOpenPanes(3)
	require.Equal(t, []*store.OpenPane{p1, p2, p3}, vis)

	// Two fit: p2 is now least recently focused (p3 opened after it, p1
	// touched last) → p2 hides, order preserved.
	vis = proj.VisibleOpenPanes(2)
	require.Equal(t, []*store.OpenPane{p1, p3}, vis)

	// One fits: only the most recently focused survives.
	vis = proj.VisibleOpenPanes(1)
	require.Equal(t, []*store.OpenPane{p1}, vis)
	assert.Empty(t, proj.VisibleOpenPanes(0))

	require.True(t, proj.CloseOpenPane(p2))
	require.False(t, proj.CloseOpenPane(p2), "closing twice reports absence")
	assert.Equal(t, 2, proj.NumOpenPanes())
}

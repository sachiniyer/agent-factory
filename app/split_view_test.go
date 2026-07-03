package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// Split view (#1024 PR 5): pane B open/swap/close, the pinned binding, the
// four-region focus ring, the attached pause covering both panes, attach of
// the focused pane, and the §2.6 collapse-on-narrow / restore-on-grow ladder.
// ----------------------------------------------------------------------------

// splitTestHome is a home with two started instances at a split-capable size,
// with the selection on "alpha".
func splitTestHome(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	h.store.AddInstance(instanceWithFakeBackend(t, "alpha"))
	h.store.AddInstance(instanceWithFakeBackend(t, "beta"))
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 140, 40)
	return h
}

// pressKey drives handleKeyPress with a raw key string, the full dispatch
// path (menu highlighting re-emit excluded — tests call the handler directly
// like the other model-level suites).
func pressKey(t *testing.T, h *home, key string) {
	t.Helper()
	name, ok := keys.GlobalKeyStringsMap[key]
	require.True(t, ok, "key %q must be mapped", key)
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}, name)
}

// TestSplit_OpenSwapClose walks the whole verb set: s with tree focus opens
// the selection in pane B; s with pane focus swaps A↔B; x on pane B closes
// the split and focus lands on pane A.
func TestSplit_OpenSwapClose(t *testing.T) {
	h := splitTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	// s with tree focus: open the selection (alpha, tab 0) in pane B.
	require.Equal(t, layout.RegionTree, h.ring.Active())
	pressKey(t, h, "s")
	require.True(t, h.store.SplitOpen(), "s opens the split")
	require.True(t, h.lastLayout.SplitActive, "the grid honors the split at 140 cols")
	assert.Same(t, alpha, h.store.PaneBInstance())
	assert.Equal(t, 0, h.store.PaneBTab())

	// The tree keeps driving pane A: select beta, pane B stays pinned.
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.Same(t, beta, h.store.GetSelectedInstance())
	assert.Same(t, alpha, h.store.PaneBInstance(), "pane B is pinned while the tree drives pane A")

	// s with pane A focus: swap — selection moves to alpha, pane B pins beta.
	h.focusRegion(layout.RegionPaneA)
	pressKey(t, h, "s")
	assert.Same(t, alpha, h.store.GetSelectedInstance(), "swap moves pane B's binding into pane A (the selection)")
	assert.Same(t, beta, h.store.PaneBInstance(), "swap pins pane A's old binding into pane B")
	assert.Equal(t, layout.RegionPaneA, h.ring.Active(), "focus stays where it was across a swap")

	// s with pane B focus swaps back.
	h.focusRegion(layout.RegionPaneB)
	pressKey(t, h, "s")
	assert.Same(t, beta, h.store.GetSelectedInstance())
	assert.Same(t, alpha, h.store.PaneBInstance())

	// x on pane B closes the split; focus lands on pane A.
	pressKey(t, h, "x")
	assert.False(t, h.store.SplitOpen(), "x closes the split")
	assert.False(t, h.lastLayout.SplitActive)
	assert.Equal(t, layout.RegionPaneA, h.ring.Active(), "focus moves off the closed pane onto pane A")
}

// TestSplit_PinnedTabDimension: opening the split from a tree TAB row pins
// that tab into pane B, and later tab moves in pane A don't touch it.
func TestSplit_PinnedTabDimension(t *testing.T) {
	h := splitTestHome(t)

	// Walk the cursor onto alpha's second tab row (j: instance → tab 0 → tab 1).
	pressNav(t, h, "j")
	pressNav(t, h, "j")
	require.True(t, h.sidebar.GetSelection().IsTab)
	require.Equal(t, 1, h.store.ActiveTab())

	pressKey(t, h, "s")
	require.True(t, h.store.SplitOpen())
	assert.Equal(t, 1, h.store.PaneBTab(), "the tree row's tab is what gets pinned")

	// Pane A jumping tabs must not move pane B's pinned tab.
	_, _ = h.handleTabJump(1)
	require.Equal(t, 0, h.store.ActiveTab())
	assert.Equal(t, 1, h.store.PaneBTab(), "pane B's tab is pinned independently of pane A's")
}

// TestSplit_FocusRingIncludesPaneB: with a split open, Tab cycles
// tree → pane A → pane B → automations and wraps; Shift-Tab reverses; with no
// split, pane B is skipped entirely (the PR-4 ring).
func TestSplit_FocusRingIncludesPaneB(t *testing.T) {
	h := splitTestHome(t)

	// No split: B is hidden from the ring.
	for _, want := range []string{layout.RegionPaneA, layout.RegionAutomations, layout.RegionTree} {
		_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
		require.Equal(t, want, h.ring.Active(), "without a split the ring must skip pane B")
	}

	pressKey(t, h, "s")
	require.True(t, h.lastLayout.SplitActive)

	for _, want := range []string{layout.RegionPaneA, layout.RegionPaneB, layout.RegionAutomations, layout.RegionTree} {
		_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
		require.Equal(t, want, h.ring.Active(), "with a split open Tab must cycle tree → A → B → automations")
	}
	for _, want := range []string{layout.RegionAutomations, layout.RegionPaneB, layout.RegionPaneA, layout.RegionTree} {
		_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab}, keys.KeyShiftTab)
		require.Equal(t, want, h.ring.Active(), "Shift-Tab must cycle the same ring backwards")
	}
}

// TestSplit_WOnPaneBClosesSplitNotTab: `w` with pane B focused closes the
// split — it must never fall through to handleCloseTab and close a tab of the
// tree selection (RFC §2.3 "x/w on pane B closes the split").
func TestSplit_WOnPaneBClosesSplitNotTab(t *testing.T) {
	h := splitTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	tabsBefore := len(alpha.GetTabs())

	pressKey(t, h, "s")
	h.focusRegion(layout.RegionPaneB)
	pressKey(t, h, "w")

	assert.False(t, h.store.SplitOpen(), "w on pane B closes the split")
	assert.Equal(t, layout.RegionPaneA, h.ring.Active())
	assert.Equal(t, tabsBefore, len(alpha.GetTabs()), "no tab of the selection may be closed")
}

// TestSplit_CollapseOnNarrowRestoreOnGrow drives the §2.6 ladder: below
// SplitMinWidth the split collapses to pane A while pane B's binding is
// retained — and out of the focus ring — then the split restores intact when
// the terminal grows back.
func TestSplit_CollapseOnNarrowRestoreOnGrow(t *testing.T) {
	h := splitTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")

	pressKey(t, h, "s")
	require.True(t, h.lastLayout.SplitActive)
	h.focusRegion(layout.RegionPaneB)

	resizeHome(h, layout.SplitMinWidth-1, 40)
	assert.False(t, h.lastLayout.SplitActive, "<110 cols: the split collapses to pane A")
	assert.True(t, h.store.SplitOpen(), "pane B's binding is retained through the collapse")
	assert.Same(t, alpha, h.store.PaneBInstance())
	assert.Equal(t, layout.RegionPaneA, h.ring.Active(),
		"focus can't stay on the invisible pane — it lands on pane A")

	// The collapsed pane leaves the ring: tree → A → automations only.
	h.focusRegion(layout.RegionPaneA)
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
	assert.Equal(t, layout.RegionAutomations, h.ring.Active(),
		"the collapsed pane B must be skipped by the focus ring")

	resizeHome(h, 140, 40)
	assert.True(t, h.lastLayout.SplitActive, "growing back restores the split")
	assert.Same(t, alpha, h.store.PaneBInstance(), "with the same pinned binding")
}

// TestSplit_OpenRefusedWhenTooNarrow: a FRESH split request on a terminal the
// grid can't honor is refused with an actionable error instead of arming an
// invisible pane (retain-on-narrow is only for splits that were already open).
func TestSplit_OpenRefusedWhenTooNarrow(t *testing.T) {
	h := splitTestHome(t)
	resizeHome(h, layout.SplitMinWidth-1, 40)

	pressKey(t, h, "s")
	assert.False(t, h.store.SplitOpen(), "the refused split must not leave a binding behind")
	assert.False(t, h.lastLayout.SplitActive)
}

// TestSplit_NarrowSKeepsRetainedBinding is the Greptile repro on #1085: a
// COLLAPSED-but-retained split (shrunk below SplitMinWidth with pane B's
// binding kept for restore-on-grow) must survive `s` being pressed while
// still narrow. The old refuse-fresh-open path armed the binding and rolled
// it back with ClearPaneB, which clobbered the RETAINED binding — so growing
// back never restored the split. A narrow `s` may be refused, but it must
// never destroy an existing retained binding.
func TestSplit_NarrowSKeepsRetainedBinding(t *testing.T) {
	h := splitTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")

	// Open a wide split pinned to alpha, then shrink below the threshold:
	// collapsed, binding retained.
	pressKey(t, h, "s")
	require.True(t, h.lastLayout.SplitActive)
	require.Same(t, alpha, h.store.PaneBInstance())
	resizeHome(h, layout.SplitMinWidth-1, 40)
	require.False(t, h.lastLayout.SplitActive)
	require.Same(t, alpha, h.store.PaneBInstance(), "collapse retains the binding")

	// The exact repro: focus pane A and press s while still narrow.
	h.focusRegion(layout.RegionPaneA)
	pressKey(t, h, "s")
	assert.Same(t, alpha, h.store.PaneBInstance(),
		"a narrow s must NOT clear the retained pane-B binding")
	assert.True(t, h.store.SplitOpen())

	// Same from tree focus (the other openSplitFromSelection route).
	h.focusRegion(layout.RegionTree)
	pressKey(t, h, "s")
	assert.Same(t, alpha, h.store.PaneBInstance(),
		"a narrow s from the tree must NOT clear the retained binding either")

	// Growing back restores the split with pane B intact.
	resizeHome(h, 140, 40)
	assert.True(t, h.lastLayout.SplitActive, "grow must restore the collapsed split")
	assert.Same(t, alpha, h.store.PaneBInstance(), "with the same pinned binding")
}

// TestSplit_BothPanesPausedWhileAttached extends the #598 gate to the second
// capture slot: with a split open and the user attached, selectionChanged
// must dispatch NO capture work — not for pane A, not for pane B.
func TestSplit_BothPanesPausedWhileAttached(t *testing.T) {
	h := splitTestHome(t)
	pressKey(t, h, "s")
	require.True(t, h.store.SplitOpen())

	// Age the throttle so it cannot be what suppresses pane B's capture.
	h.lastPaneBCapture = time.Time{}

	h.attached.Store(true)
	cmd := h.selectionChanged()
	assert.Nil(t, cmd,
		"selectionChanged must return nil while attached: BOTH panes' captures "+
			"are gated behind the attached flag (#598), so nothing may queue "+
			"behind the user's detach key")
}

// TestSplit_PaneBCaptureThrottled pins the RFC §5.2 contention lever: pane
// B's capture dispatch is floored at paneBCaptureMinInterval, so raising that
// one constant degrades pane B's cadence without touching pane A or the tick.
func TestSplit_PaneBCaptureThrottled(t *testing.T) {
	h := splitTestHome(t)
	pressKey(t, h, "s")
	require.True(t, h.lastLayout.SplitActive)

	h.lastPaneBCapture = time.Time{}
	require.NotNil(t, h.paneBRefresh(false), "an aged throttle admits the capture")
	assert.Nil(t, h.paneBRefresh(false),
		"a second dispatch inside paneBCaptureMinInterval must be swallowed")
}

// TestSplit_PaneBInstanceRemovedClosesSplit: when the pinned instance leaves
// the projection (killed here or externally), the next tick closes the split
// instead of rendering a dead session's last capture forever.
func TestSplit_PaneBInstanceRemovedClosesSplit(t *testing.T) {
	h := splitTestHome(t)
	pressKey(t, h, "s")
	require.True(t, h.store.SplitOpen())

	h.store.RemoveInstanceByTitle("alpha")
	_ = h.selectionChanged()
	assert.False(t, h.store.SplitOpen(),
		"the pinned instance leaving the projection must close the split")
}

// TestSplit_PaneBFollowsSameTitleSwap: a #765 kill+recreate swap (same title,
// rebuilt pointer) re-points pane B's pinned binding onto the replacement, so
// an open split keeps showing the live session.
func TestSplit_PaneBFollowsSameTitleSwap(t *testing.T) {
	h := splitTestHome(t)
	pressKey(t, h, "s")
	require.Equal(t, "alpha", h.store.PaneBInstance().Title)

	rebuilt := instanceWithFakeBackend(t, "alpha")
	require.True(t, h.store.ReplaceInstanceByTitle("alpha", rebuilt))
	assert.Same(t, rebuilt, h.store.PaneBInstance(),
		"pane B's pinned binding must follow a same-title swap (#765 class)")
}

// TestSplit_EnterAttachesFocusedPane: Enter attaches the FOCUSED pane's
// binding — pane B's pinned instance when pane B has focus, the tree
// selection everywhere else — and detach hands back focus + split intact.
func TestSplit_EnterAttachesFocusedPane(t *testing.T) {
	resetDetachWatchdog(t)
	h := splitTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	// Pin alpha into pane B, then drive the tree selection to beta.
	pressKey(t, h, "s")
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.Equal(t, "beta", h.store.GetSelectedInstance().Title)
	require.Equal(t, "alpha", h.store.PaneBInstance().Title)

	var attachedLabel string
	swapAttachOverlayCallbackFn(t, func(m *home, label, traceSuffix string, rem bool, _ func() (chan struct{}, error)) tea.Cmd {
		attachedLabel = label
		return m.attachOverlayCallback(label, traceSuffix, rem, func() (chan struct{}, error) {
			ch := make(chan struct{})
			close(ch) // detach immediately — no real PTY
			return ch, nil
		})
	})

	// Focus pane B: Enter must take the pane-B attach path.
	h.focusRegion(layout.RegionPaneB)
	_, cmd := h.handleEnter()
	require.NotNil(t, cmd, "the pane-B attach must run")
	assert.Equal(t, "handleEnter-paneB", attachedLabel,
		"Enter with pane B focused attaches pane B's pinned binding")
	endDetachWatchdog()

	// Detach restored everything: focus still on pane B, split still open.
	assert.Equal(t, layout.RegionPaneB, h.ring.Active(), "detach restores focus")
	assert.True(t, h.store.SplitOpen(), "detach restores the split")
	assert.True(t, h.lastLayout.SplitActive)
	assert.Equal(t, "alpha", h.store.PaneBInstance().Title)

	// Focus the tree: Enter attaches the selection (pane A's binding).
	h.focusRegion(layout.RegionTree)
	_, cmd = h.handleEnter()
	require.NotNil(t, cmd)
	assert.Equal(t, "handleEnter-sidebar", attachedLabel,
		"Enter with tree focus attaches the selection, as before the split")
	endDetachWatchdog()
}

// TestE2E_SplitFlow drives the real tea.Program through the split lifecycle:
// s opens the split (two live pane headers side by side), the tree keeps
// driving pane A while pane B stays pinned, Tab reaches pane B, s swaps the
// panes, x closes the split and focus lands on pane A.
func TestE2E_SplitFlow(t *testing.T) {
	eh := newE2EHarness(t)
	eh.addStartedInstance("alpha")
	eh.addStartedInstance("beta")
	eh.home.sidebar.SetSelectedInstance(0)
	eh.start()

	splitState := func() (open, active bool, pinned, selected, region string) {
		eh.query(func(h *home) {
			open = h.store.SplitOpen()
			active = h.lastLayout.SplitActive
			region = h.ring.Active()
			if b := h.store.PaneBInstance(); b != nil {
				pinned = b.Title
			}
			if s := h.store.GetSelectedInstance(); s != nil {
				selected = s.Title
			}
		})
		return
	}

	// s opens the selection (alpha) in pane B.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	eh.waitUntil(e2eAsyncTimeout, "s opens the split pinned to alpha", func() bool {
		open, active, pinned, _, _ := splitState()
		return open && active && pinned == "alpha"
	})

	// The tree drives pane A to beta (j walks alpha → its two tab rows → beta);
	// pane B stays pinned to alpha.
	for i := 0; i < 3; i++ {
		eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	eh.waitUntil(e2eAsyncTimeout, "tree selection lands on beta, pane B stays alpha", func() bool {
		_, _, pinned, selected, _ := splitState()
		return selected == "beta" && pinned == "alpha"
	})

	// Both panes render their headers side by side. Pane A's tab label is
	// whatever the shared active-tab index resolves to after the walk (the
	// index survives instance switches by design), so assert the instance
	// halves only.
	var view string
	eh.query(func(h *home) { view = h.View() })
	assert.Contains(t, view, "beta · ", "pane A header shows the selection")
	assert.Contains(t, view, "alpha · Preview", "pane B header shows the pinned binding")

	// Tab, Tab: tree → pane A → pane B.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.waitUntil(e2eAsyncTimeout, "the focus ring reaches pane B", func() bool {
		_, _, _, _, region := splitState()
		return region == layout.RegionPaneB
	})

	// s with pane B focused swaps the panes.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	eh.waitUntil(e2eAsyncTimeout, "s swaps A and B", func() bool {
		_, _, pinned, selected, _ := splitState()
		return selected == "alpha" && pinned == "beta"
	})

	// x closes the split; focus lands on pane A.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	eh.waitUntil(e2eAsyncTimeout, "x closes the split", func() bool {
		open, _, _, _, region := splitState()
		return !open && region == layout.RegionPaneA
	})
}

package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resizeHome drives the real WindowSizeMsg path.
func resizeHome(h *home, w, hgt int) {
	h.updateHandleWindowSizeEvent(tea.WindowSizeMsg{Width: w, Height: hgt})
}

// requireViewSized asserts the composed View is exactly the terminal size —
// the whole-window contract of the layout cutover (#1024 PR 4).
func requireViewSized(t *testing.T, view string, w, hgt int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	require.Equalf(t, hgt, len(lines), "View must render exactly %d lines", hgt)
	for i, line := range lines {
		require.Equalf(t, w, lipgloss.Width(line), "View line %d must be exactly %d cells", i, w)
	}
}

// TestLayoutCutover_ViewComposesFullWindow drives the root model through the
// real resize path at the RFC's key sizes and asserts the composed View is
// exactly terminal-sized with every region present: tree, workspace pane,
// automations strip, and status bar — the full-real-estate cutover.
func TestLayoutCutover_ViewComposesFullWindow(t *testing.T) {
	for _, tc := range []struct{ w, h int }{
		{120, 40},
		{80, 24},
	} {
		h := newTestHome(t)
		addTreeInstance(t, h, "alpha")
		h.sidebar.SetSelectedInstance(0)
		_ = h.selectionChanged()
		resizeHome(h, tc.w, tc.h)

		view := h.View()
		requireViewSized(t, view, tc.w, tc.h)
		assert.Contains(t, view, "Agent Factory", "%dx%d: tree title", tc.w, tc.h)
		assert.Contains(t, view, "alpha · Preview", "%dx%d: pane header carries title · tab", tc.w, tc.h)
		assert.Contains(t, view, "Automations", "%dx%d: automations strip", tc.w, tc.h)
		// The hint row prioritizes under width pressure (low-value hints are
		// dropped first), so help/quit must survive at BOTH sizes.
		assert.Contains(t, view, "n new", "%dx%d: status-bar hints", tc.w, tc.h)
		assert.Contains(t, view, "q quit", "%dx%d: quit hint must survive narrow widths", tc.w, tc.h)
		assert.Contains(t, view, "? help", "%dx%d: help hint must survive narrow widths", tc.w, tc.h)
	}
}

// TestLayoutCutover_FocusRingCycles pins the PR-4 focus model: Tab cycles
// tree → pane A → automations and back around; Shift-Tab reverses; the
// automations strip expands in place while focused (grid re-solve) and its
// task manager holds input focus; the status-bar hints follow.
func TestLayoutCutover_FocusRingCycles(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 100, 30)
	require.Equal(t, layout.RegionTree, h.ring.Active(), "focus starts on the tree")
	require.True(t, h.sidebar.Focused())

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
	assert.Equal(t, layout.RegionPaneA, h.ring.Active())
	assert.True(t, h.paneA.Focused())
	assert.False(t, h.sidebar.Focused())

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
	assert.Equal(t, layout.RegionAutomations, h.ring.Active())
	assert.True(t, h.automations.Focused())
	assert.True(t, h.automations.TaskPane().HasFocus(),
		"focusing the strip forwards input focus to the task manager")
	assert.True(t, h.lastLayout.AutomationsExpanded,
		"the strip expands in place while focused")

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
	assert.Equal(t, layout.RegionTree, h.ring.Active(), "the ring wraps around")
	assert.False(t, h.lastLayout.AutomationsExpanded, "the strip contracts on blur")

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab}, keys.KeyShiftTab)
	assert.Equal(t, layout.RegionAutomations, h.ring.Active(), "Shift-Tab cycles backwards")
}

// TestLayoutCutover_AutomationsEscReturnsFocusToTree: Esc inside the focused
// strip releases the manager's input focus, and the root moves the ring back
// to the tree (the pre-cutover "esc back" flow re-homed).
func TestLayoutCutover_AutomationsEscReturnsFocusToTree(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 100, 30)
	h.focusRegion(layout.RegionAutomations)
	require.True(t, h.automations.TaskPane().HasFocus())

	_, _, consumed := h.handleAutomationsFocus(tea.KeyMsg{Type: tea.KeyEsc})
	require.True(t, consumed)
	assert.Equal(t, layout.RegionTree, h.ring.Active(), "Esc returns focus to the tree")
	assert.False(t, h.automations.Focused())
}

// TestLayoutCutover_DegradationLadder drives the resize path down the §2.6
// ladder: <80 cols the strip becomes a 1-line summary; <60×15 it disappears
// and the ring skips it; below 40×10 the whole window is the fallback banner.
func TestLayoutCutover_DegradationLadder(t *testing.T) {
	h := newTestHome(t)

	resizeHome(h, 79, 24)
	require.True(t, h.lastLayout.AutomationsVisible)
	assert.True(t, h.lastLayout.AutomationsCompact, "<80 cols: 1-line automations summary")
	assert.Equal(t, 1, h.lastLayout.Automations.H)

	resizeHome(h, 59, 14)
	assert.False(t, h.lastLayout.AutomationsVisible, "minimal mode drops the strip")
	requireViewSized(t, h.View(), 59, 14)
	// The ring must skip the hidden strip: tree → paneA → tree.
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
	assert.Equal(t, layout.RegionPaneA, h.ring.Active())
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
	assert.Equal(t, layout.RegionTree, h.ring.Active(),
		"the hidden automations strip is skipped by the focus ring")

	resizeHome(h, 39, 9)
	require.True(t, h.lastLayout.Fallback)
	view := h.View()
	requireViewSized(t, view, 39, 9)
	assert.Contains(t, view, "Terminal too small", "below hard minimum: the fallback banner")

	// Growing back restores the full layout.
	resizeHome(h, 100, 30)
	assert.False(t, h.lastLayout.Fallback)
	assert.True(t, h.lastLayout.AutomationsVisible)
	assert.False(t, h.lastLayout.AutomationsCompact)
}

// TestLayoutCutover_HooksOverlay: H opens the hooks editor as a modal overlay
// (#1024 PR 4 — hooks lost their persistent sidebar slot); Esc closes it and
// returns to the workspace.
func TestLayoutCutover_HooksOverlay(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 100, 30)
	h.hooksPane.SetCommands([]string{"make setup"})

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("H")}, keys.KeyHooks)
	require.Equal(t, stateHooks, h.state)
	require.True(t, h.hooksPane.HasFocus(), "the editor opens with input focus")

	view := h.View()
	requireViewSized(t, view, 100, 30)
	assert.Contains(t, view, "Post-Worktree Hooks", "the overlay hosts the existing editor")
	assert.Contains(t, view, "make setup")

	_, _ = h.handleStateHooks(tea.KeyMsg{Type: tea.KeyEsc})
	assert.Equal(t, stateDefault, h.state, "Esc closes the overlay")
	assert.NotContains(t, h.View(), "Post-Worktree Hooks")
}

// TestLayoutCutover_TaskKeysFocusStrip: S focuses the strip's manager, and
// task creation lives on the manager's own `n` key — since #1024 PR 5 the
// global `s` is the split verb, so the strip's create flow must be fully
// reachable without it.
func TestLayoutCutover_TaskKeysFocusStrip(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 100, 30)

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")}, keys.KeyTaskList)
	assert.Equal(t, layout.RegionAutomations, h.ring.Active())
	assert.True(t, h.automations.TaskPane().HasFocus())
	assert.False(t, h.automations.TaskPane().IsCreating())

	_, _, consumed := h.handleAutomationsFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	require.True(t, consumed)
	assert.True(t, h.automations.TaskPane().IsCreating(), "n inside the focused strip opens the create form")
}

// TestE2E_LayoutCutover_FocusRingAndHooksOverlay drives the real tea.Program
// through the new workspace: Tab cycles the focus ring end to end (through
// handleMenuHighlighting's re-emit path), the automations strip expands while
// focused, Esc returns to the tree, and H opens/closes the hooks overlay.
func TestE2E_LayoutCutover_FocusRingAndHooksOverlay(t *testing.T) {
	eh := newE2EHarness(t)
	eh.addStartedInstance("alpha")
	eh.home.sidebar.SetSelectedInstance(0)
	eh.start()

	activeRegion := func() string {
		var r string
		eh.query(func(h *home) { r = h.ring.Active() })
		return r
	}

	require.Equal(t, layout.RegionTree, activeRegion(), "focus starts on the tree")

	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.waitUntil(e2eAsyncTimeout, "Tab moves focus to pane A", func() bool {
		return activeRegion() == layout.RegionPaneA
	})

	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.waitUntil(e2eAsyncTimeout, "Tab moves focus to the automations strip", func() bool {
		return activeRegion() == layout.RegionAutomations
	})
	var expanded, managerFocused bool
	eh.query(func(h *home) {
		expanded = h.lastLayout.AutomationsExpanded
		managerFocused = h.automations.TaskPane().HasFocus()
	})
	assert.True(t, expanded, "the strip expands in place while focused")
	assert.True(t, managerFocused, "the task manager takes input focus")

	// Esc inside the strip returns focus to the tree.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	eh.waitUntil(e2eAsyncTimeout, "Esc returns focus to the tree", func() bool {
		return activeRegion() == layout.RegionTree
	})

	// H opens the hooks overlay; Esc closes it.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'H'}})
	eh.waitUntil(e2eAsyncTimeout, "H opens the hooks overlay", func() bool {
		return eh.homeState() == stateHooks
	})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	eh.waitUntil(e2eAsyncTimeout, "Esc closes the hooks overlay", func() bool {
		return eh.homeState() == stateDefault
	})

	// The workspace still renders the instance afterwards.
	var view string
	eh.query(func(h *home) { view = h.View() })
	assert.Contains(t, view, "alpha · Preview")
}

// TestLayoutCutover_DigitJumpGatedByFocusRegion pins the 1-9 gate (Greptile on
// #1083): the tab jump belongs to the tree/workspace, so a digit pressed while
// the AUTOMATIONS strip has focus — including in plain list view, with no form
// open — must never retarget the content pane (the pre-cutover
// ContentModeTasks behavior). With focus back on the tree or pane A the jump
// works as before.
func TestLayoutCutover_DigitJumpGatedByFocusRegion(t *testing.T) {
	h := newTestHome(t)
	addTreeInstance(t, h, "alpha") // two default tab slots
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 100, 30)
	require.Equal(t, 0, h.store.ActiveTab())

	// Tree focused: digit jumps.
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	require.Equal(t, 1, h.store.ActiveTab(), "digit with tree focus jumps tabs")

	// Strip focused, LIST view (no form): digit must not fire a tab jump.
	h.focusRegion(layout.RegionAutomations)
	require.False(t, h.automations.TaskPane().IsCreating())
	require.False(t, h.automations.TaskPane().IsEditing())
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	require.Equal(t, 1, h.store.ActiveTab(),
		"a digit with the automations strip focused must not retarget the content pane")

	// Pane A focused: digit jumps again.
	h.focusRegion(layout.RegionPaneA)
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	require.Equal(t, 0, h.store.ActiveTab(), "digit with pane focus jumps tabs")
}

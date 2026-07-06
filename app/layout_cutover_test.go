package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/task"
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
		alpha := addTreeInstance(t, h, "alpha")
		h.sidebar.SetSelectedInstance(0)
		_ = h.selectionChanged()
		resizeHome(h, tc.w, tc.h)
		openTestPane(t, h, alpha, 0)
		// The hint assertions below are the tree/instance set; opening the
		// pane moved focus to it, so hand focus back to the tree.
		h.focusRegion(layout.RegionTree)

		view := h.View()
		requireViewSized(t, view, tc.w, tc.h)
		assert.Contains(t, view, "Agent Factory", "%dx%d: tree title", tc.w, tc.h)
		assert.Contains(t, view, "alpha · Preview", "%dx%d: pane header carries title · tab", tc.w, tc.h)
		// The header ellipsizes at the narrowest rails, so assert the stable
		// prefix plus the manage affordance FIX 2 guarantees is never cut.
		assert.Contains(t, view, "Automation", "%dx%d: automations section", tc.w, tc.h)
		assert.Contains(t, view, "S manage", "%dx%d: the manage affordance survives", tc.w, tc.h)
		// #1087: the automations section lives inside the left rail, under a
		// full-rail-width horizontal rule.
		assert.Contains(t, view, strings.Repeat("─", h.lastLayout.RailRule.W),
			"%dx%d: rail rule spans the full rail width", tc.w, tc.h)
		assert.Equal(t, h.lastLayout.Tree.W, h.lastLayout.Automations.W,
			"%dx%d: automations render at rail width", tc.w, tc.h)
		assert.Equal(t, tc.h-layout.StatusBarRows, h.lastLayout.Panes[0].H,
			"%dx%d: the pane takes the full height above the status bar", tc.w, tc.h)
		// The hint row prioritizes under width pressure (low-value hints are
		// dropped first), so help/quit must survive at BOTH sizes.
		assert.Contains(t, view, "n new", "%dx%d: status-bar hints", tc.w, tc.h)
		assert.Contains(t, view, "q quit", "%dx%d: quit hint must survive narrow widths", tc.w, tc.h)
		assert.Contains(t, view, "? help", "%dx%d: help hint must survive narrow widths", tc.w, tc.h)
	}
}

// TestLayoutCutover_FocusRingCycles pins the focus model: Tab cycles
// tree → pane A → automations and back around; Shift-Tab reverses; the
// focused in-rail automations section shows its cursor but never hosts the
// task manager (that is the tasks overlay — #1096 play-test); the status-bar
// hints follow.
func TestLayoutCutover_FocusRingCycles(t *testing.T) {
	h := newTestHome(t)
	alpha := addTreeInstance(t, h, "alpha")
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 100, 30)
	p := openTestPane(t, h, alpha, 0)
	h.focusRegion(layout.RegionTree)
	require.Equal(t, layout.RegionTree, h.ring.Active(), "focus starts on the tree")
	require.True(t, h.sidebar.Focused())

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
	assert.Equal(t, layout.PaneRegion(p.ID()), h.ring.Active())
	assert.True(t, h.paneWindows[p.ID()].Focused())
	assert.False(t, h.sidebar.Focused())

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
	assert.Equal(t, layout.RegionAutomations, h.ring.Active())
	assert.True(t, h.automations.Focused())
	assert.False(t, h.automations.TaskPane().HasFocus(),
		"focusing the section must not focus the manager — Enter/S open it as an overlay")
	assert.Equal(t, layout.AutomationsRows, h.lastLayout.Automations.H,
		"the focused section keeps its compact allocation — no in-rail expansion")

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
	assert.Equal(t, layout.RegionTree, h.ring.Active(), "the ring wraps around")

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab}, keys.KeyShiftTab)
	assert.Equal(t, layout.RegionAutomations, h.ring.Active(), "Shift-Tab cycles backwards")
}

// TestLayoutCutover_AutomationsEscReturnsFocusToTree: Esc on the focused
// section moves the ring back to the tree (the pre-cutover "esc back" flow
// re-homed).
func TestLayoutCutover_AutomationsEscReturnsFocusToTree(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 100, 30)
	h.focusRegion(layout.RegionAutomations)
	require.True(t, h.automations.Focused())

	_, _, consumed := h.handleAutomationsFocus(tea.KeyMsg{Type: tea.KeyEsc})
	require.True(t, consumed)
	assert.Equal(t, layout.RegionTree, h.ring.Active(), "Esc returns focus to the tree")
	assert.False(t, h.automations.Focused())
}

// TestLayoutCutover_EnterOpensTasksOverlay: Enter on the focused in-rail
// section opens the task manager as a centered modal (#1096 play-test fix 1),
// preselecting the section cursor's task; Esc closes it and saving runs.
// TestLayoutCutover_EnterOpensTaskInEditMode is the #1249 guard: acting on a
// task once (Enter on the section cursor's task) drops straight into that
// task's editable config form — no second keypress to leave the list. Esc
// steps back out of the form to the list (overlay still open), and a second
// Esc closes the overlay.
func TestLayoutCutover_EnterOpensTaskInEditMode(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 100, 30)
	tasks := []task.Task{
		{ID: "1", Name: "alpha-task", CronExpr: "0 3 * * *", Enabled: true},
		{ID: "2", Name: "beta-task", CronExpr: "0 4 * * *", Enabled: true},
	}
	h.store.SetTasks(tasks)
	h.automations.TaskPane().SetTasks(tasks)

	h.focusRegion(layout.RegionAutomations)
	h.automations.ScrollDown() // cursor onto beta-task

	_, _, consumed := h.handleAutomationsFocus(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, consumed)
	require.Equal(t, stateTasks, h.state, "Enter opens the tasks overlay")
	require.True(t, h.automations.TaskPane().HasFocus(), "the manager opens with input focus")
	require.True(t, h.automations.TaskPane().IsEditing(),
		"acting on a task once lands directly in its config editor (#1249)")

	view := h.View()
	requireViewSized(t, view, 100, 30)
	assert.Contains(t, view, "Edit Task 2",
		"the overlay opens the cursor's task straight into its edit form")
	assert.Contains(t, view, "beta-task", "the form is prefilled with the selected task")

	// First Esc backs the form out to the list — the overlay stays open.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})
	assert.Equal(t, stateTasks, h.state, "Esc from the form returns to the list, not out")
	assert.False(t, h.automations.TaskPane().IsEditing(), "Esc leaves edit mode")
	assert.Contains(t, h.View(), "n new", "the list view (and its key line) is back")

	// Second Esc closes the overlay.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc})
	assert.Equal(t, stateDefault, h.state, "Esc from the list closes the overlay")
	assert.NotContains(t, h.View(), "n new", "the manager's key line leaves with the overlay")
}

// TestLayoutCutover_DegradationLadder drives the resize path down the §2.6
// ladder: <80 cols the strip becomes a 1-line summary; <60×15 it disappears
// and the ring skips it; below 40×10 the whole window is the fallback banner.
func TestLayoutCutover_DegradationLadder(t *testing.T) {
	h := newTestHome(t)
	alpha := addTreeInstance(t, h, "alpha")
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 100, 30)
	p := openTestPane(t, h, alpha, 0)
	h.focusRegion(layout.RegionTree)

	resizeHome(h, 79, 24)
	require.True(t, h.lastLayout.AutomationsVisible)
	assert.True(t, h.lastLayout.AutomationsCompact, "<80 cols: 1-line automations summary")
	assert.Equal(t, 1, h.lastLayout.Automations.H)

	resizeHome(h, 59, 14)
	assert.False(t, h.lastLayout.AutomationsVisible, "minimal mode drops the strip")
	requireViewSized(t, h.View(), 59, 14)
	// The ring must skip the hidden strip: tree → pane → tree.
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
	assert.Equal(t, layout.PaneRegion(p.ID()), h.ring.Active())
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

// TestLayoutCutover_TaskKeysOpenOverlay: S opens the task manager overlay,
// and task creation lives on the manager's own `n` key — the "S, then n"
// muscle memory survives the overlay move (#1096 play-test fix 1).
func TestLayoutCutover_TaskKeysOpenOverlay(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 100, 30)

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")}, keys.KeyTaskList)
	assert.Equal(t, stateTasks, h.state, "S opens the tasks overlay")
	assert.True(t, h.automations.TaskPane().HasFocus())
	assert.False(t, h.automations.TaskPane().IsCreating())

	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	assert.True(t, h.automations.TaskPane().IsCreating(), "n inside the overlay opens the create form")
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

	// s opens the selection as a pane and focuses it.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	eh.waitUntil(e2eAsyncTimeout, "s opens a focused pane", func() bool {
		return layout.IsPaneRegion(activeRegion())
	})

	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.waitUntil(e2eAsyncTimeout, "Tab moves focus to the automations section", func() bool {
		return activeRegion() == layout.RegionAutomations
	})
	var managerFocused bool
	eh.query(func(h *home) { managerFocused = h.automations.TaskPane().HasFocus() })
	assert.False(t, managerFocused,
		"focusing the section must not focus the manager — it opens as an overlay")

	// Enter opens the tasks overlay; Esc closes it (manager focus released).
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	eh.waitUntil(e2eAsyncTimeout, "Enter opens the tasks overlay", func() bool {
		return eh.homeState() == stateTasks
	})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	eh.waitUntil(e2eAsyncTimeout, "Esc closes the tasks overlay", func() bool {
		return eh.homeState() == stateDefault
	})

	// Esc on the section returns focus to the tree.
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
	addTreeInstance(t, h, "alpha") // real agent + shell tab pair
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
		"a digit with the automations strip focused must not retarget the selection")

	// A workspace pane focused: digit jumps the pane binding without
	// retargeting the sidebar selection's active tab.
	alpha := h.store.GetSelectedInstance()
	p := openTestPane(t, h, alpha, 1)
	require.True(t, layout.IsPaneRegion(h.ring.Active()))
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	require.Equal(t, 1, h.store.ActiveTab(), "digit with pane focus must not retarget the selection")
	require.Equal(t, 0, p.Tab(), "digit with pane focus jumps the focused pane's tab")
}

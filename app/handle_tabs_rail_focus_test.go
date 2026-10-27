package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// Tab close (w) / new tab (t) under focused rail ownership (#1417 leak class).
//
// `w` and `t` route through the global key map to handleCloseTab and
// showNewTabPicker. Both resolve their target from state that is NOT the
// focused rail — the store's sticky last tree selection (w) and the sidebar
// cursor's row (t) — so before the focus gate a `w`/`t` pressed while the
// Automations or Projects rail owned the keyboard fell through the dispatcher
// and opened a delete-tab confirmation / new-tab picker against a session the
// rail does not display (`w` is the most destructive verb of that class). The
// handlers now gate on a workspace region — exactly as showRenameTabPrompt and
// showJumpTabPrompt do (#3067) — and the pair is also on the Automations
// suppression list, mirroring the pane verbs and </> (#1417). A rail that owns
// the keyboard keeps it.
// ----------------------------------------------------------------------------

// railHome builds a home with both rail sections populated (tasks + projects),
// a 4-tab fake-backend instance, and an open workspace pane, sized so the rails
// render and are focusable. The pane is required for the layout to allocate the
// rail column; focus is left on the tree. tabIDForSlot gives every tab a stable
// id, the shape the rail-focus assertions (tabNames) read.
func railHome(t *testing.T) (*home, *session.Instance) {
	t.Helper()
	h := newTestHome(t)
	h.store.SetTasks([]task.Task{{ID: "rail-task", Name: "watch"}})
	h.projects.SetProjects([]ui.SidebarProject{
		{Name: "project", Root: "/project"},
		{Name: "tools", Root: "/tools"},
	})
	alpha := instanceWithFakeBackend(t, "alpha")
	alpha.AddTabForTest("agent", session.TabKindAgent)
	alpha.AddTabForTest("shell", session.TabKindShell)
	alpha.AddTabForTest("shell-2", session.TabKindShell)
	alpha.AddTabForTest("shell-3", session.TabKindShell)
	for i, tab := range alpha.GetTabs() {
		tab.ID = tabIDForSlot(i)
	}
	h.store.AddInstance(alpha)
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 100, 30)
	openTestPane(t, h, alpha, 0)
	h.focusRegion(layout.RegionTree)
	return h, alpha
}

// assertRosterUntouched fails if alpha's 4-tab fixture changed.
func assertRosterUntouched(t *testing.T, alpha *session.Instance) {
	t.Helper()
	require.Equal(t, 4, alpha.TabCount(), "a rail-focused w/t must not touch the roster")
	require.Equal(t, []string{"agent", "shell", "shell-2", "shell-3"}, tabNames(alpha))
}

// TestCloseTab_AutomationsFocusInert is the headline regression: with the
// Automations rail focused, `w` driven through the real handleKeyPress dispatch
// must NOT open the delete-tab confirmation against the store's sticky last
// tree selection. The rail's footer does not advertise w; the key must stay
// inert there (#1417).
func TestCloseTab_AutomationsFocusInert(t *testing.T) {
	h, alpha := railHome(t)
	h.store.SetActiveTab(2) // a NON-zero tab so a swallowed close is detectable
	h.focusRegion(layout.RegionAutomations)
	require.True(t, h.automations.Focused(), "precondition: the automations rail holds focus")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})

	require.Equal(t, stateDefault, h.state,
		"w under automations focus must not open the delete-tab confirmation")
	require.Nil(t, h.confirmationOverlay, "no confirmation overlay under rail focus")
	require.Equal(t, layout.RegionAutomations, h.ring.Active(),
		"w must not move focus away from the automations rail")
	assertRosterUntouched(t, alpha)
}

// TestNewTab_AutomationsFocusInert is the headline regression for `t`: with the
// Automations rail focused, `t` driven through handleKeyPress must NOT open the
// new-tab picker targeting the sidebar cursor's row.
func TestNewTab_AutomationsFocusInert(t *testing.T) {
	h, alpha := railHome(t)
	h.store.SetActiveTab(2)
	h.focusRegion(layout.RegionAutomations)
	require.True(t, h.automations.Focused(), "precondition: the automations rail holds focus")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})

	require.Equal(t, stateDefault, h.state,
		"t under automations focus must not open the new-tab picker")
	require.Nil(t, h.selectionOverlay, "no picker overlay under rail focus")
	require.Equal(t, layout.RegionAutomations, h.ring.Active(),
		"t must not move focus away from the automations rail")
	assertRosterUntouched(t, alpha)
}

// TestCloseTab_AutomationsFocusHandlerRefuses proves the gate lives on the
// HANDLER, not only on the dispatch suppression list: calling handleCloseTab
// directly with the rail focused still refuses. This protects any future call
// site, not just the handleKeyPress path.
func TestCloseTab_AutomationsFocusHandlerRefuses(t *testing.T) {
	h, alpha := railHome(t)
	h.store.SetActiveTab(2)
	h.focusRegion(layout.RegionAutomations)
	require.True(t, h.automations.Focused())

	_, _ = h.handleCloseTab()

	require.Equal(t, stateDefault, h.state, "the handler must refuse under rail focus")
	require.Nil(t, h.confirmationOverlay)
	assertRosterUntouched(t, alpha)
}

// TestNewTab_AutomationsFocusHandlerRefuses is the handler-gate half for `t`.
func TestNewTab_AutomationsFocusHandlerRefuses(t *testing.T) {
	h, alpha := railHome(t)
	h.store.SetActiveTab(2)
	h.focusRegion(layout.RegionAutomations)
	require.True(t, h.automations.Focused())

	_, _ = h.showNewTabPicker()

	require.Equal(t, stateDefault, h.state, "the handler must refuse under rail focus")
	require.Nil(t, h.selectionOverlay)
	assertRosterUntouched(t, alpha)
}

// TestCloseTab_ProjectsFocusInert pins the captive Projects rail (a peer of the
// Automations rail): the handler gate refuses there too, and the rail's own
// blanket no-op already consumes the key. Driven through handleKeyPress.
func TestCloseTab_ProjectsFocusInert(t *testing.T) {
	h, alpha := railHome(t)
	h.store.SetActiveTab(2)
	h.focusRegion(layout.RegionProjects)
	require.True(t, h.projects.Focused(), "precondition: the projects rail holds focus")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})

	require.Equal(t, stateDefault, h.state,
		"w under projects focus must not open the delete-tab confirmation")
	require.Nil(t, h.confirmationOverlay)
	require.Equal(t, layout.RegionProjects, h.ring.Active(), "w must not move focus")
	assertRosterUntouched(t, alpha)
}

// TestNewTab_ProjectsFocusInert is the `t` half for the Projects rail.
func TestNewTab_ProjectsFocusInert(t *testing.T) {
	h, alpha := railHome(t)
	h.store.SetActiveTab(2)
	h.focusRegion(layout.RegionProjects)
	require.True(t, h.projects.Focused(), "precondition: the projects rail holds focus")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})

	require.Equal(t, stateDefault, h.state,
		"t under projects focus must not open the new-tab picker")
	require.Nil(t, h.selectionOverlay)
	require.Equal(t, layout.RegionProjects, h.ring.Active(), "t must not move focus")
	assertRosterUntouched(t, alpha)
}

// TestCloseTab_ProjectsFocusHandlerRefuses proves the handler gate defends the
// Projects rail too — not only the rail's own captive no-op. The Projects rail
// already swallows the key at dispatch; this asserts the handler itself refuses
// if reached some other way, so the gate is defense-in-depth for both rails.
func TestCloseTab_ProjectsFocusHandlerRefuses(t *testing.T) {
	h, alpha := railHome(t)
	h.store.SetActiveTab(2)
	h.focusRegion(layout.RegionProjects)
	require.True(t, h.projects.Focused())

	_, _ = h.handleCloseTab()

	require.Equal(t, stateDefault, h.state, "the handler must refuse under projects focus")
	require.Nil(t, h.confirmationOverlay)
	assertRosterUntouched(t, alpha)
}

// TestNewTab_ProjectsFocusHandlerRefuses is the handler-gate half for `t` under
// the Projects rail.
func TestNewTab_ProjectsFocusHandlerRefuses(t *testing.T) {
	h, alpha := railHome(t)
	h.store.SetActiveTab(2)
	h.focusRegion(layout.RegionProjects)
	require.True(t, h.projects.Focused())

	_, _ = h.showNewTabPicker()

	require.Equal(t, stateDefault, h.state, "the handler must refuse under projects focus")
	require.Nil(t, h.selectionOverlay)
	assertRosterUntouched(t, alpha)
}

// TestCloseTab_TreeFocusStillOpensConfirm is the no-regression control: with
// the tree focused, `w` still opens the delete-tab confirmation on the tree's
// active tab — the gate allows a workspace region through.
func TestCloseTab_TreeFocusStillOpensConfirm(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.store.SetActiveTab(2) // shell-2 (the confirmation quotes it as "3 Terminal")
	h.focusRegion(layout.RegionTree)
	require.Equal(t, layout.RegionTree, h.ring.Active())

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})

	require.Equal(t, stateConfirm, h.state, "w with tree focus must open the confirmation")
	require.NotNil(t, h.confirmationOverlay)
	// The quotation names the tab's TEXT, not its tree row: no "›" kind glyph
	// (#4630). The three shells share the text "Terminal", so the slot number
	// still tells them apart — "3 Terminal" is slot 2, the tree's active tab.
	require.Contains(t, h.confirmationOverlay.Render(), `Delete tab "3 Terminal"`,
		"the confirmation must target the tree's active tab")
	require.Equal(t, 4, alpha.TabCount(), "nothing is deleted before consent")
}

// TestNewTab_TreeFocusStillOpensPicker is the no-regression control for `t`.
func TestNewTab_TreeFocusStillOpensPicker(t *testing.T) {
	h, _ := multiTabHome(t)
	h.store.SetActiveTab(2)
	h.focusRegion(layout.RegionTree)
	require.Equal(t, layout.RegionTree, h.ring.Active())

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})

	require.Equal(t, stateSelectTabKind, h.state, "t with tree focus must open the picker")
	require.NotNil(t, h.selectionOverlay)
}

// TestCloseTab_PaneFocusStillOpensConfirm is the pane-focus control: a pane is
// a workspace region, so the gate allows the handler through, and `w` targets
// the FOCUSED PANE's tab (#1884) — not the tree's active tab.
func TestCloseTab_PaneFocusStillOpensConfirm(t *testing.T) {
	h, alpha := multiTabHome(t)
	h.store.SetActiveTab(1) // tree active = shell (slot 1, quoted as "2 Terminal")
	pane := openTestPane(t, h, alpha, 1)
	_, _ = h.handleTabJump(4) // pane jumps to shell-3 (slot 3, quoted as "4 Terminal"); tree stays on 1
	require.Equal(t, 3, pane.Tab())
	require.Equal(t, 1, h.store.ActiveTab())
	require.True(t, layout.IsPaneRegion(h.ring.Active()), "precondition: a pane holds focus")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})

	require.Equal(t, stateConfirm, h.state, "w with pane focus must open the confirmation")
	require.NotNil(t, h.confirmationOverlay)
	// The pane-focused close targets the pane's tab (shell-3 → "4 Terminal"),
	// not the tree's active tab (shell → "2 Terminal") — the #1884 rule. The
	// quotation is the tab's text without the "›" kind glyph (#4630); the slot
	// number is what tells the same-text shells apart.
	require.Contains(t, h.confirmationOverlay.Render(), `Delete tab "4 Terminal"`,
		"the confirmation must target the focused pane's tab, not the tree's (#1884)")
	require.Equal(t, 4, alpha.TabCount(), "nothing is deleted before consent")
}

// TestNewTab_PaneFocusStillOpensPicker is the pane-focus control for `t`.
func TestNewTab_PaneFocusStillOpensPicker(t *testing.T) {
	h, alpha := multiTabHome(t)
	openTestPane(t, h, alpha, 1)
	require.True(t, layout.IsPaneRegion(h.ring.Active()), "precondition: a pane holds focus")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})

	require.Equal(t, stateSelectTabKind, h.state, "t with pane focus must open the picker")
	require.NotNil(t, h.selectionOverlay)
}

// TestAutomationsFocusConsumesCloseNewTabVerbs pins the suppression-list half:
// handleAutomationsFocus must CONSUME w/t (return consumed=true) while the rail
// owns focus, so the keys never fall through to the global dispatcher. Mirrors
// the pane-verb coverage in TestLayoutCutover_AutomationsFocusConsumesHiddenPaneVerbs.
func TestAutomationsFocusConsumesCloseNewTabVerbs(t *testing.T) {
	h, _ := railHome(t)
	h.focusRegion(layout.RegionAutomations)
	require.True(t, h.automations.Focused())

	for _, k := range []string{"w", "t"} {
		_, _, consumed := h.handleAutomationsFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		require.True(t, consumed, "handleAutomationsFocus must consume %q under rail focus", k)
	}
	// Control: a chrome key the rail lets fall through (help) is NOT consumed
	// by the mutation-verb list, so the gate stays scoped to the mutating verbs.
	_, _, consumed := h.handleAutomationsFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	require.Equal(t, keys.KeyHelp, keys.GlobalKeyStringsMap["?"],
		"the help key remains mapped (sanity for the fall-through control)")
	require.False(t, consumed, "the help key must fall through, not be consumed as a mutation verb")
}

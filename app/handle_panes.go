package app

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"

	tea "github.com/charmbracelet/bubbletea"
)

// This file hosts the N-pane verbs (#1088, RFC §2.3, replacing the PR-5 A/B
// split): `s` opens the selected tab as a new vertical-split pane to the
// right of the existing panes (or focuses its pane when the tab is already
// open), `x` hides the focused pane back to the background. Hiding never
// kills anything — the tab keeps running in its tmux session and reopens from
// the tree any time. There is no pinned/primary pane distinction: every pane
// is an explicit (instance, tab) binding in the store's open-pane list.

// openPaneWindow appends a pane bound to (instance, tab) to the store's
// open-pane list and creates its content window. Callers dedupe via
// FindOpenPane first and relayout afterwards.
func (m *home) openPaneWindow(instance *session.Instance, tab int) *store.OpenPane {
	p := m.store.AddOpenPane(instance, tab)
	if p == nil {
		return nil
	}
	w := ui.NewTabbedWindow(ui.NewTabPane(), p)
	// Wire the pane's mouse identity (#1024 R4): its zone ids are keyed by
	// the same region id the focus ring and layout use, stable for the
	// window's life.
	w.SetRegion(layout.PaneRegion(p.ID()))
	w.SetZoneRegistry(m.zones)
	m.paneWindows[p.ID()] = w
	return p
}

// focusedOpenPane returns the open pane the focus ring points at, or nil when
// focus is on the tree/automations (or the pane vanished).
func (m *home) focusedOpenPane() *store.OpenPane {
	active := m.ring.Active()
	if !layout.IsPaneRegion(active) {
		return nil
	}
	for _, p := range m.visiblePanes {
		if layout.PaneRegion(p.ID()) == active {
			return p
		}
	}
	return nil
}

// paneSelectionHint names the current tree/sidebar selection when a visible
// pane is bound elsewhere. Panes are explicit workspace bindings, not
// selection-driven previews; showing the selected title in the header makes
// that divergence visible instead of letting the tree highlight and pane
// content appear to disagree (#1289).
func (m *home) paneSelectionHint(p *store.OpenPane) string {
	if p == nil {
		return ""
	}
	selected := m.store.GetSelectedInstance()
	if selected == nil {
		return ""
	}
	if p.Instance() == selected && p.Tab() == m.store.ActiveTab() {
		return ""
	}
	tabLabel := ""
	labels := tree.TabLabels(selected)
	activeTab := m.store.ActiveTab()
	if activeTab >= 0 && activeTab < len(labels) {
		tabLabel = labels[activeTab]
	}
	if tabLabel == "" {
		return selected.Title
	}
	return fmt.Sprintf("%s · %s", selected.Title, tabLabel)
}

// handleOpenPane dispatches the `s` key: open the tree selection's (instance,
// active tab) as a new pane — rightmost — or focus its pane when that tab is
// already open (§2.3). The verb reads the selection regardless of which
// region has focus, so `s` works identically from a tree row and from a
// focused pane. Opening one more pane than fits auto-hides the
// least-recently-focused pane (§2.6) — never an error.
func (m *home) handleOpenPane() (tea.Model, tea.Cmd) {
	selected := m.store.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	if selected.HasInFlightOp() {
		return m, nil
	}
	return m.openOrFocusPane(selected, m.store.ActiveTab())
}

// openOrFocusPane opens (instance, tab) as a new focused pane, or focuses the
// pane already bound to it. The new/refocused pane is stamped most recently
// focused, so the §2.6 fitting keeps it visible even at capacity.
func (m *home) openOrFocusPane(instance *session.Instance, tab int) (tea.Model, tea.Cmd) {
	m.cancelPanePreview(false)
	p := m.store.FindOpenPane(instance, tab)
	if p == nil {
		p = m.openPaneWindow(instance, tab)
		if p == nil {
			return m, nil
		}
	}
	m.store.TouchOpenPane(p)
	m.relayout()
	m.focusRegion(layout.PaneRegion(p.ID()))
	return m, m.selectionChanged()
}

// handleHidePane dispatches the `x` key: hide the FOCUSED pane back to the
// background (§2.3). The pane leaves the workspace and the remaining panes
// re-divide the width; the tab keeps running — nothing is killed (killing
// tabs stays `w`, killing instances stays `D`). With focus on the tree or
// the automations section `x` is a no-op.
func (m *home) handleHidePane() (tea.Model, tea.Cmd) {
	p := m.focusedOpenPane()
	if p == nil {
		return m, nil
	}
	m.hidePane(p)
	return m, m.selectionChanged()
}

// hidePane removes a pane from the workspace and lands focus on the pane
// that takes its slot (the same position, or the new last pane), falling
// back to the tree when it was the only one.
func (m *home) hidePane(p *store.OpenPane) {
	pos := -1
	for i, vis := range m.visiblePanes {
		if vis == p {
			pos = i
			break
		}
	}
	m.closePaneWindow(p)
	m.relayout()
	if pos >= 0 && len(m.visiblePanes) > 0 {
		if pos >= len(m.visiblePanes) {
			pos = len(m.visiblePanes) - 1
		}
		m.ring.Focus(layout.PaneRegion(m.visiblePanes[pos].ID()))
	} else {
		m.ring.Focus(layout.RegionTree)
	}
	m.syncFocus()
}

// closePaneWindow removes a pane from the open list and drops its window and
// throttle state — the one primitive every pane-closing path (hide verb,
// tab-kill rebind, snapshot reconcile, dead-instance prune) goes through.
// Callers relayout afterwards.
func (m *home) closePaneWindow(p *store.OpenPane) {
	if m.panePreviewTxn != nil && m.panePreviewTxn.ownerPaneID == p.ID() {
		m.cancelPanePreview(false)
	}
	// Release the live termpane attachment before its window goes away —
	// its (pane, window) binding is about to dangle (#1089).
	if p == m.livePane {
		m.closeLiveTermPane()
		// If the user was typing INTO that pane, the mode's premise just
		// left with it: drop to nav now rather than a tick later.
		m.enforceInteractiveInvariant()
	}
	m.store.CloseOpenPane(p)
	delete(m.paneWindows, p.ID())
	delete(m.lastPaneCapture, p.ID())
}

// pruneDeadPanes closes panes whose backing session can no longer render: the
// instance left the projection (killed here, or removed by an external kill the
// snapshot reconcile mirrored), OR the instance was archived (#1028). An
// archived session's tmux and worktree are torn down, so an open pane bound to
// it dangles on a dead session — the archived-row "no live panes" contract. The
// instance stays PRESENT in the projection when archived (it moves to the
// Archived folder), so the containment check alone would skip it; the status
// check closes it. Keying on status here (not just the finalize handler) covers
// EVERY archive path — the TUI `a` verb and a CLI `af sessions archive` mirrored
// by the reconcile — since panesRefresh runs this on selection changes and
// reconciles. Reports whether anything closed; callers relayout on true.
func (m *home) pruneDeadPanes() bool {
	pruned := false
	for _, p := range append([]*store.OpenPane(nil), m.store.OpenPanes()...) {
		inst := p.Instance()
		if !m.store.ContainsInstance(inst) || (inst != nil && inst.GetLiveness() == session.LiveArchived) {
			m.closePaneWindow(p)
			pruned = true
		}
	}
	return pruned
}

// paneTabNames captures an instance's tab-slot names before a tab-set change,
// for reconcilePanesForTabs. Within an instance the tab NAME is the tab's
// identity — the daemon's own tab reconcile (ReconcileTabsFromData) keys on
// it, and names are unique per instance.
func paneTabNames(instance *session.Instance) []string {
	tabs := instance.GetTabs()
	names := make([]string, len(tabs))
	for i, tab := range tabs {
		names[i] = tab.Name
	}
	return names
}

// reconcilePanesForTabs re-binds the instance's open panes after its tab set
// changed — the SHARED close/rebind semantics of the TUI `w` kill and the
// daemon snapshot reconcile (#960: tabs can change with no local action, so
// both paths must apply the same rules). oldNames is the slot→name list
// captured BEFORE the change. A pane whose tab vanished is closed — its
// session is gone, exactly like the TUI-kill case; a pane whose tab moved
// slots re-binds to the tab's new index so it keeps showing the SAME tab
// rather than a shifted neighbor. Slots beyond the old real-tab list (the
// default-padded label slots of a young instance) are left alone —
// ClampActiveTab keeps them in range. Reports whether any pane closed or
// re-bound; callers relayout on true so the focus ring and the §2.6
// pane-count fitting stay consistent.
func (m *home) reconcilePanesForTabs(instance *session.Instance, oldNames []string) bool {
	tabs := instance.GetTabs()
	newIdx := make(map[string]int, len(tabs))
	for i, tab := range tabs {
		newIdx[tab.Name] = i
	}
	changed := false
	for _, p := range append([]*store.OpenPane(nil), m.store.OpenPanes()...) {
		if p.Instance() != instance {
			continue
		}
		slot := p.Tab()
		if slot < 0 || slot >= len(oldNames) {
			continue
		}
		idx, ok := newIdx[oldNames[slot]]
		switch {
		case !ok:
			m.closePaneWindow(p)
			changed = true
		case idx != slot:
			if m.panePreviewTxn != nil && m.panePreviewTxn.ownerPaneID == p.ID() {
				m.cancelPanePreview(false)
			}
			p.SetTab(idx)
			changed = true
		}
	}
	return changed
}

// focusTreeForNav returns the focus ring to the tree when a tree-navigation
// key (Up/Down, section jumps, collapse/expand) is pressed while a pane holds
// focus. Those keys move the SIDEBAR cursor regardless of which region the ring
// points at, so leaving the ring on a stale pane desyncs the two: the
// full-screen attach verb `o` (handleAttach) reads the focus ring first and
// would keep attaching the previously-focused pane's instance instead of the
// just-selected one — the same wrong-target class as the #1233 Enter bug. Enter
// is context-dependent now: a focused pane owns Enter, while tree focus resolves
// the current selection. After Ctrl-] leaves interactive mode the ring stays on
// instance A's pane, so without this a user who navigates to instance B and
// presses `o` or Enter would target A. Re-homing the ring on the tree makes
// those verbs resolve the current selection fresh. No-op unless a pane is
// focused, so it never churns the tree/automations ring or the live attachment
// (which persists on its still-visible pane).
func (m *home) focusTreeForNav() {
	if layout.IsPaneRegion(m.ring.Active()) {
		m.focusRegion(layout.RegionTree)
	}
}

// enterPane enters interactive mode on a SPECIFIC pane — the keyboard
// focused-pane target and the mouse click-to-interact target (§2.5). Tree-focus
// Enter still resolves through the sidebar selection; callers that already have
// a pane binding enter that pane directly. Remote/non-embeddable panes fall
// back to the full-screen attach of the pane's tab, mirroring handleEnter's
// remote branch; guard errors surface the same way.
func (m *home) enterPane(p *store.OpenPane) (tea.Model, tea.Cmd) {
	if p == nil {
		return m, nil
	}
	if instErr := interactiveGuard(p.Instance()); instErr != nil {
		return m, m.handleError(instErr)
	}
	if p.Instance() == nil || p.Instance().IsCreating() {
		return m, nil
	}
	if liveSessionName(p.Instance(), p.Tab()) == "" {
		// Not embeddable (remote): the full-screen attach of this pane's tab.
		return m.handleEnterPane(p)
	}
	return m.requestInteractive(p)
}

// handleEnterPane attaches the focused pane's (instance, tab) full-screen:
// the Enter half of "attach the FOCUSED pane". It mirrors the tree path in
// handleEnter guard for guard — Loading/Deleting fences, the #935 dead-tmux
// error, the #716 capture-at-keypress discipline (binding + tab captured
// before the help overlay defers the attach), and the #889 remote-ness
// routing — but reads the pane's binding instead of the tree selection.
func (m *home) handleEnterPane(p *store.OpenPane) (tea.Model, tea.Cmd) {
	instance := p.Instance()
	if instance == nil || instance.IsCreating() {
		return m, nil
	}
	if instance.IsTearingDown() {
		return m, m.handleError(fmt.Errorf("session '%s' is being deleted", instance.Title))
	}
	if instance.GetLiveness() == session.LiveLost {
		return m, m.handleError(fmt.Errorf("session '%s' was lost — its tmux session is gone", instance.Title))
	}
	if !instance.TmuxAlive() {
		return m, m.handleError(fmt.Errorf("session '%s' is no longer running", instance.Title))
	}
	tabIdx := p.Tab()
	if tabIdx != 0 {
		return m.showHelpScreen(helpTypeInstanceAttach{}, func() tea.Cmd {
			return attachOverlayCallbackFn(m, instance.Title, "handleEnter-pane-terminal", "", instance.IsRemote(), func() (chan struct{}, error) {
				return ui.AttachTerminalTab(instance, tabIdx)
			})
		})
	}
	return m.showHelpScreen(helpTypeInstanceAttach{}, func() tea.Cmd {
		return attachOverlayCallbackFn(m, instance.Title, "handleEnter-pane", "", instance.IsRemote(), func() (chan struct{}, error) {
			return m.store.AttachInstance(instance)
		})
	})
}

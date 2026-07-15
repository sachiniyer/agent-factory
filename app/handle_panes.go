package app

import (
	"fmt"

	"github.com/charmbracelet/bubbles/key"
	"github.com/sachiniyer/agent-factory/keys"
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
// open), `S` commits an active #1321 preview alongside the owner pane, and
// `x` hides the focused pane back to the background. Hiding never kills
// anything — the tab keeps running in its tmux session and reopens from the
// tree any time. There is no pinned/primary pane distinction: every pane is
// an explicit (instance, tab) binding in the store's open-pane list.

// openPaneWindow appends a pane bound to (instance, tab) to the store's
// open-pane list and creates its content window. Callers relayout afterwards.
//
// The (instance, tab) → at-most-one-pane invariant is enforced HERE, at the
// single pane-creation chokepoint: opening a tab that already has a pane
// (visible or auto-hidden) returns the existing pane instead of appending a
// duplicate — the open-or-focus contract (#1493), now unconditional so the
// callers that skip the FindOpenPane pre-check (the startup auto-open, the
// started-session auto-open, the restore path) can never split one tab across
// two panes and render it twice (#1557).
func (m *home) openPaneWindow(instance *session.Instance, tab int) *store.OpenPane {
	if existing := m.store.FindOpenPane(instance, tab); existing != nil {
		return existing
	}
	p := m.store.AddOpenPane(instance, tab)
	if p == nil {
		return nil
	}
	w := ui.NewTabbedWindow(ui.NewTabPane(m.newTabPaneSource()), p)
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
	if m.paneMatchesSelection(p) {
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

func (m *home) paneMatchesSelection(p *store.OpenPane) bool {
	if p == nil {
		return false
	}
	selected := m.store.GetSelectedInstance()
	return selected != nil && p.Instance() == selected && p.Tab() == m.store.ActiveTab()
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
	m.focusOpenPane(p)
	selectionCmd := m.selectionChanged()
	// Consume AFTER selectionChanged so this catch-all drains an auto-hide
	// status produced by ANY relayout in this open-or-focus operation — the
	// focusOpenPane above or a preview relayout inside selectionChanged (#1685).
	statusCmd := m.consumePaneAutoHideStatus()
	return m, tea.Batch(selectionCmd, statusCmd)
}

// focusOpenPane stamps an existing pane as recently focused, makes it visible
// if pane-count fitting had hidden it, and moves the focus ring onto it. Its
// relayout can auto-hide a previously visible pane (§2.6 fitting), which sets a
// pending auto-hide status; callers MUST run consumePaneAutoHideStatus()
// afterwards to start that notice's 3s auto-clear timer (#1685). openOrFocusPane
// does so via its trailing consume; the mouse/preview path does so in
// updatePanePreview.
func (m *home) focusOpenPane(p *store.OpenPane) {
	if p == nil {
		return
	}
	m.store.TouchOpenPane(p)
	m.relayout()
	m.focusRegion(layout.PaneRegion(p.ID()))
}

// handleSplitPane dispatches the `S` key: commit the active preview alongside
// its owner pane. The owner returns to its original committed binding, then
// the preview target is opened as a new pane or, if already open, focused.
func (m *home) handleSplitPane() (tea.Model, tea.Cmd) {
	if m.panePreviewTxn == nil {
		return m, nil
	}
	return m, m.commitPanePreviewAlongside()
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
	var focusedAfter *store.OpenPane
	if pos >= 0 && len(m.visiblePanes) > 0 {
		if pos >= len(m.visiblePanes) {
			pos = len(m.visiblePanes) - 1
		}
		focusedAfter = m.visiblePanes[pos]
		m.ring.Focus(layout.PaneRegion(focusedAfter.ID()))
	} else {
		m.ring.Focus(layout.RegionTree)
	}
	m.suppressPanePreviewForSelection(focusedAfter)
	m.syncFocus()
}

// handlePaneFocusKey owns pane-local navigation shortcuts in nav mode. The
// default LEFT/RIGHT bindings intentionally overlap the tree's collapse/expand
// arrows; this handler runs only when the focus ring is on a workspace pane,
// so sidebar focus keeps the sidebar behavior and interactive mode forwards
// arrows to the agent before this path can run.
func (m *home) handlePaneFocusKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if m.focusedOpenPane() == nil {
		return m, nil, false
	}
	if paneFocusReservedKey(msg) {
		return m, nil, false
	}
	switch {
	case key.Matches(msg, keys.GlobalKeyBindings[keys.KeyPanePrev]):
		return m, m.focusAdjacentPane(-1), true
	case key.Matches(msg, keys.GlobalKeyBindings[keys.KeyPaneNext]):
		return m, m.focusAdjacentPane(1), true
	default:
		return m, nil, false
	}
}

func paneFocusReservedKey(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "ctrl+c", "enter", "tab", "shift+tab", "esc", "ctrl+]":
		return true
	}
	if len(msg.Runes) == 1 {
		return msg.Runes[0] >= '1' && msg.Runes[0] <= '9'
	}
	return false
}

// focusAdjacentPane moves pane focus left/right in visible workspace order.
// Edges clamp: pressing previous on the leftmost pane or next on the rightmost
// pane is consumed but leaves focus where it is.
func (m *home) focusAdjacentPane(delta int) tea.Cmd {
	var refresh tea.Cmd
	if m.panePreviewTxn != nil {
		m.suppressActivePanePreview()
		m.cancelPanePreview(true)
		refresh = m.panesRefresh(m.attached.Load())
	}
	if delta == 0 || len(m.visiblePanes) < 2 {
		return refresh
	}
	current := m.focusedOpenPane()
	if current == nil {
		return refresh
	}
	pos := -1
	for i, p := range m.visiblePanes {
		if p == current {
			pos = i
			break
		}
	}
	if pos < 0 {
		return refresh
	}
	next := pos + delta
	if next < 0 {
		next = 0
	}
	if next >= len(m.visiblePanes) {
		next = len(m.visiblePanes) - 1
	}
	if next == pos {
		return refresh
	}
	m.focusRegion(layout.PaneRegion(m.visiblePanes[next].ID()))
	return refresh
}

// closePaneWindow removes a pane from the open list and drops its window and
// throttle state — the one primitive every pane-closing path (hide verb,
// tab-kill rebind, snapshot reconcile, dead-instance prune) goes through.
// Callers relayout afterwards.
func (m *home) closePaneWindow(p *store.OpenPane) {
	if m.panePreviewTxn != nil && m.panePreviewTxn.ownerPaneID == p.ID() {
		m.cancelPanePreview(false)
	}
	// Release the pane's live attachment before its window goes away — its (pane,
	// window) binding is about to dangle (#1089).
	m.closeLiveTermPaneFor(p.ID())
	// If the user was typing INTO that pane, the mode's premise just left with it:
	// drop to nav now rather than a tick later.
	m.enforceInteractiveInvariant()
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
// remote branch; guard errors surface the same way. A non-nil replayKey is the
// keystroke that triggered the entry and is forwarded into the pane once
// interactive mode is live (#1576) — the keyboard focused-pane Enter passes it
// so the first Enter reaches the agent; the mouse click passes nil (no
// keystroke to forward).
func (m *home) enterPane(p *store.OpenPane, replayKey *tea.KeyMsg) (tea.Model, tea.Cmd) {
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
	return m.requestInteractive(p, replayKey)
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
	if instance.GetInFlightOp() == session.OpRestoring {
		return m, m.handleError(fmt.Errorf("session '%s' is being restored", instance.Title))
	}
	if instance.GetLiveness() == session.LiveLost {
		return m, m.handleError(fmt.Errorf("session '%s' was lost — restore it first (af sessions restore %s)", instance.Title, instance.Title))
	}
	if instance.GetLiveness() == session.LiveDead {
		return m, m.handleError(fmt.Errorf("session '%s' is no longer running — restore it first (af sessions restore %s)", instance.Title, instance.Title))
	}
	if !instance.TmuxAlive() {
		return m, m.handleError(fmt.Errorf("session '%s' is no longer running", instance.Title))
	}
	// Fence attach off a browser-only tab HERE too. The tree-selection paths guard
	// on the SELECTED tab, but a web/vscode tab already open as a focused pane
	// reaches attach through here instead — and this pane's tab, not the selection,
	// is what gets attached. Without this, Enter/`o` on such a pane dials a PTY
	// stream for a tab that has none and surfaces the low-level attach failure
	// rather than the message telling the user where to actually view it.
	if err := webTabAttachGuard(instance, p.Tab()); err != nil {
		return m, m.handleError(err)
	}
	return m.attachInstanceTab(instance, p.Tab(), "handleEnter-pane", "handleEnter-pane-terminal")
}

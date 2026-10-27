package app

import (
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"
)

// updateHandleWindowSizeEvent records the terminal size and re-solves the
// layout.
func (m *home) updateHandleWindowSizeEvent(msg tea.WindowSizeMsg) tea.Cmd {
	m.termWidth = msg.Width
	m.termHeight = msg.Height
	m.relayout()
	return m.consumePaneAutoHideStatus()
}

// relayout is the single sizing path (#1024 PR 4): layout.Grid turns the
// terminal size into the region rects — applying the §2.6 degradation ladder
// and the #1088 pane-count fitting — and every pane is re-rected. Called on
// every WindowSizeMsg and whenever a grid input changes without a resize (a
// pane opening or closing).
func (m *home) relayout() {
	previousVisible := append([]*store.OpenPane(nil), m.visiblePanes...)
	// The grid is asked for every open pane; it honors at most MaxPanes of
	// them (§2.6). The store then picks WHICH panes stay visible — the
	// most-recently-focused ones, in workspace order — while the hidden
	// panes' bindings persist and restore on grow, which is exactly the
	// retain-and-restore contract the A/B split had.
	m.grid.Panes = m.store.NumOpenPanes()
	// Size the automations section to its content: the grid grows it to show
	// every automation when the rail has the room, collapsing only when the
	// tree + automations can't both fit (#1126).
	m.grid.Automations = m.store.NumTasks()
	// Size the Projects section to its content the same way (#1588 follow-up):
	// the grid grows it to show every project the rail has room for.
	m.grid.Projects = len(m.projects.Projects())
	// Reserve the alarm banner row exactly when a delivery-failure alarm is
	// raised (#1238), so the row appears/disappears with the alarm and never
	// steals space in the healthy steady state.
	m.grid.Banner = m.alarmBanner.Active()
	lay := m.grid.Solve(m.termWidth, m.termHeight)
	m.lastLayout = lay
	if lay.Fallback {
		// No rects to hand out, but keep the focus flags + hints coherent so
		// key routing stays correct while the terminal is too small.
		m.visiblePanes = nil
		m.syncFocus()
		return
	}

	// First real relayout after a restore: the restore-time relayout ran at
	// (0,0) and fell through to fallback with visiblePanes=nil, so the restored
	// panes are the visibility baseline this pass uses to detect a pane the
	// terminal can't fit (#1535). Clear it ONLY when it is actually consumed
	// (previousVisible empty): an intermediate relayout that reaches here with a
	// real previousVisible already established leaves nothing to consume, and one
	// that falls through to fallback returned above — so the baseline survives
	// every relayout until the first sized one uses it, instead of being cleared
	// out from under that resize (#1551 review). Since visiblePanes is nil until
	// the first non-fallback relayout, that first sized relayout is exactly the
	// one that consumes it.
	if len(previousVisible) == 0 && len(m.restoredPaneBaseline) > 0 {
		previousVisible = m.restoredPaneBaseline
		m.restoredPaneBaseline = nil
	}

	nextVisible := m.store.VisibleOpenPanes(lay.PaneCount())
	if hidden := newlyAutoHiddenPane(previousVisible, nextVisible, m.store.OpenPanes()); hidden != nil {
		m.setPaneAutoHideStatus(hidden, m.store.NumOpenPanes())
	}
	m.visiblePanes = nextVisible
	m.clearStaleAutoHideStatus()

	// Rebuild the ring's pane entries to the visible set (auto-hidden panes
	// leave the ring; the focused pane is most-recently-focused, so it is
	// never the one auto-hidden). SetIDs keeps the active id when it
	// survives; a vanished active falls back to the tree.
	ids := make([]string, 0, len(m.visiblePanes)+3)
	ids = append(ids, layout.RegionTree)
	for _, p := range m.visiblePanes {
		ids = append(ids, layout.PaneRegion(p.ID()))
	}
	// Projects follows automations so forward Tab is tree → panes → automations
	// → projects → (wrap) tree (#1588 follow-up).
	ids = append(ids, layout.RegionAutomations, layout.RegionProjects)
	m.ring.SetIDs(ids...)
	m.ring.SetHidden(layout.RegionAutomations, !lay.AutomationsVisible)
	m.ring.SetHidden(layout.RegionProjects, !lay.ProjectsVisible)
	m.applyPendingTUIViewFocus()
	m.syncFocus()

	m.sidebar.SetRect(lay.Tree)
	// The re-rect invalidated the sidebar's fitted window (SetSize drops
	// hasRendered), so a bound resting row may have crossed the viewport
	// fold with no cursor move and no row-list rebuild — a resize is one
	// trigger, but a pane opening or a rail section growing re-solves the
	// same rects. Re-resolve the footer's row-verb target NOW —
	// RowVerbTarget refits before answering — rather than let the stale
	// target (and its hint/click zones) ride until the next
	// selectionChanged up to ~100ms away (#5259). In every cursor position
	// the menu's target IS RowVerbTarget: the cursor's own row when it
	// rests on one, else the on-screen ▾-marked resting row its section
	// header adopts — the value selectionChanged writes in both branches.
	m.menu.SetInstance(m.sidebar.RowVerbTarget())
	visible := make(map[int]bool, len(m.visiblePanes))
	for i, p := range m.visiblePanes {
		visible[p.ID()] = true
		if w := m.paneWindows[p.ID()]; w != nil {
			w.SetRect(lay.Panes[i])
		}
	}
	// Auto-hidden panes render nothing while retaining their window state.
	for id, w := range m.paneWindows {
		if !visible[id] {
			w.SetRect(layout.Rect{})
		}
	}
	m.automations.SetRect(lay.Automations)
	m.automations.SetCompact(lay.AutomationsCompact)
	m.projects.SetRect(lay.Projects)
	m.projects.SetCompact(lay.ProjectsCompact)
	m.statusBar.SetRect(lay.StatusBar)
	m.alarmBanner.SetRect(lay.Banner)

	m.layoutModalOverlays()

	// Live panes report their view geometry through SetRect → w.live.Resize —
	// which is render-only unless the attachment currently OWNS the pane's size
	// (the focused interactive pane, #4480): only an owner's Resize reaches the
	// WS stream's last-resize-wins resize-window. The TUI no longer resizes
	// local tmux sessions from the relayout, and a passive viewer's layout churn
	// never does either.
}

func newlyAutoHiddenPane(previousVisible, nextVisible, openPanes []*store.OpenPane) *store.OpenPane {
	if len(previousVisible) == 0 || len(openPanes) <= len(nextVisible) {
		return nil
	}
	open := make(map[*store.OpenPane]bool, len(openPanes))
	for _, p := range openPanes {
		open[p] = true
	}
	visible := make(map[*store.OpenPane]bool, len(nextVisible))
	for _, p := range nextVisible {
		visible[p] = true
	}
	for _, p := range previousVisible {
		if p != nil && open[p] && !visible[p] {
			return p
		}
	}
	return nil
}

// setPaneAutoHideStatus reports the pane the terminal could not fit. It names
// the pane that was ACTUALLY displaced — `instance · tab`, the same identity the
// pane's own header shows — or says nothing about which pane when it cannot name
// one. The instance title alone is not a pane identity: since #930 an instance
// can own several panes, so "docs hidden" while a second `docs` pane is on
// screen tells the user something they can see is false (#1997).
//
// The bar truncates from the RIGHT, so the LAST fragment is the one that dies,
// and that fragment is the recovery hint — the `s` key, which is the cheaper of
// the two remedies because it needs no resize and is the one a user cannot
// discover by fiddling. #1973 bought room for it by dropping a word; #1997 then
// spent that room making the subject a full pane identity, and at 80 columns the
// line clipped to a dangling "resize wider or…" for even a five-character
// session name (#2580).
//
// So the reason clause is now the thing that pays: it says "too narrow" without
// counting the panes (the notice already names the one that went), which buys
// back enough cells for the hint to survive an ordinary title at 80 columns.
// Long titles still overflow — nothing fits an unbounded title — but the
// fragments stay ordered worst-first, so what survives is which pane went away,
// and since #2618 the clipped tail is readable in full with `E details`.
func (m *home) setPaneAutoHideStatus(p *store.OpenPane, paneCount int) {
	// Suppress the auto-hide notice during a config-agent spawn (handleConfigAgent).
	if p == nil || paneCount <= 1 || m.configAgentSpawning {
		return
	}
	subject := "a pane is hidden"
	if label, ok := paneStatusLabel(p); ok {
		subject = label + " hidden"
	}
	msg := fmt.Sprintf("%s — too narrow; resize%s", subject, paneRecoveryStatusHint())
	m.pendingPaneAutoHideStatus = msg
	m.paneAutoHideNoticeID = m.setTransientNotice(errors.New(msg))
}

// clearStaleAutoHideStatus drops the "N hidden: terminal too narrow" guidance
// the moment a relayout fits every open pane again — otherwise a resize wide
// enough to reveal the auto-hidden panes still leaves the narrow-width status
// on the bar, contradicting the now-visible panes (#1557). Keyed on the notice
// id so a newer, unrelated status that superseded ours is never wiped; the
// guidance's own transient timer still handles the case where it is the current
// notice but the panes never came back.
func (m *home) clearStaleAutoHideStatus() {
	if m.paneAutoHideNoticeID == 0 || m.store.NumOpenPanes() > len(m.visiblePanes) {
		return
	}
	if m.transientNoticeID == m.paneAutoHideNoticeID {
		m.errBox.Clear()
	}
	m.paneAutoHideNoticeID = 0
	m.pendingPaneAutoHideStatus = ""
}

// paneStatusLabel names a pane for a user-facing message the way the pane's own
// header names it — `instance · tab` (ui.TabbedWindow.renderHeader), reading the
// tab through the same disambiguated tree label source so the toast and the
// header can never disagree about what a pane is called.
//
// It reports false rather than guessing. An instance title alone is not a pane
// identity (#930), and tree.TabLabels answers with a placeholder "Agent" slot
// for an instance whose tabs have not materialized — so the tab is read through
// TabLabelAt, which distinguishes a real tab from "no tab list yet". A caller
// that cannot name the pane must say so instead of naming the wrong one (#1997).
func paneStatusLabel(p *store.OpenPane) (string, bool) {
	if p == nil || p.Instance() == nil || p.Instance().Title == "" {
		return "", false
	}
	label, ok := tree.TabLabelAt(p.Instance(), p.Tab())
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s · %s", p.Instance().Title, label), true
}

// paneRecoveryStatusHint names the key that brings the pane back without a
// resize. It drops the filler "use": every cell it costs comes out of its own
// survival at 80 columns, since it is the last fragment on a bar that truncates
// from the right (#2580).
func paneRecoveryStatusHint() string {
	if key := bindingKeyWithDesc("pane list"); key != "" {
		return fmt.Sprintf(" or `%s` pane list", key)
	}
	if binding, ok := keys.GlobalKeyBindings[keys.KeyOpenPane]; ok {
		help := binding.Help()
		if help.Key != "" && help.Desc != "" {
			return fmt.Sprintf(" or `%s` %s", help.Key, help.Desc)
		}
	}
	return ""
}

func bindingKeyWithDesc(desc string) string {
	for _, binding := range keys.GlobalKeyBindings {
		help := binding.Help()
		if help.Desc == desc && help.Key != "" {
			return help.Key
		}
	}
	return ""
}

func (m *home) consumePaneAutoHideStatus() tea.Cmd {
	if m.pendingPaneAutoHideStatus == "" {
		return nil
	}
	status := m.pendingPaneAutoHideStatus
	m.pendingPaneAutoHideStatus = ""
	m.paneAutoHideNoticeID = m.setTransientNotice(errors.New(status))
	return m.clearTransientMessageAfterDelay(m.paneAutoHideNoticeID)
}

package app

import (
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"
)

// The pane auto-hide status helpers, split out of home_model.go when it crossed
// the file-length limit (#1145): the cluster is self-contained — it names the
// pane a relayout displaced and keeps the status bar honest about it.

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
	if p == nil || paneCount <= 1 {
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

package app

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"

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
	m.paneWindows[p.ID()] = ui.NewTabbedWindow(ui.NewTabPane(), p)
	return p
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
	if status := selected.GetStatus(); status == session.Loading || status == session.Deleting {
		return m, nil
	}
	return m.openOrFocusPane(selected, m.store.ActiveTab())
}

// openOrFocusPane opens (instance, tab) as a new focused pane, or focuses the
// pane already bound to it. The new/refocused pane is stamped most recently
// focused, so the §2.6 fitting keeps it visible even at capacity.
func (m *home) openOrFocusPane(instance *session.Instance, tab int) (tea.Model, tea.Cmd) {
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
	m.store.CloseOpenPane(p)
	delete(m.paneWindows, p.ID())
	delete(m.lastPaneCapture, p.ID())
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

// handleEnterPane attaches the focused pane's (instance, tab) full-screen:
// the Enter half of "attach the FOCUSED pane". It mirrors the tree path in
// handleEnter guard for guard — Loading/Deleting fences, the #935 dead-tmux
// error, the #716 capture-at-keypress discipline (binding + tab captured
// before the help overlay defers the attach), and the #889 remote-ness
// routing — but reads the pane's binding instead of the tree selection.
func (m *home) handleEnterPane(p *store.OpenPane) (tea.Model, tea.Cmd) {
	instance := p.Instance()
	if instance == nil || instance.GetStatus() == session.Loading {
		return m, nil
	}
	if instance.GetStatus() == session.Deleting {
		return m, m.handleError(fmt.Errorf("session '%s' is being deleted", instance.Title))
	}
	if !instance.TmuxAlive() {
		return m, m.handleError(fmt.Errorf("session '%s' is no longer running", instance.Title))
	}
	tabIdx := p.Tab()
	if tabIdx != 0 {
		return m.showHelpScreen(helpTypeInstanceAttach{}, func() tea.Cmd {
			return attachOverlayCallbackFn(m, "handleEnter-pane-terminal", "", instance.IsRemote(), func() (chan struct{}, error) {
				return ui.AttachTerminalTab(instance, tabIdx)
			})
		})
	}
	return m.showHelpScreen(helpTypeInstanceAttach{}, func() tea.Cmd {
		return attachOverlayCallbackFn(m, "handleEnter-pane", "", instance.IsRemote(), func() (chan struct{}, error) {
			return m.store.AttachInstance(instance)
		})
	})
}

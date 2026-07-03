package app

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"

	tea "github.com/charmbracelet/bubbletea"
)

// This file hosts the split-view verbs (#1024 PR 5, RFC §2.3): `s` opens the
// tree selection in pane B or swaps the panes, `x`/`w` on pane B closes the
// split. Pane B is PINNED — its (instance, tab) binding lives in the store
// and only these verbs move it, while the tree selection keeps driving pane A
// live. The split is vertical (side-by-side) only.

// handleSplit dispatches the `s` key by focus region: tree focus opens (or
// retargets) the selection in pane B; pane focus swaps A↔B when a split is
// open, and pane A opens one when none is. The automations strip never
// reaches here in practice (its task manager consumes `s` while focused), but
// the region switch keeps that true by construction rather than by the
// strip's key table.
func (m *home) handleSplit() (tea.Model, tea.Cmd) {
	switch m.ring.Active() {
	case layout.RegionTree:
		return m.openSplitFromSelection()
	case layout.RegionPaneA:
		if m.lastLayout.SplitActive {
			return m.swapPanes()
		}
		return m.openSplitFromSelection()
	case layout.RegionPaneB:
		return m.swapPanes()
	}
	return m, nil
}

// openSplitFromSelection pins the current selection — the (instance, tab)
// pane A is showing — into pane B, opening the split (or retargeting an open
// one). The tree cursor is free to move on afterwards: pane B keeps this
// binding while the selection drives pane A.
func (m *home) openSplitFromSelection() (tea.Model, tea.Cmd) {
	selected := m.store.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	if status := selected.GetStatus(); status == session.Loading || status == session.Deleting {
		return m, nil
	}
	// Refuse a too-narrow `s` BEFORE touching the pane-B binding, on a trial
	// solve (§2.6: a fresh open that cannot show anything must not arm an
	// invisible pane). Deciding up front — rather than arming the binding and
	// rolling it back — is load-bearing when a collapsed-but-retained split
	// already exists: the rollback path cleared the RETAINED binding, so a
	// narrow `s` silently destroyed the split that should have restored on
	// grow (Greptile on #1085). An existing binding is left exactly as it is;
	// only the retarget/open is refused.
	trial := m.grid
	trial.Split = true
	if !trial.Solve(m.termWidth, m.termHeight).SplitActive {
		return m, m.handleError(fmt.Errorf("terminal too narrow for a split view (needs %d+ columns)", layout.SplitMinWidth))
	}
	m.store.SetPaneB(selected, m.store.ActiveTab())
	m.relayout()
	return m, m.selectionChanged()
}

// swapPanes exchanges the two panes' bindings: the tree selection (pane A)
// moves to pane B's pinned instance+tab, and pane B pins what pane A was
// showing. Focus stays on whichever region had it.
func (m *home) swapPanes() (tea.Model, tea.Cmd) {
	pinned := m.store.PaneBInstance()
	current := m.store.GetSelectedInstance()
	if pinned == nil || current == nil {
		return m, nil
	}
	currentTab := m.store.ActiveTab()
	pinnedTab := m.store.PaneBTab()

	m.store.SetPaneB(current, currentTab)
	// Pane A is selection-driven, so its half of the swap is a selection
	// move: re-pin the tree cursor onto the old pane-B instance (the cursor
	// lands on the instance row, so pushSelection leaves the active tab
	// alone), then set its tab.
	m.sidebar.SelectInstance(pinned)
	m.store.SetActiveTab(pinnedTab)
	m.paneA.ClampActiveTab()
	m.menu.SetActiveTab(m.paneA.GetActiveTab())
	return m, m.selectionChanged()
}

// closeSplit drops pane B's binding and re-solves the layout. Focus moves to
// pane A when it was on the closing pane, so the user's attention lands on
// the view that absorbed the workspace.
func (m *home) closeSplit() {
	if m.ring.Active() == layout.RegionPaneB {
		m.ring.Focus(layout.RegionPaneA)
	}
	m.store.ClearPaneB()
	m.relayout()
}

// handleCloseSplit is the `x` key: close the split when the pinned pane has
// focus. Anywhere else `x` stays a no-op (the RFC scopes the close verb to
// pane B).
func (m *home) handleCloseSplit() (tea.Model, tea.Cmd) {
	if m.ring.Active() != layout.RegionPaneB {
		return m, nil
	}
	m.closeSplit()
	return m, m.selectionChanged()
}

// handleEnterPaneB attaches pane B's pinned (instance, tab) full-screen: the
// Enter half of "attach the FOCUSED pane". It mirrors the pane-A path in
// handleEnter guard for guard — Loading/Deleting fences, the #935 dead-tmux
// error, the #716 capture-at-keypress discipline (binding + tab captured
// before the help overlay defers the attach), and the #889 remote-ness
// routing — but reads the pinned binding instead of the tree selection.
func (m *home) handleEnterPaneB() (tea.Model, tea.Cmd) {
	pinned := m.store.PaneBInstance()
	if pinned == nil || pinned.GetStatus() == session.Loading {
		return m, nil
	}
	if pinned.GetStatus() == session.Deleting {
		return m, m.handleError(fmt.Errorf("session '%s' is being deleted", pinned.Title))
	}
	if !pinned.TmuxAlive() {
		return m, m.handleError(fmt.Errorf("session '%s' is no longer running", pinned.Title))
	}
	tabIdx := m.store.PaneBTab()
	if tabIdx != 0 {
		return m.showHelpScreen(helpTypeInstanceAttach{}, func() tea.Cmd {
			return attachOverlayCallbackFn(m, "handleEnter-paneB-terminal", "", pinned.IsRemote(), func() (chan struct{}, error) {
				return m.paneB.AttachTerminalForInstance(pinned, tabIdx)
			})
		})
	}
	return m.showHelpScreen(helpTypeInstanceAttach{}, func() tea.Cmd {
		return attachOverlayCallbackFn(m, "handleEnter-paneB", "", pinned.IsRemote(), func() (chan struct{}, error) {
			return m.store.AttachInstance(pinned)
		})
	})
}

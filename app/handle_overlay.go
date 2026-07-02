package app

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"

	tea "github.com/charmbracelet/bubbletea"
)

// handleStateSelectProgram handles key events during program selection.
func (m *home) handleStateSelectProgram(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	shouldClose := m.selectionOverlay.HandleKeyPress(msg)
	if shouldClose {
		if m.selectionOverlay.IsSubmitted() {
			idx := m.selectionOverlay.GetSelectedIndex()
			m.pendingProgram = tmux.SupportedPrograms[idx]
		}
		m.selectionOverlay = nil
		m.state = stateNew
		m.menu.SetState(ui.StateNewInstance)
		return m, nil
	}
	return m, nil
}

// handleStateConfirm handles key events during confirmation dialogs.
func (m *home) handleStateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	shouldClose := m.confirmationOverlay.HandleKeyPress(msg)
	if shouldClose {
		if m.state == stateConfirm {
			m.state = stateDefault
		}
		m.confirmationOverlay = nil
		// Forward any non-error tea.Msg returned by the confirmation
		// action (e.g. instanceChangedMsg{}) back into the Bubble Tea
		// event loop so its handler can run (e.g. selectionChanged).
		if pending := m.pendingConfirmMsg; pending != nil {
			m.pendingConfirmMsg = nil
			return m, func() tea.Msg { return pending }
		}
		return m, nil
	}
	return m, nil
}

// handleStateSearch handles key events during session search.
func (m *home) handleStateSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	shouldClose := m.searchOverlay.HandleKeyPress(msg)
	if shouldClose {
		if m.searchOverlay.IsSubmitted() {
			if inst := m.searchOverlay.GetSelectedInstance(); inst != nil {
				m.sidebar.SelectInstance(inst)
			}
		}
		m.searchOverlay = nil
		m.state = stateDefault
		return m, tea.Sequence(tea.WindowSize(), m.selectionChanged())
	}
	return m, nil
}

// showSearchOverlay displays the session search overlay.
func (m *home) showSearchOverlay() (tea.Model, tea.Cmd) {
	// The overlay outlives this call and holds onto the slice, while a
	// background snapshotFetchedMsg reconcile can remove instances from the
	// projection in place (append-shift on the shared backing array). Hand the
	// overlay a stable copy so a later removal can't corrupt its list with
	// duplicate/ghost entries (#1008).
	instances := m.store.GetInstancesSnapshot()
	if len(instances) == 0 {
		return m, m.handleError(fmt.Errorf("no sessions to search"))
	}
	m.searchOverlay = overlay.NewSearchOverlay(instances)
	m.searchOverlay.SetWidth(60)
	m.state = stateSearch
	return m, nil
}

// handleContentPaneFocus routes key events to focused content pane and processes pending actions.
func (m *home) handleContentPaneFocus(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if !m.contentPane.HasFocus() {
		return m, nil, false
	}

	consumed := m.contentPane.HandleKeyPress(msg)
	if !consumed {
		return m, nil, false
	}

	// If focus was released (Esc), save state. A failed save reloads both panes
	// to match disk and is surfaced inline so the dropped edit isn't silent.
	if !m.contentPane.HasFocus() {
		if err := m.saveContentPaneState(); err != nil {
			return m, m.handleError(err), true
		}
	}

	// Check if a new task was submitted via the inline form
	sp := m.contentPane.TaskPane()
	if sp.HasPendingCreate() {
		// Submitting the create form sets pendingCreate without releasing
		// focus, so the "save on focus release" branch above doesn't run.
		// handleTaskCreate writes the new task to disk and then reloads
		// every task via SetTasks, which clears the dirty flag and any
		// unsaved toggle/edit/delete. Flush those changes first so the
		// reload picks them up (#578). If that flush fails, surface it and
		// skip the create: the pending toggle/edit didn't persist, so we
		// don't want handleTaskCreate's reload to silently discard it (#934).
		if err := m.saveContentPaneState(); err != nil {
			return m, m.handleError(err), true
		}
		return m, m.handleTaskCreate(), true
	}
	if sp.HasPendingTrigger() {
		return m, m.handleTaskTrigger(), true
	}

	return m, nil, true
}

// handleContentPaneEnter handles Enter/o key for focusing content panes (tasks/hooks).
func (m *home) handleContentPaneEnter(msg tea.KeyMsg, name keys.KeyName) (tea.Model, tea.Cmd, bool) {
	if name == keys.KeyEnter {
		mode := m.contentPane.GetMode()
		if mode == ui.ContentModeTasks || mode == ui.ContentModeHooks {
			consumed := m.contentPane.HandleKeyPress(msg)
			if consumed {
				return m, nil, true
			}
		}
	}

	return m, nil, false
}

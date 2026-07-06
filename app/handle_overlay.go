package app

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/overlay"

	"github.com/charmbracelet/bubbles/key"
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

// handleAutomationsFocus routes key events for the focused IN-RAIL automations
// section — which is only the compact summary since the #1096 play-test; the
// full manager lives in the tasks overlay. Cursor keys move the section's
// selection, Enter opens the manager overlay on it, Esc returns focus to the
// tree. Everything else falls through to the caller so the global bindings
// (Tab/Shift-Tab focus ring, S manage, H hooks, ? help, q quit) keep working.
func (m *home) handleAutomationsFocus(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if m.ring.Active() != layout.RegionAutomations {
		return m, nil, false
	}
	if _, consumed := m.automations.HandleKey(msg); consumed {
		return m, nil, true
	}
	switch msg.String() {
	case "enter":
		mod, cmd := m.showTasksOverlay()
		return mod, cmd, true
	case "esc":
		m.focusRegion(layout.RegionTree)
		return m, nil, true
	}
	return m, nil, false
}

// showTasksOverlay opens the task manager (list + create/edit form) as a
// centered modal overlay, preselecting the in-rail cursor's task and dropping
// straight into that task's editable config in a single action (#1249) — no
// second keypress to enter edit mode. Esc backs the form out to the list,
// where the rest of the key loop (j/k, n new, x toggle, D delete, r run, esc
// close) is live. When there are no tasks the overlay stays in list mode so
// `n` can create the first one.
func (m *home) showTasksOverlay() (tea.Model, tea.Cmd) {
	sp := m.automations.TaskPane()
	if idx := m.automations.SelectedTaskIndex(); idx >= 0 {
		sp.SelectTask(idx)
	}
	sp.SetFocus(true)
	sp.EnterEditSelected()
	m.state = stateTasks
	return m, nil
}

// handleStateTasks routes key events to the task manager overlay and
// processes its pending actions. Esc in list mode drops the manager's own
// focus: the overlay closes and any dirty edits are saved — a failed save
// reloads both views to match disk and is surfaced inline so the dropped
// edit isn't silent. q and ctrl+c are not consumed by the manager and quit.
func (m *home) handleStateTasks(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	sp := m.automations.TaskPane()
	consumed := sp.HandleKeyPress(msg)

	if !sp.HasFocus() {
		m.state = stateDefault
		if err := m.saveContentPaneState(); err != nil {
			return m, m.handleError(err)
		}
		return m, nil
	}

	if sp.HasPendingCreate() {
		// Submitting the create form sets pendingCreate without releasing
		// focus, so the save-on-close branch above doesn't run.
		// handleTaskCreate writes the new task to disk and then reloads
		// every task via SetTasks, which clears the dirty flag and any
		// unsaved toggle/edit/delete. Flush those changes first so the
		// reload picks them up (#578). If that flush fails, surface it and
		// skip the create: the pending toggle/edit didn't persist, so we
		// don't want handleTaskCreate's reload to silently discard it (#934).
		if err := m.saveContentPaneState(); err != nil {
			return m, m.handleError(err)
		}
		return m, m.handleTaskCreate()
	}
	if sp.HasPendingTrigger() {
		// The trigger closes the overlay (handleTaskTrigger lands focus on the
		// spawned instance); flush dirty toggles/edits first so they aren't
		// stranded in a closed overlay until quit.
		if err := m.saveContentPaneState(); err != nil {
			return m, m.handleError(err)
		}
		return m, m.handleTaskTrigger()
	}

	if !consumed {
		if msg.String() == "ctrl+c" || key.Matches(msg, keys.GlobalKeyBindings[keys.KeyQuit]) {
			return m.handleQuit()
		}
	}
	return m, nil
}

// showHooksOverlay opens the post-worktree hooks editor as a modal overlay
// (#1024 PR 4: hooks lost their persistent sidebar slot). The editor opens
// with input focus so its key loop (n add, enter edit, D delete, esc close)
// is live immediately.
func (m *home) showHooksOverlay() (tea.Model, tea.Cmd) {
	m.hooksPane.SetFocus(true)
	m.state = stateHooks
	return m, nil
}

// handleStateHooks routes key events to the hooks editor overlay. Esc closes
// the overlay (the pane drops its own focus) and persists any edits; q and
// ctrl+c are not consumed by the pane and quit, exactly as they did when the
// editor was hosted in the content pane.
func (m *home) handleStateHooks(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	consumed := m.hooksPane.HandleKeyPress(msg)
	if !m.hooksPane.HasFocus() {
		// The pane released focus (Esc): close the overlay and save. A failed
		// save keeps the pane dirty (#1001) and surfaces the error.
		m.state = stateDefault
		if err := m.saveContentPaneState(); err != nil {
			return m, m.handleError(err)
		}
		return m, nil
	}
	if !consumed {
		if msg.String() == "ctrl+c" || key.Matches(msg, keys.GlobalKeyBindings[keys.KeyQuit]) {
			return m.handleQuit()
		}
	}
	return m, nil
}

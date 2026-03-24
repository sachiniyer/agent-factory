package app

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/board"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"

	tea "github.com/charmbracelet/bubbletea"
)

// handleStateSelectWorktree handles key events during worktree selection.
func (m *home) handleStateSelectWorktree(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	shouldClose := m.selectionOverlay.HandleKeyPress(msg)
	if shouldClose {
		if m.selectionOverlay.IsSubmitted() {
			idx := m.selectionOverlay.GetSelectedIndex()
			wt := m.availableWorktrees[idx]
			m.selectedWorktree = &wt
			m.selectionOverlay = nil

			instance, err := session.NewInstance(session.InstanceOptions{
				Title:   "",
				Path:    ".",
				Program: m.program,
			})
			if err != nil {
				m.selectedWorktree = nil
				m.state = stateDefault
				m.menu.SetState(ui.StateDefault)
				return m, m.handleError(err)
			}

			instance.SetStatus(session.Loading)
			m.newInstanceFinalizer = m.sidebar.AddInstance(instance)
			m.sidebar.SetSelectedInstance(m.sidebar.NumInstances() - 1)
			m.namingInstance = instance
			m.state = stateNew
			m.menu.SetState(ui.StateNewInstance)
			return m, nil
		}
		m.selectionOverlay = nil
		m.selectedWorktree = nil
		m.availableWorktrees = nil
		m.state = stateDefault
		m.menu.SetState(ui.StateDefault)
		return m, nil
	}
	return m, nil
}

// handleStateLinkInstance handles key events during instance-to-task linking.
func (m *home) handleStateLinkInstance(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	shouldClose := m.selectionOverlay.HandleKeyPress(msg)
	if shouldClose {
		if m.selectionOverlay.IsSubmitted() {
			idx := m.selectionOverlay.GetSelectedIndex()
			instances := m.sidebar.GetInstances()
			if idx >= 0 && idx < len(instances) {
				inst := instances[idx]
				kp := m.contentPane.KanbanPane()
				if b := kp.GetBoard(); b != nil {
					b.LinkTask(m.linkingTaskID, inst.Title)
					kp.SetBoard(b) // refresh flat list
					if err := board.SaveBoard(b); err != nil {
						log.ErrorLog.Printf("failed to save board: %v", err)
					}
				}
			}
		}
		m.selectionOverlay = nil
		m.linkingTaskID = ""
		m.state = stateDefault
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

// showAttachWorktreeOverlay displays the worktree selection overlay.
func (m *home) showAttachWorktreeOverlay() (tea.Model, tea.Cmd) {
	worktrees, err := git.ListWorktrees(".")
	if err != nil {
		return m, m.handleError(fmt.Errorf("failed to list worktrees: %v", err))
	}
	if len(worktrees) == 0 {
		return m, m.handleError(fmt.Errorf("no worktrees found"))
	}

	trackedPaths := make(map[string]bool)
	for _, inst := range m.sidebar.GetInstances() {
		if p := inst.GetWorktreePath(); p != "" {
			trackedPaths[p] = true
		}
	}

	items := make([]string, len(worktrees))
	for i, wt := range worktrees {
		label := wt.Path
		if wt.Branch != "" {
			label = fmt.Sprintf("%s (%s)", wt.Branch, wt.Path)
		}
		if wt.IsMainWorktree {
			label += " [root]"
		}
		if trackedPaths[wt.Path] {
			label += " [has session]"
		}
		items[i] = label
	}

	m.availableWorktrees = worktrees
	m.selectionOverlay = overlay.NewSelectionOverlay("Attach to existing worktree", items)
	m.selectionOverlay.SetWidth(60)
	m.state = stateSelectWorktree
	return m, nil
}

// showSearchOverlay displays the session search overlay.
func (m *home) showSearchOverlay() (tea.Model, tea.Cmd) {
	instances := m.sidebar.GetInstances()
	if len(instances) == 0 {
		return m, m.handleError(fmt.Errorf("no sessions to search"))
	}
	m.searchOverlay = overlay.NewSearchOverlay(instances)
	m.searchOverlay.SetWidth(60)
	m.state = stateSearch
	return m, nil
}

// showLinkInstanceOverlay shows a selection overlay to pick an instance to link to a task.
func (m *home) showLinkInstanceOverlay(taskID string) tea.Cmd {
	instances := m.sidebar.GetInstances()
	if len(instances) == 0 {
		return m.handleError(fmt.Errorf("no sessions available to link"))
	}

	items := make([]string, len(instances))
	for i, inst := range instances {
		items[i] = inst.Title
	}

	m.linkingTaskID = taskID
	m.selectionOverlay = overlay.NewSelectionOverlay("Link task to session", items)
	m.selectionOverlay.SetWidth(60)
	m.state = stateLinkInstance
	return nil
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

	// If focus was released (Esc), save state
	if !m.contentPane.HasFocus() {
		m.saveContentPaneState()
	}

	// Check for pending jump/attach/link/status from kanban
	kp := m.contentPane.KanbanPane()
	if statusMsg := kp.ConsumeStatusMsg(); statusMsg != "" {
		return m, m.handleError(fmt.Errorf("%s", statusMsg)), true
	}
	if title := kp.ConsumePendingJump(); title != "" {
		return m, m.jumpToInstance(title), true
	}
	if title := kp.ConsumePendingAttach(); title != "" {
		mod, cmd := m.attachToInstance(title)
		return mod, cmd, true
	}
	if taskID := kp.ConsumePendingLink(); taskID != "" {
		return m, m.showLinkInstanceOverlay(taskID), true
	}
	if spawnTask := kp.ConsumePendingSpawn(); spawnTask != nil {
		return m, m.handleBoardSpawn(spawnTask), true
	}

	// Check if a new task was submitted via the inline form
	sp := m.contentPane.TaskPane()
	if sp.HasPendingCreate() {
		return m, m.handleTaskCreate(), true
	}
	if sp.HasPendingTrigger() {
		return m, m.handleTaskTrigger(), true
	}

	return m, nil, true
}

// handleContentPaneEnter handles Enter/o/a key for focusing content panes (board/tasks/hooks).
func (m *home) handleContentPaneEnter(msg tea.KeyMsg, name keys.KeyName) (tea.Model, tea.Cmd, bool) {
	if name == keys.KeyEnter {
		mode := m.contentPane.GetMode()
		if mode == ui.ContentModeBoard || mode == ui.ContentModeTasks || mode == ui.ContentModeHooks {
			consumed := m.contentPane.HandleKeyPress(msg)
			if consumed {
				return m, nil, true
			}
		}
	}

	// Route 'a' to board when viewing Board section (instead of worktree attach)
	if name == keys.KeyAttach && m.contentPane.GetMode() == ui.ContentModeBoard {
		consumed := m.contentPane.HandleKeyPress(msg)
		if consumed {
			return m, nil, true
		}
	}

	return m, nil, false
}

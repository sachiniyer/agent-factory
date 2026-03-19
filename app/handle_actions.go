package app

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/board"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// handleDefaultKeyPress handles key events in stateDefault (main interaction state).
func (m *home) handleDefaultKeyPress(msg tea.KeyMsg, name keys.KeyName) (tea.Model, tea.Cmd) {
	tw := m.contentPane.TabbedWindow()

	switch name {
	case keys.KeyHelp:
		return m.showHelpScreen(helpTypeGeneral{}, nil)

	// Sidebar navigation
	case keys.KeyUp:
		m.sidebar.Up()
		return m, m.selectionChanged()
	case keys.KeyDown:
		m.sidebar.Down()
		return m, m.selectionChanged()
	case keys.KeyLeft:
		m.sidebar.CollapseSection()
		return m, m.selectionChanged()
	case keys.KeyRight:
		m.sidebar.ExpandSection()
		return m, m.selectionChanged()
	case keys.KeyNextSection:
		m.sidebar.JumpNextSection()
		return m, m.selectionChanged()
	case keys.KeyPrevSection:
		m.sidebar.JumpPrevSection()
		return m, m.selectionChanged()

	// Instance creation
	case keys.KeyPrompt:
		return m.startNewInstance(true)

	case keys.KeyNew:
		// Context-aware: if on Tasks section, create a task instead
		if m.sidebar.GetSelection().Kind == ui.SectionTasks {
			cwd, err := os.Getwd()
			if err != nil {
				cwd = "."
			}
			m.contentPane.TaskPane().EnterCreateMode(cwd)
			m.contentPane.SetMode(ui.ContentModeTasks)
			return m, m.selectionChanged()
		}
		return m.startNewInstance(false)

	case keys.KeyTask:
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		m.contentPane.TaskPane().EnterCreateMode(cwd)
		m.navigateToSection(ui.SectionTasks)
		m.contentPane.SetMode(ui.ContentModeTasks)
		return m, m.selectionChanged()

	case keys.KeyTaskList:
		m.navigateToSection(ui.SectionTasks)
		return m, m.selectionChanged()

	case keys.KeyTriggerTask:
		if m.sidebar.GetSelection().Kind != ui.SectionTasks {
			return m, nil
		}
		sp := m.contentPane.TaskPane()
		if len(sp.GetTasks()) == 0 {
			return m, m.handleError(fmt.Errorf("no tasks to trigger"))
		}
		m.contentPane.SetMode(ui.ContentModeTasks)
		sp.SetFocus(true)
		sp.SetPendingTrigger()
		return m, m.handleTaskTrigger()

	case keys.KeyBoard:
		m.navigateToSection(ui.SectionBoard)
		return m, m.selectionChanged()

	case keys.KeySearch:
		return m.showSearchOverlay()

	case keys.KeyAttach:
		return m.showAttachWorktreeOverlay()

	// Hooks configuration
	case keys.KeyHooks:
		m.navigateToSection(ui.SectionHooks)
		return m, m.selectionChanged()

	// PR actions
	case keys.KeyOpenPR:
		return m.handleOpenPR()
	case keys.KeyCopyPR:
		return m.handleCopyPR()

	// Scrolling
	case keys.KeyShiftUp:
		m.contentPane.ScrollUp()
		return m, m.selectionChanged()
	case keys.KeyShiftDown:
		m.contentPane.ScrollDown()
		return m, m.selectionChanged()

	// Tab cycling (instance mode only)
	case keys.KeyTab:
		if m.contentPane.GetMode() == ui.ContentModeInstance {
			tw.Toggle()
			m.menu.SetActiveTab(tw.GetActiveTab())
			return m, m.selectionChanged()
		}
		return m, nil
	case keys.KeyShiftTab:
		if m.contentPane.GetMode() == ui.ContentModeInstance {
			tw.ToggleBack()
			m.menu.SetActiveTab(tw.GetActiveTab())
			return m, m.selectionChanged()
		}
		return m, nil

	// Instance actions
	case keys.KeyKill:
		return m.handleKill()
	case keys.KeyEnter:
		return m.handleEnter()

	default:
		return m, nil
	}
}

// handleKill handles the kill/delete session action.
func (m *home) handleKill() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.Status == session.Loading {
		return m, nil
	}

	tw := m.contentPane.TabbedWindow()
	killAction := func() tea.Msg {
		tw.CleanupTerminalForInstance(selected.Title)
		m.sidebar.Kill()
		if err := m.storage.DeleteInstance(selected.Title); err != nil {
			log.ErrorLog.Printf("failed to delete instance from storage: %v", err)
		}

		// Auto-move linked board task to "done" and unlink it.
		if b := m.contentPane.KanbanPane().GetBoard(); b != nil {
			if linkedTask := b.FindTaskByInstance(selected.Title); linkedTask != nil {
				b.UnlinkTask(linkedTask.ID)
				if err := b.MoveTask(linkedTask.ID, "done"); err == nil {
					if err := board.SaveBoard(b); err != nil {
						log.ErrorLog.Printf("failed to save board after moving task to done: %v", err)
					}
					m.contentPane.KanbanPane().SetBoard(b)
					m.sidebar.SetTaskCount(b.TaskCount())
				}
			}
		}

		return instanceChangedMsg{}
	}

	message := fmt.Sprintf("[!] Kill session '%s'?", selected.Title)
	return m, m.confirmAction(message, killAction)
}

// handleEnter handles the enter/open key action.
func (m *home) handleEnter() (tea.Model, tea.Cmd) {
	sel := m.sidebar.GetSelection()
	tw := m.contentPane.TabbedWindow()

	// Toggle expandable section headers (only Instances has children)
	if sel.IsHeader && sel.Kind == ui.SectionInstances {
		m.sidebar.ToggleSection()
		return m, m.selectionChanged()
	}
	// Instance selected
	if sel.Kind == ui.SectionInstances {
		selected := m.sidebar.GetSelectedInstance()
		if selected == nil || selected.Status == session.Loading || !selected.TmuxAlive() {
			return m, nil
		}
		if tw.IsInTerminalTab() {
			m.showHelpScreen(helpTypeInstanceAttach{}, func() {
				ch, err := tw.AttachTerminal()
				if err != nil {
					log.ErrorLog.Printf("failed to attach terminal: %v", err)
					return
				}
				<-ch
				m.state = stateDefault
			})
			return m, nil
		}
		m.showHelpScreen(helpTypeInstanceAttach{}, func() {
			ch, err := m.sidebar.Attach()
			if err != nil {
				log.ErrorLog.Printf("failed to attach: %v", err)
				return
			}
			<-ch
			m.state = stateDefault
		})
		return m, nil
	}
	return m, nil
}

// handleOpenPR opens the PR URL in the browser.
func (m *home) handleOpenPR() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.GetPRInfo() == nil {
		return m, nil
	}
	url := selected.GetPRInfo().URL
	var openCmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		openCmd = exec.Command("open", url)
	} else {
		openCmd = exec.Command("xdg-open", url)
	}
	if err := openCmd.Start(); err != nil {
		return m, m.handleError(fmt.Errorf("failed to open PR: %w", err))
	}
	return m, nil
}

// handleBoardSpawn creates a new instance from a board task, using the task title as the prompt.
func (m *home) handleBoardSpawn(bt *board.Task) tea.Cmd {
	// Auto-generate a unique instance title from the task title.
	existing := make(map[string]bool, len(m.sidebar.GetInstances()))
	for _, inst := range m.sidebar.GetInstances() {
		existing[inst.Title] = true
	}
	title := board.GenerateInstanceTitle(bt.Title, existing)

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    ".",
		Program: m.program,
	})
	if err != nil {
		return m.handleError(fmt.Errorf("failed to create instance: %w", err))
	}

	finalizer := m.sidebar.AddInstance(instance)
	m.sidebar.SetSelectedInstance(m.sidebar.NumInstances() - 1)
	instance.SetStatus(session.Loading)
	finalizer()
	m.menu.SetState(ui.StateDefault)

	// Link the board task to the new instance and move to in_progress.
	kp := m.contentPane.KanbanPane()
	if b := kp.GetBoard(); b != nil {
		b.LinkTask(bt.ID, title)
		if err := b.MoveTask(bt.ID, "in_progress"); err != nil {
			log.ErrorLog.Printf("failed to move task to in_progress: %v", err)
		}
		if err := board.SaveBoard(b); err != nil {
			log.ErrorLog.Printf("failed to save board: %v", err)
		}
		kp.SetBoard(b)
		m.sidebar.SetTaskCount(b.TaskCount())
	}

	m.preSaveInstances()

	prompt := bt.Title
	startCmd := func() tea.Msg {
		if err := instance.Start(true); err != nil {
			return instanceStartedMsg{instance: instance, err: err}
		}

		if err := task.WaitForReady(instance); err != nil {
			return instanceStartedMsg{instance: instance, err: err}
		}

		if instance.CheckAndHandleTrustPrompt() {
			time.Sleep(1 * time.Second)
			if err := task.WaitForReady(instance); err != nil {
				return instanceStartedMsg{instance: instance, err: err}
			}
		}

		if err := instance.SendPromptCommand(prompt); err != nil {
			return instanceStartedMsg{instance: instance, err: err}
		}

		return instanceStartedMsg{instance: instance, err: nil}
	}

	return tea.Batch(tea.WindowSize(), m.selectionChanged(), startCmd)
}

// handleCopyPR copies the PR URL to the clipboard.
func (m *home) handleCopyPR() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.GetPRInfo() == nil {
		return m, nil
	}
	url := selected.GetPRInfo().URL
	var copyCmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		copyCmd = exec.Command("pbcopy")
	default:
		if _, err := exec.LookPath("wl-copy"); err == nil {
			copyCmd = exec.Command("wl-copy")
		} else {
			copyCmd = exec.Command("xclip", "-selection", "clipboard")
		}
	}
	copyCmd.Stdin = strings.NewReader(url)
	if err := copyCmd.Run(); err != nil {
		return m, m.handleError(fmt.Errorf("failed to copy PR URL: %w", err))
	}
	return m, nil
}

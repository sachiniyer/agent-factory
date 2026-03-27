package app

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"os"
	"os/exec"
	"runtime"
	"strings"

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

		return instanceChangedMsg{}
	}

	// Check for uncommitted changes in the worktree (skip for remote sessions
	// which have no local worktree).
	hasChanges := false
	if !selected.IsRemote() {
		if wt := selected.GetWorktreePath(); wt != "" {
			out, err := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
			if err == nil && len(strings.TrimSpace(string(out))) > 0 {
				hasChanges = true
			}
		}
	}

	message := fmt.Sprintf("[!] Kill session '%s'?", selected.Title)
	if hasChanges {
		message = fmt.Sprintf("[!] Kill session '%s'?\n\nWARNING: This worktree has uncommitted changes that will be lost!", selected.Title)
	}
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

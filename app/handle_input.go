package app

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// handleStateNew handles key events when in stateNew (naming a new instance).
func (m *home) handleStateNew(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		m.state = stateDefault
		m.namingInstance = nil
		m.selectedWorktree = nil
		m.availableWorktrees = nil
		if err := m.sidebar.Kill(); err != nil {
			log.ErrorLog.Printf("failed to clean up instance on cancel: %v", err)
		}
		return m, tea.Sequence(
			tea.WindowSize(),
			func() tea.Msg {
				m.menu.SetState(ui.StateDefault)
				return nil
			},
		)
	}

	instance := m.namingInstance
	if instance == nil {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		if len(instance.Title) == 0 {
			return m, m.handleError(fmt.Errorf("title cannot be empty"))
		}

		// Apply the program selected during naming
		instance.Program = m.pendingProgram
		instance.SetStatus(session.Loading)
		m.newInstanceFinalizer()
		m.namingInstance = nil
		m.state = stateDefault
		m.menu.SetState(ui.StateDefault)

		m.preSaveInstances()

		selectedWt := m.selectedWorktree
		m.selectedWorktree = nil
		m.availableWorktrees = nil
		startCmd := func() tea.Msg {
			var err error
			if selectedWt != nil {
				err = instance.StartWithExistingWorktree(selectedWt.Path, selectedWt.Branch)
			} else {
				err = instance.Start(true)
			}
			return instanceStartedMsg{
				instance: instance,
				err:      err,
			}
		}

		return m, tea.Batch(tea.WindowSize(), m.selectionChanged(), startCmd)
	case tea.KeyTab:
		// Open program selection overlay
		items := make([]string, len(tmux.SupportedPrograms))
		selectedIdx := 0
		for i, p := range tmux.SupportedPrograms {
			items[i] = p
			if strings.Contains(strings.ToLower(m.pendingProgram), p) {
				selectedIdx = i
			}
		}
		m.selectionOverlay = overlay.NewSelectionOverlay("Select Program", items)
		m.selectionOverlay.SetWidth(40)
		m.selectionOverlay.SetSelectedIndex(selectedIdx)
		m.state = stateSelectProgram
		return m, nil
	case tea.KeyRunes:
		if runewidth.StringWidth(instance.Title) >= 32 {
			return m, m.handleError(fmt.Errorf("title cannot be longer than 32 characters"))
		}
		if err := instance.SetTitle(instance.Title + string(msg.Runes)); err != nil {
			return m, m.handleError(err)
		}
	case tea.KeyBackspace:
		runes := []rune(instance.Title)
		if len(runes) == 0 {
			return m, nil
		}
		if err := instance.SetTitle(string(runes[:len(runes)-1])); err != nil {
			return m, m.handleError(err)
		}
	case tea.KeySpace:
		if err := instance.SetTitle(instance.Title + " "); err != nil {
			return m, m.handleError(err)
		}
	case tea.KeyEsc:
		if err := m.sidebar.Kill(); err != nil {
			log.ErrorLog.Printf("failed to clean up instance on cancel: %v", err)
		}
		m.namingInstance = nil
		m.state = stateDefault
		m.selectedWorktree = nil
		m.availableWorktrees = nil
		cmd := m.selectionChanged()

		return m, tea.Batch(cmd, tea.Sequence(
			tea.WindowSize(),
			func() tea.Msg {
				m.menu.SetState(ui.StateDefault)
				return nil
			},
		))
	default:
	}
	return m, nil
}

// startNewInstance creates a new instance and enters stateNew for naming.
// If remote is true, the instance is forced to use the remote hook backend.
func (m *home) startNewInstance(remote bool) (tea.Model, tea.Cmd) {
	m.pendingProgram = m.program
	instance, err := session.NewInstance(session.InstanceOptions{
		Title:       "",
		Path:        ".",
		Program:     m.pendingProgram,
		ForceRemote: remote,
	})
	if err != nil {
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

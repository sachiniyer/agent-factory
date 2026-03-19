package app

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// handleStateNew handles key events when in stateNew (naming a new instance).
func (m *home) handleStateNew(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		m.state = stateDefault
		m.promptAfterName = false
		m.selectedWorktree = nil
		m.availableWorktrees = nil
		m.sidebar.Kill()
		return m, tea.Sequence(
			tea.WindowSize(),
			func() tea.Msg {
				m.menu.SetState(ui.StateDefault)
				return nil
			},
		)
	}

	instance := m.sidebar.GetInstances()[m.sidebar.NumInstances()-1]
	switch msg.Type {
	case tea.KeyEnter:
		if len(instance.Title) == 0 {
			return m, m.handleError(fmt.Errorf("title cannot be empty"))
		}

		instance.SetStatus(session.Loading)
		m.newInstanceFinalizer()
		promptAfterName := m.promptAfterName
		m.promptAfterName = false
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
				instance:        instance,
				err:             err,
				promptAfterName: promptAfterName,
			}
		}

		return m, tea.Batch(tea.WindowSize(), m.selectionChanged(), startCmd)
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
		m.sidebar.Kill()
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

// handleStatePrompt handles key events when in statePrompt (entering a prompt).
func (m *home) handleStatePrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	shouldClose := m.textInputOverlay.HandleKeyPress(msg)
	if shouldClose {
		selected := m.sidebar.GetSelectedInstance()
		if selected == nil {
			return m, nil
		}
		if m.textInputOverlay.IsSubmitted() {
			if err := selected.SendPrompt(m.textInputOverlay.GetValue()); err != nil {
				return m, m.handleError(err)
			}
		}
		m.textInputOverlay = nil
		m.state = stateDefault
		return m, tea.Sequence(
			tea.WindowSize(),
			func() tea.Msg {
				m.menu.SetState(ui.StateDefault)
				m.showHelpScreen(helpStart(selected), nil)
				return nil
			},
		)
	}
	return m, nil
}

// startNewInstance creates a new instance and enters stateNew for naming.
func (m *home) startNewInstance(promptAfterName bool) (tea.Model, tea.Cmd) {
	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   "",
		Path:    ".",
		Program: m.program,
	})
	if err != nil {
		return m, m.handleError(err)
	}
	instance.SetStatus(session.Loading)
	m.newInstanceFinalizer = m.sidebar.AddInstance(instance)
	m.sidebar.SetSelectedInstance(m.sidebar.NumInstances() - 1)
	m.state = stateNew
	m.menu.SetState(ui.StateNewInstance)
	m.promptAfterName = promptAfterName
	return m, nil
}

// showPromptOverlay displays the prompt text input overlay for the selected instance.
func (m *home) showPromptOverlay() {
	m.state = statePrompt
	m.menu.SetState(ui.StatePrompt)
	m.textInputOverlay = overlay.NewTextInputOverlay("Enter prompt", "")
}

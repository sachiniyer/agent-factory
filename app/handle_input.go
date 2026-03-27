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
		m.namingInstance = nil
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

	instance := m.namingInstance
	if instance == nil {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		if len(instance.Title) == 0 {
			return m, m.handleError(fmt.Errorf("title cannot be empty"))
		}

		// For remote instances, collect a prompt before launching.
		if instance.IsRemote() {
			m.remotePromptInstance = instance
			m.namingInstance = nil
			m.state = stateRemotePrompt
			m.remotePromptOverlay = overlay.NewTextInputOverlay("Enter prompt for remote session", "")
			// Size will be set on the next WindowSizeMsg; use a default for now.
			m.remotePromptOverlay.SetSize(60, 10)
			return m, tea.WindowSize()
		}

		return m.finishNewInstance(instance)
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

// handleStateRemotePrompt handles key events when entering a prompt for a remote instance.
func (m *home) handleStateRemotePrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.remotePromptOverlay == nil {
		m.state = stateDefault
		return m, nil
	}

	closed := m.remotePromptOverlay.HandleKeyPress(msg)
	if !closed {
		return m, nil
	}

	instance := m.remotePromptInstance
	m.remotePromptInstance = nil

	if m.remotePromptOverlay.IsCanceled() {
		// User cancelled — remove the instance.
		m.remotePromptOverlay = nil
		m.sidebar.Kill()
		m.state = stateDefault
		m.menu.SetState(ui.StateDefault)
		cmd := m.selectionChanged()
		return m, tea.Batch(cmd, tea.WindowSize())
	}

	// Submit — set the prompt and launch.
	prompt := m.remotePromptOverlay.GetValue()
	m.remotePromptOverlay = nil

	if prompt == "" {
		return m, m.handleError(fmt.Errorf("prompt cannot be empty for remote sessions"))
	}

	instance.Prompt = prompt
	return m.finishNewInstance(instance)
}

// finishNewInstance finalizes instance creation: sets loading, triggers Start in background.
func (m *home) finishNewInstance(instance *session.Instance) (tea.Model, tea.Cmd) {
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
}

// startNewInstance creates a new instance and enters stateNew for naming.
// If remote is true, the instance is forced to use the remote hook backend.
func (m *home) startNewInstance(remote bool) (tea.Model, tea.Cmd) {
	instance, err := session.NewInstance(session.InstanceOptions{
		Title:       "",
		Path:        ".",
		Program:     m.program,
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

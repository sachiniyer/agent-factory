package app

import (
	"fmt"

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
		// Kill by the captured namingInstance pointer, not the live selection:
		// background sync may have drifted the selection off the naming row, in
		// which case selection-based Kill() silently no-ops and leaves a
		// "Loading" zombie behind (#717). Kill before clearing the pointer.
		if err := m.sidebar.KillInstance(m.namingInstance); err != nil {
			log.ErrorLog.Printf("failed to clean up instance on cancel: %v", err)
		}
		m.state = stateDefault
		m.namingInstance = nil
		m.selectedWorktree = nil
		m.availableWorktrees = nil
		// Menu.SetState rebuilds the options slice; call it synchronously
		// on the event-loop goroutine rather than from a tea.Cmd closure
		// that runs off-loop and races with home.View -> Menu.String.
		m.menu.SetState(ui.StateDefault)
		return m, tea.Batch(m.selectionChanged(), tea.WindowSize())
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
		for _, other := range m.sidebar.GetInstances() {
			if other == instance {
				continue
			}
			if other.Title == instance.Title {
				return m, m.handleError(fmt.Errorf("a session titled %q already exists", instance.Title))
			}
		}
		if instance.IsRemote() {
			existing := make([]*session.Instance, 0, m.sidebar.NumInstances())
			for _, other := range m.sidebar.GetInstances() {
				if other == instance || !other.IsRemote() {
					continue
				}
				existing = append(existing, other)
			}
			if dup := session.FindSlugCollision(instance.Title, existing); dup != "" {
				return m, m.handleError(fmt.Errorf(
					"a remote session titled %q already maps to hook name %q",
					dup, session.Slugify(instance.Title),
				))
			}
		}

		// Apply the program selected during naming
		instance.Program = m.pendingProgram
		instance.SetStatus(session.Loading)
		m.namingInstance = nil
		m.state = stateDefault
		m.menu.SetState(ui.StateDefault)

		selectedWt := m.selectedWorktree
		m.selectedWorktree = nil
		m.availableWorktrees = nil
		startCmd := func() tea.Msg {
			req := sessionStartRequest{
				Title:       instance.Title,
				RepoPath:    instance.Path,
				Program:     instance.Program,
				AutoYes:     m.autoYes,
				ForceRemote: instance.IsRemote(),
			}
			if selectedWt != nil {
				req.ExistingWorktreePath = selectedWt.Path
				req.ExistingWorktreeBranch = selectedWt.Branch
			}
			started, err := startSessionThroughDaemon(instance, req)
			return instanceStartedMsg{
				instance: instance,
				started:  started,
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
			if m.pendingProgram == p {
				selectedIdx = i
			}
		}
		m.selectionOverlay = overlay.NewSelectionOverlay("Select Program", items)
		m.selectionOverlay.SetWidth(40)
		m.selectionOverlay.SetSelectedIndex(selectedIdx)
		m.state = stateSelectProgram
		return m, nil
	case tea.KeyRunes:
		newTitle := instance.Title + string(msg.Runes)
		if runewidth.StringWidth(newTitle) > 32 {
			return m, m.handleError(fmt.Errorf("title cannot be longer than 32 characters"))
		}
		if err := instance.SetTitle(newTitle); err != nil {
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
		newTitle := instance.Title + " "
		if runewidth.StringWidth(newTitle) > 32 {
			return m, m.handleError(fmt.Errorf("title cannot be longer than 32 characters"))
		}
		if err := instance.SetTitle(newTitle); err != nil {
			return m, m.handleError(err)
		}
	case tea.KeyEsc:
		// Kill by the captured namingInstance pointer, not the live selection
		// (#717) — see the ctrl+c branch above for the full rationale.
		if err := m.sidebar.KillInstance(m.namingInstance); err != nil {
			log.ErrorLog.Printf("failed to clean up instance on cancel: %v", err)
		}
		m.namingInstance = nil
		m.state = stateDefault
		m.selectedWorktree = nil
		m.availableWorktrees = nil
		cmd := m.selectionChanged()

		// Menu.SetState rebuilds the options slice; call it synchronously
		// on the event-loop goroutine rather than from a tea.Cmd closure
		// that runs off-loop and races with home.View -> Menu.String.
		m.menu.SetState(ui.StateDefault)
		return m, tea.Batch(cmd, tea.WindowSize())
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
	m.sidebar.AddInstance(instance)
	m.sidebar.SetSelectedInstance(m.sidebar.NumInstances() - 1)
	m.namingInstance = instance
	m.state = stateNew
	m.menu.SetState(ui.StateNewInstance)
	return m, nil
}

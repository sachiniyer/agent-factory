package app

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
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
		if err := m.store.KillInstance(m.namingInstance); err != nil {
			log.ErrorLog.Printf("failed to clean up instance on cancel: %v", err)
		}
		m.state = stateDefault
		m.namingInstance = nil
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
		// Reject whitespace-only titles too: len()/== "" pass a "   " title
		// through to session creation, producing an invisible name in the
		// sidebar (#973). TrimSpace mirrors the daemon's validateTitleAvailableLocked.
		if strings.TrimSpace(instance.Title) == "" {
			return m, m.handleError(fmt.Errorf("title cannot be empty"))
		}
		// "root" is reserved for the daemon-managed root agent (#1106). The
		// daemon's reserveCreate is the authoritative gate; rejecting here
		// keeps the user in the naming overlay instead of surfacing the
		// error after submit, mirroring the #936 collision pre-check below.
		if session.IsReservedTitle(instance.Title) {
			return m, m.handleError(fmt.Errorf("title %q is reserved for the daemon-managed root agent; pick another name", instance.Title))
		}
		for _, other := range m.store.GetInstances() {
			if other == instance {
				continue
			}
			// Mirror the daemon's authoritative collision rule (git.TitlesCollide:
			// case-insensitive equality OR same sanitized branch) so the naming
			// flow rejects what the daemon would reject after submit, instead of
			// only catching exact duplicates and deferring case/branch variants
			// to a post-Start error (#936).
			if git.TitlesCollide(other.Title, instance.Title, m.appConfig.BranchPrefix) {
				return m, m.handleError(fmt.Errorf("a session titled %q conflicts with existing session %q", instance.Title, other.Title))
			}
		}
		if instance.IsRemote() {
			existing := make([]*session.Instance, 0, m.store.NumInstances())
			for _, other := range m.store.GetInstances() {
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
		if err := m.preflightSessionCreate(instance); err != nil {
			return m, m.handleError(err)
		}

		// Apply the program selected during naming
		instance.Program = m.pendingProgram
		instance.SetInFlightOp(session.OpCreating)
		m.namingInstance = nil
		m.state = stateDefault
		m.menu.SetState(ui.StateDefault)

		// Capture the start seam on the event loop, before the goroutine: it is a
		// package var swapped by test seams, so reading it inside the cmd goroutine
		// would race a sibling parallel test's swap (the #960 PR 4 snapshot-race
		// class). Reading it here pins the value for this cmd.
		start := startSessionThroughDaemon
		startCmd := func() tea.Msg {
			req := sessionStartRequest{
				Title:       instance.Title,
				RepoPath:    instance.Path,
				Program:     instance.Program,
				AutoYes:     m.autoYes,
				ForceRemote: instance.IsRemote(),
			}
			started, err := start(instance, req)
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
		if err := m.store.KillInstance(m.namingInstance); err != nil {
			log.ErrorLog.Printf("failed to clean up instance on cancel: %v", err)
		}
		m.namingInstance = nil
		m.state = stateDefault
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
	if m.pendingProgram == "" && m.appConfig != nil {
		m.pendingProgram = m.appConfig.DefaultProgram
	}
	instance, err := session.NewInstance(session.InstanceOptions{
		Title:       "",
		Path:        ".",
		Program:     m.pendingProgram,
		ForceRemote: remote,
	})
	if err != nil {
		return m, m.handleError(err)
	}
	instance.SetInFlightOp(session.OpCreating)
	m.store.AddInstance(instance)
	m.sidebar.SetSelectedInstance(m.store.NumInstances() - 1)
	m.namingInstance = instance
	m.state = stateNew
	m.menu.SetState(ui.StateNewInstance)
	return m, nil
}

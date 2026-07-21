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
		m.pendingPrompt = ""
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
		if instance.Capabilities().Workspace == session.WorkspaceRemote {
			existing := make([]*session.Instance, 0, m.store.NumInstances())
			for _, other := range m.store.GetInstances() {
				if other == instance || other.Capabilities().Workspace != session.WorkspaceRemote {
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

		// Apply the program selected during naming. The optimistic create op
		// (OpCreating) was already raised in startNewInstance when the naming flow
		// began — re-raising it here would be a second BeginCreate from OpCreating,
		// an illegal edge the chokepoint rejects (#1350). Set it exactly once.
		instance.Program = m.pendingProgram
		// Read the pending prompt here, on the event loop, and clear it with the
		// rest of the naming state: the cmd below runs off-loop, so reading
		// m.pendingPrompt from inside the closure would race the next create's
		// reset. TrimSpace so a field holding only whitespace is "no prompt"
		// rather than a stray newline delivered to the agent.
		prompt := strings.TrimSpace(m.pendingPrompt)
		m.pendingPrompt = ""
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
				Title:    instance.Title,
				RepoPath: instance.Path,
				Program:  instance.Program,
				// The initial prompt typed into the naming form's shift+tab
				// field (#1936). session_control.go forwards it to the daemon,
				// which delivers it once the agent is ready — the same path
				// `af sessions create --prompt` takes. Empty means "no prompt",
				// exactly as before this field existed.
				Prompt:      prompt,
				AutoYes:     m.autoYes,
				ForceRemote: instance.Capabilities().Workspace == session.WorkspaceRemote,
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
		m.selectionOverlay = overlay.NewSelectionOverlay("Select program", items)
		m.selectionOverlay.SetWidth(40)
		m.selectionOverlay.SetSelectedIndex(selectedIdx)
		m.layoutSelectionOverlay()
		m.state = stateSelectProgram
		return m, nil
	case tea.KeyShiftTab:
		// Open the initial-prompt field, seeded with whatever is already
		// pending so reopening it is an edit, not a retype (#1936).
		m.promptOverlay = overlay.NewPromptOverlay("Initial prompt", m.pendingPrompt)
		m.layoutPromptOverlay()
		m.state = statePromptInput
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
		m.pendingPrompt = ""
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
	// Every create starts with an empty prompt field. The cancel paths clear it
	// too, but this is the authoritative reset: it also covers a create that
	// ended by any route other than Enter/Esc/ctrl+c.
	m.pendingPrompt = ""
	if m.pendingProgram == "" && m.appConfig != nil {
		m.pendingProgram = m.appConfig.DefaultProgram
	}
	// Target the ACTIVE project's repo root, not the process cwd: after an
	// in-place project switch (#1461) the active repo is m.repoRoot, which may no
	// longer be where af was launched. At launch m.repoRoot is the cwd's repo, so
	// this is equivalent for the unswitched case.
	repoPath := m.repoRoot
	if repoPath == "" {
		repoPath = "."
	}
	if remote {
		configured, err := session.RemoteHooksConfiguredForPath(repoPath)
		if err != nil {
			return m, m.handleError(err)
		}
		if !configured {
			// The menu advertises `N new remote` next to `n new`, so an
			// unconfigured repo must SAY that rather than eat the keypress
			// (#2020). RemoteHooksConfiguredForPath reports the unconfigured
			// repo as (false, nil) — a normal empty state, not an error — which
			// is why only a MALFORMED remote_hooks config used to surface
			// anything, and the common case (no remote_hooks at all) did
			// nothing at all. Every other gated action in the TUI explains
			// itself; this was the one that did not.
			//
			// The cause and the fix lead the sentence: the transient notice
			// clips to the terminal width and the tail is what disappears
			// (#1973), so the guide URL — recoverable under `E details` — goes
			// last.
			return m, m.handleError(fmt.Errorf(
				"remote sessions need a remote_hooks backend configured for this repo — press n for a local session, or configure remote_hooks and try again. Guide: https://sachiniyer.github.io/agent-factory/remote-hooks/"))
		}
	}
	instance, err := session.NewInstance(session.InstanceOptions{
		Title:       "",
		Path:        repoPath,
		Program:     m.pendingProgram,
		ForceRemote: remote,
	})
	if err != nil {
		return m, m.handleError(err)
	}
	_ = instance.Transition(session.BeginCreate())
	m.store.AddInstance(instance)
	m.sidebar.SelectInstance(instance)
	m.namingInstance = instance
	m.state = stateNew
	m.menu.SetNamingHasPrompt(false)
	m.menu.SetState(ui.StateNewInstance)
	return m, nil
}

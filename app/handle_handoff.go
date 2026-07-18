package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// handoffAgentChoices returns the agents the selected session may be handed to:
// every supported agent except the one already running.
//
// It returns the display list and a parallel slice of agent names rather than
// indexing SupportedPrograms directly the way the create-time picker does. That
// picker can index the canonical slice because it offers all of it; this one
// filters, so positions no longer line up — and SupportedPrograms is explicitly
// documented as positionally load-bearing. Carrying the names alongside removes
// the chance of an off-by-one silently handing off to the wrong agent.
func handoffAgentChoices(current string) []string {
	choices := make([]string, 0, len(tmux.SupportedPrograms))
	for _, agent := range tmux.SupportedPrograms {
		if agent == current {
			continue
		}
		choices = append(choices, agent)
	}
	return choices
}

// handleHandoff opens the agent picker for a handoff (#2013).
//
// Guards run BEFORE the picker, not after the choice: making the user pick an
// agent and only then telling them the session cannot be handed off wastes the
// interaction and reads as a bug.
func (m *home) handleHandoff() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.IsCreating() {
		return m, nil
	}
	if selected.IsTearingDown() {
		return m, m.handleError(fmt.Errorf("session '%s' is being deleted", selected.Title))
	}
	if !selected.Capabilities().Handoff {
		return m, m.handleError(fmt.Errorf("session '%s' cannot be handed off: only local-worktree sessions can swap their agent", selected.Title))
	}

	current := selected.ResolvedAgent()
	choices := handoffAgentChoices(current)
	if len(choices) == 0 {
		return m, m.handleError(fmt.Errorf("no other agent is available to hand '%s' off to", selected.Title))
	}

	m.handoffChoices = choices
	m.selectionOverlay = overlay.NewSelectionOverlay("Hand off to", choices)
	m.state = stateSelectHandoffAgent
	return m, nil
}

// handleStateSelectHandoffAgent handles key events while the handoff agent
// picker is open. On submit it does NOT swap immediately — it drops into the
// standard confirmation overlay, because a handoff replaces the agent editing a
// live branch and the picker alone is a single keystroke away from doing that
// by accident.
func (m *home) handleStateSelectHandoffAgent(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	shouldClose := m.selectionOverlay.HandleKeyPress(msg)
	if !shouldClose {
		return m, nil
	}

	submitted := m.selectionOverlay.IsSubmitted()
	idx := m.selectionOverlay.GetSelectedIndex()
	choices := m.handoffChoices

	m.selectionOverlay = nil
	m.handoffChoices = nil
	m.state = stateDefault
	m.menu.SetState(ui.StateDefault)

	if !submitted || idx < 0 || idx >= len(choices) {
		return m, nil
	}
	target := choices[idx]

	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	title := selected.Title
	from := selected.ResolvedAgent()

	// Lead with the consequential half: the clipped-notice class (#1973) drops the
	// TAIL of a line at real terminal widths, so what the user must understand
	// goes first and the reassurance goes last.
	message := fmt.Sprintf("Hand '%s' from %s to %s?", title, from, target)
	detail := "The new agent starts fresh with a summary of the work so far. " +
		"Same worktree and branch — nothing is discarded."

	return m, m.confirmActionWithDetail(message, detail, func() tea.Msg {
		return startHandoffMsg{title: title, target: target}
	})
}

// startHandoffMsg is emitted by the confirmation action and turned into the
// async daemon call, mirroring the kill/archive confirm→msg→cmd shape so the
// event loop never blocks on the swap (which stops a process and starts
// another).
type startHandoffMsg struct {
	title  string
	target string
}

type handoffDoneMsg struct {
	title  string
	from   string
	target string
	err    error
}

// handoffCmd runs the daemon handoff off the event loop.
func (m *home) handoffCmd(title, target string) tea.Cmd {
	repoID := m.repoID
	handoff := handoffSessionThroughDaemon
	return func() tea.Msg {
		from, err := handoff(title, repoID, target)
		if err != nil {
			log.ErrorLog.Printf("could not hand session %q off to %s: %v", title, target, err)
		}
		return handoffDoneMsg{title: title, from: from, target: target, err: err}
	}
}

// handleHandoffDone finalizes an async handoff. The daemon has already swapped
// the program, cleared any limit block, and persisted; the TUI is a projection,
// so there is no local state to reconcile beyond surfacing the outcome.
func (m *home) handleHandoffDone(msg handoffDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.handleError(fmt.Errorf("handoff of '%s' to %s failed: %w", msg.title, msg.target, msg.err))
	}
	from := msg.from
	if from == "" {
		from = "its previous agent"
	}
	return m, m.showTransientMessage(fmt.Sprintf("'%s' handed from %s to %s", msg.title, from, msg.target))
}

package app

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

type handoffPickerTarget struct {
	id        string
	title     string
	repoID    string
	createdAt time.Time
}

func (target handoffPickerTarget) request(to string) daemon.HandoffSessionRequest {
	return daemon.HandoffSessionRequest{
		ID: target.id, Title: target.title, RepoID: target.repoID, To: to,
	}
}

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

// handoffConfirmMessage builds the confirmation prompt for a handoff.
//
// It leads with the consequential half: the clipped-notice class (#1973) drops
// the TAIL of a line at real terminal widths, so what the user must understand
// goes first and the reassurance goes last.
//
// When the outgoing agent is unknown it drops the "from" clause entirely rather
// than interpolating an empty string. "Hand 'alpha' from  to codex?" renders a
// double space and reads as a rendering bug — on the one dialog that has to be
// trusted before a running agent is replaced.
func handoffConfirmMessage(title, from, target string) string {
	if from == "" {
		return fmt.Sprintf("Hand '%s' to %s?", title, target)
	}
	return fmt.Sprintf("Hand '%s' from %s to %s?", title, from, target)
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
	if err := selected.ValidateRuntimeAction(session.RuntimeActionHandoff); err != nil {
		return m, m.handleError(err)
	}
	if !selected.Capabilities().Handoff {
		return m, m.handleError(fmt.Errorf("session '%s' cannot be handed off: only local-worktree sessions can swap their agent", selected.Title))
	}

	current := selected.CurrentAgentName()
	choices := handoffAgentChoices(current)
	if len(choices) == 0 {
		return m, m.handleError(fmt.Errorf("no other agent is available to hand '%s' off to", selected.Title))
	}

	m.handoffChoices = choices
	m.handoffTarget = handoffPickerTarget{
		id: selected.ID, title: selected.Title, repoID: m.repoID, createdAt: selected.CreatedAt,
	}
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
	pickerTarget := m.handoffTarget

	m.selectionOverlay = nil
	m.handoffChoices = nil
	m.handoffTarget = handoffPickerTarget{}
	m.state = stateDefault
	m.menu.SetState(ui.StateDefault)

	if !submitted || idx < 0 || idx >= len(choices) {
		return m, nil
	}
	target := choices[idx]

	selected := m.resolveHandoffPickerTarget(pickerTarget)
	if selected == nil {
		return m, nil
	}
	title := selected.Title
	from := selected.CurrentAgentName()

	message := handoffConfirmMessage(title, from, target)
	detail := "The new agent starts fresh with a summary of the work so far. " +
		"Same worktree and branch — nothing is discarded."

	return m, m.confirmActionWithDetail(message, detail, func() tea.Msg {
		// Confirmation is a second retained-intent boundary after the picker.
		// Re-resolve the captured identity so an id-less legacy row replaced while
		// the dialog was open cannot hand its title to the new session.
		if m.resolveHandoffPickerTarget(pickerTarget) == nil {
			return nil
		}
		return startHandoffMsg{request: pickerTarget.request(target), target: pickerTarget}
	})
}

func (m *home) resolveHandoffPickerTarget(target handoffPickerTarget) *session.Instance {
	if target.repoID == "" || target.repoID != m.repoID {
		return nil
	}
	if target.id == "" {
		// Compatibility for records written before stable session IDs existed.
		// CreatedAt is the same legacy discriminator snapshot reconciliation uses;
		// a zero timestamp proves no identity, so fail closed instead of letting
		// title reuse inherit a retained destructive action (#2322/#2358).
		if target.createdAt.IsZero() {
			return nil
		}
		inst := m.store.GetInstanceByTitle(target.title)
		if inst != nil && inst.CreatedAt.Equal(target.createdAt) {
			return inst
		}
		return nil
	}
	for _, inst := range m.store.GetInstances() {
		if inst.ID == target.id {
			return inst
		}
	}
	return nil
}

// startHandoffMsg is emitted by the confirmation action and turned into the
// async daemon call, mirroring the kill/archive confirm→msg→cmd shape so the
// event loop never blocks on the swap (which stops a process and starts
// another).
type startHandoffMsg struct {
	request daemon.HandoffSessionRequest
	target  handoffPickerTarget
}

type handoffDoneMsg struct {
	title  string
	from   string
	target string
	err    error
}

// handoffCmd runs the daemon handoff off the event loop.
func (m *home) handoffCmd(request daemon.HandoffSessionRequest) tea.Cmd {
	handoff := handoffSessionThroughDaemon
	return func() tea.Msg {
		from, err := handoff(request)
		if err != nil {
			log.ErrorLog.Printf("could not hand session %q off to %s: %v", request.Title, request.To, err)
		}
		return handoffDoneMsg{title: request.Title, from: from, target: request.To, err: err}
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

package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

type handoffPickerTarget = sessionActionTarget

// handoffAgentChoices returns the agents the selected session may be handed to:
// every supported agent except the one already running.
//
// "Already running" is a resolved-identity question, not an enum one (#4430
// review): program_overrides.aider = "codex" makes the aider enum a
// self-handoff the daemon's guard refuses, while the codex enum — resolving
// to aider — is a legitimate cross-agent target the enum compare would hide.
// resolvedAgents maps each enum to the agent its configured command launches
// ("" when the command is not a provable agent invocation — never the current
// agent); a nil map falls back to the enum answer for callers without one.
//
// It returns the display list and a parallel slice of agent names rather than
// indexing SupportedPrograms directly the way the create-time picker does. That
// picker can index the canonical slice because it offers all of it; this one
// filters, so positions no longer line up — and SupportedPrograms is explicitly
// documented as positionally load-bearing. Carrying the names alongside removes
// the chance of an off-by-one silently handing off to the wrong agent.
func handoffAgentChoices(current, recorded string, resolvedAgents map[string]string) []string {
	choices := make([]string, 0, len(tmux.SupportedPrograms))
	for _, agent := range tmux.SupportedPrograms {
		resolved, known := resolvedAgents[agent]
		if !known {
			resolved = agent
		}
		if session.HandoffTargetIsCurrent(current, agent, resolved, recorded) {
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
		return m, m.handleNotice(err)
	}
	if !selected.Capabilities().Handoff {
		return m, m.handleNotice(fmt.Errorf("session '%s' cannot be handed off: only local-worktree sessions can swap their agent", selected.Title))
	}

	current := selected.CurrentAgentName()
	m.handoffChoices = nil
	m.handoffAccounts = nil
	m.handoffWarnings = nil
	var choices []string
	if account, _ := selected.AccountSelection(); account != "" {
		// A scoped session's rows are rebuilt wholesale from the daemon's
		// account answer — a synchronous repo-config read here would block
		// Update only to be discarded (#4430 review).
		choices = []string{"Loading accounts…"}
	} else {
		// Unscoped sessions get an optimistic first frame from the local
		// inspection-scope read — the same predicate the daemon's picker
		// answer applies. Handoff is only offered for local-worktree
		// sessions (guarded above), so this repo's config IS the config the
		// daemon resolves against; the daemon's ResolvedAgents rebuild is
		// still authoritative once the answer lands, and this frame is the
		// fallback if that call fails (#4430 review).
		choices = handoffAgentChoices(current, selected.AgentProgram(),
			session.HandoffEffectiveAgentsForPathInspection(selected.GetRepoPath(), tmux.SupportedPrograms))
		if len(choices) == 0 {
			return m, m.handleNotice(fmt.Errorf("no other agent is available to hand '%s' off to", selected.Title))
		}
		m.handoffChoices = choices
	}
	m.handoffTarget = captureSessionActionTarget(selected, m.repoID)
	m.selectionOverlay = overlay.NewSelectionOverlay("Hand off to", choices)
	m.state = stateSelectHandoffAgent
	return m, m.loadHandoffAccounts(current, selected.GetRepoPath())
}

// handleStateSelectHandoffAgent handles key events while the handoff agent
// picker is open. On submit it does NOT swap immediately — it drops into the
// standard confirmation overlay, because a handoff replaces the agent editing a
// live branch and the picker alone is a single keystroke away from doing that
// by accident.
func (m *home) handleStateSelectHandoffAgent(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyEnter {
		idx := m.selectionOverlay.GetSelectedIndex()
		if idx < 0 || idx >= len(m.handoffChoices) || m.handoffChoices[idx] == "" {
			return m, nil
		}
	}
	shouldClose := m.selectionOverlay.HandleKeyPress(msg)
	if !shouldClose {
		return m, nil
	}

	submitted := m.selectionOverlay.IsSubmitted()
	idx := m.selectionOverlay.GetSelectedIndex()
	choices := m.handoffChoices
	accounts := m.handoffAccounts
	warnings := m.handoffWarnings
	pickerTarget := m.handoffTarget

	m.selectionOverlay = nil
	m.handoffChoices = nil
	m.handoffAccounts = nil
	m.handoffWarnings = nil
	m.handoffTarget = handoffPickerTarget{}
	m.state = stateDefault
	m.menu.SetState(ui.StateDefault)

	if !submitted || idx < 0 || idx >= len(choices) {
		return m, nil
	}
	target := choices[idx]
	account := ""
	if idx < len(accounts) {
		account = accounts[idx]
	}

	selected := m.resolveSessionActionTarget(pickerTarget)
	if selected == nil {
		return m, nil
	}
	title := selected.Title
	from := selected.CurrentAgentName()

	message := handoffConfirmMessage(title, from, target)
	if account != "" {
		message = fmt.Sprintf("Hand %q to %s account %q?", title, target, account)
	}
	detail := "The new agent starts fresh with a summary of the work so far. " +
		"Same worktree and branch — nothing is discarded."

	if idx < len(warnings) {
		detail = warnings[idx] + detail
	}
	return m, m.confirmActionWithDetail(message, detail, func() tea.Msg {
		// Confirmation is a second retained-intent boundary after the picker.
		// Re-resolve the captured identity so an id-less legacy row replaced while
		// the dialog was open cannot hand its title to the new session.
		if m.resolveSessionActionTarget(pickerTarget) == nil {
			return nil
		}
		req := pickerTarget.handoffRequest(target, account)
		return startHandoffMsg{request: req, target: pickerTarget}
	})
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
	fromAccount string
	toAccount   string
	title       string
	from        string
	target      string
	err         error
}

// handoffCmd runs the daemon handoff off the event loop.
func (m *home) handoffCmd(request daemon.HandoffSessionRequest) tea.Cmd {
	handoff := handoffSessionThroughDaemon
	return func() tea.Msg {
		response, err := handoff(request)
		if err != nil {
			log.ErrorLog.Printf("could not hand session %q off to %s: %v", request.Title, request.To, err)
		}
		target := response.To
		if target == "" {
			target = request.To
		}
		return handoffDoneMsg{title: request.Title, from: response.From, target: target,
			fromAccount: response.FromAccount, toAccount: response.ToAccount, err: err}
	}
}

// handleHandoffDone finalizes an async handoff. The daemon has already swapped
// the program, cleared any limit block, and persisted; the TUI is a projection,
// so there is no local state to reconcile beyond surfacing the outcome.
func (m *home) handleHandoffDone(msg handoffDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		if apiclient.IsMutationCommitted(msg.err) {
			from := msg.from
			if from == "" {
				from = "its previous agent"
			}
			return m, m.showTransientMessage(fmt.Sprintf("'%s' handed from %s to %s, with warning: %v", msg.title, handoffIdentityLabel(from, msg.fromAccount), handoffIdentityLabel(msg.target, msg.toAccount), msg.err))
		}
		// A handoff whose reply was lost may already have swapped the agent and
		// delivered the mission (#4824). Reporting it as failed invites a second
		// handoff, which swaps again and delivers the brief twice.
		if mutationOutcomeUnknown(msg.err) {
			return m, m.handleError(mutationOutcomeError(
				fmt.Sprintf("handing '%s' to %s", msg.title, msg.target), "the session's agent", msg.err))
		}
		return m, m.handleError(fmt.Errorf("handoff of '%s' to %s failed: %w", msg.title, msg.target, msg.err))
	}
	from := msg.from
	if from == "" {
		from = "its previous agent"
	}
	return m, m.showTransientMessage(fmt.Sprintf("'%s' handed from %s to %s", msg.title, handoffIdentityLabel(from, msg.fromAccount), handoffIdentityLabel(msg.target, msg.toAccount)))
}

func handoffIdentityLabel(agent, account string) string {
	if account == "" {
		return agent
	}
	return fmt.Sprintf("%s (%s)", agent, account)
}

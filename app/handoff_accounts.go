package app

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

type handoffAccountsLoadedMsg struct {
	target   handoffPickerTarget
	agent    string
	response daemon.ListAccountsResponse
	err      error
}

func (m *home) loadHandoffAccounts(agent, path string) tea.Cmd {
	target, fetch := m.handoffTarget, listAccountsThroughDaemon
	return func() tea.Msg {
		resp, err := fetch("", path)
		return handoffAccountsLoadedMsg{target: target, agent: agent, response: resp, err: err}
	}
}

func (m *home) handleHandoffAccountsLoaded(msg handoffAccountsLoadedMsg) (tea.Model, tea.Cmd) {
	if m.state != stateSelectHandoffAgent || m.handoffTarget != msg.target {
		return m, nil
	}
	selected := m.resolveSessionActionTarget(msg.target)
	if selected == nil || selected.CurrentAgentName() != msg.agent {
		return m, nil
	}
	if msg.err != nil {
		return m, m.handleNotice(fmt.Errorf("load handoff accounts: %w", msg.err))
	}
	current, _ := selected.AccountSelection()
	agents, accounts, labels, warnings := []string{}, []string{}, []string{}, []string{}
	preselected := -1
	// ambientFallback records the first scope-drop row for a scoped session:
	// it is a valid offer but never the right default while a scopable
	// target's account row exists, so it is only consulted if no account row
	// claims preselected (#4430 review). An unscoped session's ambient rows
	// ARE the same-identity default and claim preselected directly.
	ambientFallback := -1
	// The daemon's account-capable roster decides whether a target can carry a
	// scope at all; a registry entry is proof of the same fact for a response
	// whose roster was empty.
	scopable := make(map[string]bool, len(msg.response.Agents)+len(msg.response.Entries))
	for _, name := range msg.response.Agents {
		scopable[name] = true
	}
	for _, entry := range msg.response.Entries {
		scopable[entry.Agent] = true
	}
	// Group the target enums by resolved identity, not by name: "the current
	// agent" is the enum whose resolved command IS this session's running
	// agent, and an enum that resolves to a DIFFERENT agent stays offerable
	// even when its name equals the resolved current — program_overrides can
	// point the current agent's name at another agent's command, so
	// program_overrides.codex = "aider" beside a codex pane makes codex a real
	// cross-agent target while aider (resolving to codex) is the same-agent
	// account section (#4430 review). The daemon's same-target guard compares
	// the same resolved identities, so a row the daemon would refuse or a
	// refusal the picker hides are both lies. An absent map means an older
	// daemon; the enum is the safe fallback. A KNOWN empty answer means the
	// resolved command is not a provable agent invocation — that is
	// non-scopable, and the target's enum alone says whether it is the current
	// agent (session.HandoffTargetIsCurrent).
	resolvedFor := func(agent string) string {
		resolved, resolvedKnown := msg.response.ResolvedAgents[agent]
		if !resolvedKnown {
			resolved = agent
		}
		return resolved
	}
	sameAgent := make(map[string]bool, len(tmux.SupportedPrograms))
	ordered := make([]string, 0, len(tmux.SupportedPrograms))
	recorded := selected.AgentProgram()
	for _, agent := range tmux.SupportedPrograms {
		if session.HandoffTargetIsCurrent(msg.agent, agent, resolvedFor(agent), recorded) {
			sameAgent[agent] = true
			ordered = append(ordered, agent)
		}
	}
	for _, agent := range tmux.SupportedPrograms {
		if !sameAgent[agent] {
			ordered = append(ordered, agent)
		}
	}
	for _, agent := range ordered {
		resolved := resolvedFor(agent)
		isCurrent := sameAgent[agent]
		canCarry := scopable[resolved]
		// The ambient row is also the honest offer for a scoped session aimed at
		// a KNOWN agent that cannot carry a scope: the swap drops it and reports
		// the drop on from_account (#4428), so the target is offered with its
		// warning rather than hidden. An UNCLASSIFIABLE resolution ("") is not
		// offered to a scoped session at all — the daemon refuses the drop on an
		// answer it cannot prove, and a row that can only error is a lie
		// (#4430 review, D1). The current agent's own section never
		// carries one: the daemon refuses a resolved-identity self-handoff, so
		// the offer would only error.
		if !isCurrent && (current == "" || (!canCarry && resolved != "")) {
			if current == "" {
				if preselected < 0 {
					preselected = len(labels)
				}
			} else if ambientFallback < 0 {
				ambientFallback = len(labels)
			}
			warning := ""
			if current != "" {
				if resolved != agent {
					warning = fmt.Sprintf("%s launches %s, which cannot carry an account — the %q scope is dropped on handoff. ", agent, resolved, current)
				} else {
					warning = fmt.Sprintf("%s cannot carry an account — the %q scope is dropped on handoff. ", agent, current)
				}
			}
			agents = append(agents, agent)
			accounts = append(accounts, "")
			labels = append(labels, agent+" (ambient)")
			warnings = append(warnings, warning)
		}
		// Account rows come from the RESOLVED namespace, not the requested
		// enum: the daemon's --account transaction registers the name against
		// the resolved command's agent, so `aider` redirected to `codex` is
		// honestly served by codex's registry — hiding those rows made the
		// target unreachable even though the daemon accepts the handoff
		// (#4430 review round 3).
		for _, entry := range msg.response.Entries {
			if entry.Agent != resolved || (isCurrent && entry.Name == current) || entry.RegistrationOnly {
				continue
			}
			label := fmt.Sprintf("%s: %s", agent, entry.Name)
			warning := ""
			if resolved != agent {
				warning = fmt.Sprintf("%s launches %s — %q is a %s account. ", agent, resolved, entry.Name, resolved)
			}
			if !entry.LoggedIn {
				label += " (not logged in)"
				warning += fmt.Sprintf("%s has no %s credential yet. Log in before handing off. ", entry.Name, resolved)
			}
			isDefault := msg.response.Defaults[resolved] == entry.Name
			if isDefault {
				label += " (project default)"
			}
			if entry.LoggedIn && (preselected < 0 || (isDefault && agents[preselected] == agent)) {
				preselected = len(labels)
			}
			agents = append(agents, agent)
			accounts = append(accounts, entry.Name)
			labels = append(labels, label)
			warnings = append(warnings, warning)
		}
	}
	if len(labels) == 0 {
		m.selectionOverlay = nil
		m.state = stateDefault
		return m, m.handleNotice(fmt.Errorf("no registered target account is available to hand %q off to", selected.Title))
	}
	if preselected < 0 {
		preselected = ambientFallback
	}
	if preselected < 0 {
		agents = append([]string{""}, agents...)
		accounts = append([]string{""}, accounts...)
		labels = append([]string{"Choose an account"}, labels...)
		warnings = append([]string{""}, warnings...)
		preselected = 0
	}
	m.handoffWarnings = warnings
	m.handoffChoices, m.handoffAccounts = agents, accounts
	m.handoffResolvedAgents = msg.response.ResolvedAgents
	m.selectionOverlay = overlay.NewSelectionOverlay("Hand off to", labels)
	m.selectionOverlay.SetWidth(64)
	m.selectionOverlay.SetSelectedIndex(preselected)
	return m, nil
}

package app

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
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
	for _, agent := range append([]string{msg.agent}, handoffAgentChoices(msg.agent)...) {
		// Classify by the command the target LAUNCHES, not the enum: the
		// daemon's resolved_agents map answers "which agent runs" for this
		// session's repo, so program_overrides.codex = "aider" shows the ambient
		// row Aider's launch warrants rather than Codex accounts a resolved
		// Aider could never use (#4430 review). A missing entry means an older
		// daemon; the enum is the safe fallback.
		resolved := msg.response.ResolvedAgents[agent]
		if resolved == "" {
			resolved = agent
		}
		canCarry := scopable[resolved]
		// The ambient row is also the honest offer for a scoped session aimed at
		// a target that cannot carry a scope: the swap drops it and reports the
		// drop on from_account (#4428), so the target is offered with its
		// warning rather than hidden.
		if agent != msg.agent && (current == "" || !canCarry) {
			if preselected < 0 {
				preselected = len(labels)
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
		// Account rows are honest only when the requested enum IS the agent the
		// command launches: the daemon's --account transaction registers against
		// the resolved command's namespace, so a redirected target could never
		// apply an account the picker offered under its own name.
		if resolved != agent {
			continue
		}
		for _, entry := range msg.response.Entries {
			if entry.Agent != agent || (agent == msg.agent && entry.Name == current) || entry.RegistrationOnly {
				continue
			}
			label := fmt.Sprintf("%s: %s", agent, entry.Name)
			warning := ""
			if !entry.LoggedIn {
				label += " (not logged in)"
				warning = fmt.Sprintf("%s has no %s credential yet. Log in before handing off. ", entry.Name, agent)
			}
			isDefault := msg.response.Defaults[agent] == entry.Name
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
		agents = append([]string{""}, agents...)
		accounts = append([]string{""}, accounts...)
		labels = append([]string{"Choose an account"}, labels...)
		warnings = append([]string{""}, warnings...)
		preselected = 0
	}
	m.handoffWarnings = warnings
	m.handoffChoices, m.handoffAccounts = agents, accounts
	m.selectionOverlay = overlay.NewSelectionOverlay("Hand off to", labels)
	m.selectionOverlay.SetWidth(64)
	m.selectionOverlay.SetSelectedIndex(preselected)
	return m, nil
}

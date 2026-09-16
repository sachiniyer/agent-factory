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
	// Account-only mode (#4433) is the reserved root's whole picker: the agent
	// axis cannot move, so the rows are the running agent's other accounts and
	// nothing else — no agent rows to refuse at submit, and no ambient row,
	// because descoping root is not an operation this surface offers.
	targetAgents := append([]string{msg.agent}, handoffAgentChoices(msg.agent)...)
	if m.handoffAccountOnly {
		targetAgents = []string{msg.agent}
	}
	for _, agent := range targetAgents {
		if agent != msg.agent && current == "" {
			if preselected < 0 {
				preselected = len(labels)
			}
			agents = append(agents, agent)
			accounts = append(accounts, "")
			labels = append(labels, agent+" (ambient)")
			warnings = append(warnings, "")
		}
		for _, entry := range msg.response.Entries {
			if entry.Agent != agent || (agent == msg.agent && entry.Name == current) || entry.RegistrationOnly {
				continue
			}
			label := fmt.Sprintf("%s: %s", agent, entry.Name)
			if m.handoffAccountOnly {
				label = entry.Name // every row is the same agent — say the account
			}
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
		accountOnly := m.handoffAccountOnly
		m.selectionOverlay = nil
		m.handoffAccountOnly = false
		m.state = stateDefault
		if accountOnly {
			return m, m.handleNotice(fmt.Errorf(
				"no other %s account is registered — 'af accounts login %s <name>' adds one", msg.agent, msg.agent))
		}
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
	title := "Hand off to"
	if m.handoffAccountOnly {
		title = "Switch account to"
	}
	m.selectionOverlay = overlay.NewSelectionOverlay(title, labels)
	m.selectionOverlay.SetWidth(64)
	m.selectionOverlay.SetSelectedIndex(preselected)
	return m, nil
}

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
		resp, err := fetch(agent, path)
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
	agents, accounts, labels := []string{}, []string{}, []string{}
	preselected := 0
	for _, entry := range msg.response.Entries {
		if entry.Agent != msg.agent || entry.Name == current || entry.RegistrationOnly {
			continue
		}
		label := fmt.Sprintf("%s: %s", msg.agent, entry.Name)
		if msg.response.Defaults[msg.agent] == entry.Name {
			preselected = len(labels)
			label += " (project default)"
		}
		agents = append(agents, msg.agent)
		accounts = append(accounts, entry.Name)
		labels = append(labels, label)
	}
	for _, agent := range handoffAgentChoices(msg.agent) {
		agents = append(agents, agent)
		accounts = append(accounts, "")
		labels = append(labels, agent)
	}
	m.handoffChoices, m.handoffAccounts = agents, accounts
	m.selectionOverlay = overlay.NewSelectionOverlay("Hand off to", labels)
	m.selectionOverlay.SetSelectedIndex(preselected)
	return m, nil
}

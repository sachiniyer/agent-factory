package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
)

// accountRegisteredMsg carries both remote round trips back to the UI loop.
// Commands perform I/O only; they never mutate the pane from their goroutine.
type accountRegisteredMsg struct {
	generation uint64
	response   daemon.RegisterAccountResponse
	accounts   daemon.ListAccountsResponse
	err        error
	listErr    error
}

func (m *home) remoteAccountRegisterCmd(agent, name string) tea.Cmd {
	m.accountGeneration++
	generation := m.accountGeneration
	register, list := registerAccount, listAccountsForPane
	m.configPane.SetAccountStatus(fmt.Sprintf("Registering %s account %q…", agent, name), false)
	return func() tea.Msg {
		response, err := register(daemon.RegisterAccountRequest{Agent: agent, Name: name})
		result := accountRegisteredMsg{generation: generation, response: response, err: err}
		if err == nil {
			result.accounts, result.listErr = list(daemon.ListAccountsRequest{})
		}
		return result
	}
}

func (m *home) handleAccountRegistered(msg accountRegisteredMsg) {
	// Closing/reopening or starting another registration makes this answer stale.
	// A late completion must neither reopen Accounts nor overwrite newer feedback.
	if msg.generation != m.accountGeneration || m.state != stateConfigEditor || !m.configPane.HasFocus() {
		return
	}
	if msg.err != nil {
		m.configPane.SetAccountStatus(msg.err.Error(), true)
		return
	}
	m.applyAccountsToPane(msg.accounts, msg.listErr)
	m.setAccountRegisteredStatus(msg.response)
}

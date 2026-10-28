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
	if m.accountRegisterInFlight != nil {
		return nil
	}
	m.accountRegisterInFlight = &daemon.RegisterAccountRequest{Agent: agent, Name: name}
	m.showAccountRegisterPending()
	m.accountGeneration++
	generation := m.accountGeneration
	register, list := registerAccount, listAccountsForPane
	return func() tea.Msg {
		response, err := register(daemon.RegisterAccountRequest{Agent: agent, Name: name})
		result := accountRegisteredMsg{generation: generation, response: response, err: err}
		if err == nil {
			result.accounts, result.listErr = list(daemon.ListAccountsRequest{})
		}
		return result
	}
}

// showAccountRegisterPending restores the view of a mutation that may have
// started in an earlier opening. The home model, not pane focus, owns its life.
func (m *home) showAccountRegisterPending() {
	m.configPane.SetAccountsBusy(m.accountRegisterInFlight != nil)
	if m.accountRegisterInFlight != nil {
		m.configPane.SetAccountStatus(accountRegisterPendingStatus(*m.accountRegisterInFlight), false)
	}
}

func accountRegisterPendingStatus(req daemon.RegisterAccountRequest) string {
	return fmt.Sprintf("Registering %s account %q…", req.Agent, req.Name)
}

func (m *home) handleAccountRegistered(msg accountRegisteredMsg) tea.Cmd {
	pending := m.accountRegisterInFlight
	m.accountRegisterInFlight = nil
	if m.state != stateConfigEditor || !m.configPane.HasFocus() {
		return nil
	}
	m.configPane.SetAccountsBusy(false)
	if msg.generation != m.accountGeneration {
		// Preserve newer feedback, but retire our own pending notice. A failed
		// mutation still needs its error shown when no newer account status exists.
		status := m.configPane.AccountStatus()
		ownsStatus := status == "" || (pending != nil && status == accountRegisterPendingStatus(*pending))
		if msg.err != nil {
			if ownsStatus {
				m.configPane.SetAccountStatus(msg.err.Error(), true)
			}
			return nil
		}
		if ownsStatus {
			m.configPane.SetAccountStatus("", false)
		}
		// The old command's snapshot may predate this opening. Fetch again, and
		// invalidate even this opening's initial read if it is still in flight.
		m.accountGeneration++
		return m.remoteAccountsLoadCmd()
	}
	if msg.err != nil {
		m.configPane.SetAccountStatus(msg.err.Error(), true)
		return nil
	}
	m.applyAccountsToPane(msg.accounts, msg.listErr)
	m.setAccountRegisteredStatus(msg.response)
	return nil
}

package app

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
)

// accountsLoadedMsg delivers the initial remote list back to the UI loop.
type accountsLoadedMsg struct {
	generation uint64
	response   daemon.ListAccountsResponse
	err        error
}

func (m *home) remoteAccountsLoadCmd() tea.Cmd {
	generation := m.accountGeneration
	list := listAccountsForPane
	return func() tea.Msg {
		response, err := list(daemon.ListAccountsRequest{})
		return accountsLoadedMsg{generation: generation, response: response, err: err}
	}
}

func (m *home) handleAccountsLoaded(msg accountsLoadedMsg) {
	// Registration also advances the generation: an older initial read must not
	// overwrite the newly registered account or feedback from a newer operation.
	if msg.generation != m.accountGeneration || m.state != stateConfigEditor || !m.configPane.HasFocus() {
		return
	}
	m.applyAccountsToPane(msg.response, msg.err)
}

package app

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
)

// configSectionsLoadedMsg delivers the Usage and Accounts reads back to the UI
// loop together (#4361). The overlay's two daemon-reported sections load as one
// command on open so opening it returns a single command, and each half carries
// its OWN generation: an account register bumping accountGeneration must not
// discard this opening's usage report, nor a usage read's generation gate the
// accounts.
type configSectionsLoadedMsg struct {
	accountGeneration uint64
	usageGeneration   uint64
	accounts          daemon.ListAccountsResponse
	accountsErr       error
	usage             daemon.QuotaReportResponse
	usageErr          error
}

// remoteSectionsLoadCmd reads both daemon-reported sections off the UI loop.
// Commands perform I/O only; they never mutate the pane from their goroutine.
func (m *home) remoteSectionsLoadCmd() tea.Cmd {
	accountGeneration, usageGeneration := m.accountGeneration, m.usageGeneration
	list, report := listAccountsForPane, quotaReportForPane
	return func() tea.Msg {
		accounts, accountsErr := list(daemon.ListAccountsRequest{})
		usage, usageErr := report(daemon.QuotaReportRequest{})
		return configSectionsLoadedMsg{
			accountGeneration: accountGeneration, usageGeneration: usageGeneration,
			accounts: accounts, accountsErr: accountsErr,
			usage: usage, usageErr: usageErr,
		}
	}
}

func (m *home) handleConfigSectionsLoaded(msg configSectionsLoadedMsg) {
	if m.state != stateConfigEditor || !m.configPane.HasFocus() {
		return
	}
	// Registration also advances the account generation: an older read must not
	// overwrite the newly registered account or feedback from a newer operation.
	if msg.accountGeneration == m.accountGeneration {
		m.applyAccountsToPane(msg.accounts, msg.accountsErr)
	}
	if msg.usageGeneration == m.usageGeneration {
		m.applyUsageToPane(msg.usage, msg.usageErr)
	}
}

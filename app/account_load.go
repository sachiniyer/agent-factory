package app

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
)

// accountsLoadedMsg delivers the remote Accounts read back to the UI loop, and
// usageLoadedMsg the Usage read (#4361). They are SEPARATE messages on purpose:
// the two daemon calls run concurrently, so a stalled ListAccounts keeps only
// Accounts on "Loading…" while Usage renders, and a stalled QuotaReport no
// longer withholds an Accounts answer that already arrived. Each message
// carries its OWN generation: an account register bumping accountGeneration
// must not discard this opening's usage report, nor a usage read's generation
// gate the accounts.
type accountsLoadedMsg struct {
	generation uint64
	accounts   daemon.ListAccountsResponse
	err        error
}

type usageLoadedMsg struct {
	generation uint64
	usage      daemon.QuotaReportResponse
	err        error
}

// remoteSectionsLoadCmd reads both daemon-reported sections off the UI loop.
// Commands perform I/O only; they never mutate the pane from their goroutine.
// tea.Batch runs the two reads concurrently and delivers each section's
// message the moment its own request finishes.
func (m *home) remoteSectionsLoadCmd() tea.Cmd {
	accountGeneration, usageGeneration := m.accountGeneration, m.usageGeneration
	list, report := listAccountsForPane, quotaReportForPane
	return tea.Batch(
		func() tea.Msg {
			accounts, err := list(daemon.ListAccountsRequest{})
			return accountsLoadedMsg{generation: accountGeneration, accounts: accounts, err: err}
		},
		func() tea.Msg {
			usage, err := report(daemon.QuotaReportRequest{})
			return usageLoadedMsg{generation: usageGeneration, usage: usage, err: err}
		},
	)
}

func (m *home) handleAccountsLoaded(msg accountsLoadedMsg) {
	if m.state != stateConfigEditor || !m.configPane.HasFocus() {
		return
	}
	// Registration also advances the account generation: an older read must not
	// overwrite the newly registered account or feedback from a newer operation.
	if msg.generation == m.accountGeneration {
		m.applyAccountsToPane(msg.accounts, msg.err)
	}
}

func (m *home) handleUsageLoaded(msg usageLoadedMsg) {
	if m.state != stateConfigEditor || !m.configPane.HasFocus() {
		return
	}
	if msg.generation == m.usageGeneration {
		m.applyUsageToPane(msg.usage, msg.err)
	}
}

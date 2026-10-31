package app

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/ui"
)

// quotaLoadedMsg delivers a remote QuotaReport read back to the UI loop.
type quotaLoadedMsg struct {
	generation uint64
	response   daemon.QuotaReportResponse
	err        error
}

// loadQuotaIntoPane fills the config overlay's Usage section (#2983). Like the
// Accounts read it is made on every open: a session parked at a limit since the
// TUI started must show as it is now, not as af last remembered.
//
// The local read answers synchronously over the gob control socket. The remote
// one is a real HTTP round trip, so it is fired when the opening's accounts
// read lands (handleAccountsLoaded → remoteQuotaLoadCmd): that keeps one
// remote read in flight per opening and leaves showConfigEditor returning the
// accounts cmd alone — the single-cmd shape the UI-loop tests drive. A stale
// response is discarded by the generation the way accountsLoadedMsg's is.
func (m *home) loadQuotaIntoPane() {
	m.configPane.SetQuotaLoading()
	if apiclient.IsRemoteTarget() {
		return
	}
	resp, err := targetedQuotaReport()
	m.applyQuotaToPane(resp, err)
}

func (m *home) remoteQuotaLoadCmd() tea.Cmd {
	generation := m.quotaGeneration
	report := targetedQuotaReport
	return func() tea.Msg {
		response, err := report()
		return quotaLoadedMsg{generation: generation, response: response, err: err}
	}
}

func (m *home) handleQuotaLoaded(msg quotaLoadedMsg) {
	if msg.generation != m.quotaGeneration || m.state != stateConfigEditor || !m.configPane.HasFocus() {
		return
	}
	m.applyQuotaToPane(msg.response, msg.err)
}

// applyQuotaToPane applies a completed read on the UI loop. The rows are
// already rendered server-side — the pane displays the daemon's words.
func (m *home) applyQuotaToPane(resp daemon.QuotaReportResponse, err error) {
	if err != nil {
		log.WarningLog.Printf("quota: could not read the usage report for the config pane: %v", err)
		m.configPane.SetQuota(nil, nil, err)
		return
	}
	rows := make([]ui.QuotaRow, 0, len(resp.Agents))
	for _, agent := range resp.Agents {
		rows = append(rows, ui.QuotaRow{
			Program:         agent.Program,
			Quota:           agent.Quota,
			Observed:        agent.Observed,
			Sessions:        agent.Sessions,
			LimitedSessions: agent.LimitedSessions,
			ResetAt:         agent.ResetAt,
			Detail:          agent.Detail,
		})
	}
	m.configPane.SetQuota(rows, resp.Warnings, nil)
}

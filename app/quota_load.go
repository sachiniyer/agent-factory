package app

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// quotaReportForPane is the read seam behind the Usage section, a var so the
// TUI tests can drive every branch without a daemon. Same shape as
// listAccountsForPane.
var quotaReportForPane = targetedQuotaReportForPane

// SetUsageSeamForTest swaps the daemon call behind the Usage section and
// returns a restore func, matching SetAccountSeamsForTest.
func SetUsageSeamForTest(
	report func(daemon.QuotaReportRequest) (daemon.QuotaReportResponse, error),
) func() {
	prev := quotaReportForPane
	quotaReportForPane = report
	return func() { quotaReportForPane = prev }
}

// loadUsageIntoPane fills the config overlay's Usage section (#4361).
//
// It is called on every open, like the config and accounts reads beside it, so
// the section shows the attached host's walls as they are NOW — a session
// parked since the TUI started is the answer the operator opened it for.
//
// A failure becomes the section's own message rather than blocking the overlay:
// the editor is still useful when the report cannot be read, and an operator
// who came to change a key should not be turned away because the usage read
// failed. A remote read is deferred to remoteSectionsLoadCmd — returned by the
// caller — so a stalled daemon cannot freeze the UI; only the in-process local
// read stays inline.
func (m *home) loadUsageIntoPane() {
	// Pace the bounded refresh from the read's dispatch (#4361 review) — the
	// remote read lands off the UI loop beside this call, so both paths mark
	// here and the refresh stays one minute behind the data either way.
	m.lastUsageRead = time.Now()
	if apiclient.IsRemoteTarget() {
		m.configPane.SetUsageLoading()
		return
	}
	resp, err := quotaReportForPane(daemon.QuotaReportRequest{})
	m.applyUsageToPane(resp, err)
}

// applyUsageToPane applies a completed read on the UI loop.
func (m *home) applyUsageToPane(resp daemon.QuotaReportResponse, err error) {
	m.lastUsageRead = time.Now()
	if err != nil {
		log.WarningLog.Printf("usage: could not read the usage report for the config pane: %v", err)
		m.configPane.SetUsage(daemon.QuotaReportResponse{}, err)
		return
	}
	m.configPane.SetUsage(resp, nil)
}

// usagePollInterval is the bounded refresh cadence for the Usage section while
// the config overlay stays open (#4361 review) — the same one-minute tick the
// web rail polls the same report on (web/src/index.ts TASK_HEALTH_POLL_MS).
const usagePollInterval = time.Minute

// usageRefreshDue re-reads the Usage section once it is a minute stale while
// the config editor is open and focused (#4361 review): a session reaching or
// clearing a wall, being killed, or being created behind the overlay changes
// the report, and the relative times ("just now", "in 5m") never advance on an
// open-only read. It rides the app's existing previewTickMsg heartbeat — a
// tea.Tick command of its own cannot ride the opener's returned command
// (remote must stay the pure section-read batch, local must stay nil, and a
// blocking tick inside either would stall the consumers that run those
// commands directly). The remote refresh is generation-fenced and
// deliberately skips SetUsageLoading: the section keeps the rows it already
// has until the answer lands rather than blinking to "Loading…" each minute.
func (m *home) usageRefreshDue() tea.Cmd {
	if m.state != stateConfigEditor || !m.configPane.HasFocus() {
		return nil
	}
	if time.Since(m.lastUsageRead) < usagePollInterval {
		return nil
	}
	m.lastUsageRead = time.Now()
	if !apiclient.IsRemoteTarget() {
		m.loadUsageIntoPane()
		return nil
	}
	m.usageGeneration++ // this read supersedes any still in flight
	return m.remoteUsageLoadCmd()
}

// remoteUsageLoadCmd refetches only the Usage section off the UI loop — the
// bounded refresh's read (#4361 review). It carries this dispatch's generation
// so a stale completion cannot overwrite a newer read's rows.
func (m *home) remoteUsageLoadCmd() tea.Cmd {
	usageGeneration := m.usageGeneration
	report := quotaReportForPane
	return func() tea.Msg {
		usage, err := report(daemon.QuotaReportRequest{})
		return usageLoadedMsg{generation: usageGeneration, usage: usage, err: err}
	}
}

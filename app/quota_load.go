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
	if apiclient.IsRemoteTarget() {
		m.configPane.SetUsageLoading()
		return
	}
	resp, err := quotaReportForPane(daemon.QuotaReportRequest{})
	m.applyUsageToPane(resp, err)
}

// applyUsageToPane applies a completed read on the UI loop.
func (m *home) applyUsageToPane(resp daemon.QuotaReportResponse, err error) {
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

// usageRefreshTickMsg fires the bounded Usage-section refresh (#4361 review):
// a session reaching or clearing a wall, being killed, or being created while
// the overlay stays open changes the report behind the pane, and the relative
// times ("just now", "in 5m") never advance at all on an open-only read. The
// message carries the open's generation so a re-opened pane runs exactly one
// refresh chain: an older chain's tick dies on its next edge.
type usageRefreshTickMsg struct {
	generation uint64
}

// usageRefreshTickCmd schedules the next refresh edge, tagged with the open's
// usage generation. The loop is bounded by the overlay's lifetime rather than
// cancelled: each tick re-arms only while the pane is still open and focused,
// so a tick landing after close is a no-op instead of a timer to reap.
func (m *home) usageRefreshTickCmd() tea.Cmd {
	generation := m.usageGeneration
	return tea.Tick(usagePollInterval, func(time.Time) tea.Msg {
		return usageRefreshTickMsg{generation: generation}
	})
}

// refreshUsageSection answers one usageRefreshTickMsg: while the config editor
// is still open and focused it re-reads the report and re-arms; once the pane
// closes — or a newer open superseded this chain — the chain ends. The remote
// refresh is generation-fenced and deliberately skips SetUsageLoading: a
// refresh keeps the rows it already has until the answer lands rather than
// blinking the section to "Loading…" once a minute.
func (m *home) refreshUsageSection(msg usageRefreshTickMsg) tea.Cmd {
	if m.state != stateConfigEditor || !m.configPane.HasFocus() || msg.generation != m.usageGeneration {
		return nil
	}
	var refresh tea.Cmd
	if apiclient.IsRemoteTarget() {
		m.usageGeneration++ // this read supersedes any still in flight
		refresh = m.remoteUsageLoadCmd()
	} else {
		m.loadUsageIntoPane()
	}
	return tea.Batch(refresh, m.usageRefreshTickCmd())
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

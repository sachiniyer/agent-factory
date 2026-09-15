package app

import (
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

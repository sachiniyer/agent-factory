package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/internal/quotaobs"
)

// QuotaReport serves the per-agent usage/quota report (#2983) — the same
// records read and the same quota.Build projection `af quota` makes, so the
// TUI, the web, and a remote `af quota` cannot disagree about what af has
// observed.
//
// Like ListTasks and GetConfig it is deliberately NOT gated on
// requireManagerReady: it reads records fresh from disk on every call, so it
// is safe — and still truthful — while the daemon is warming up or an upgrade
// candidate is parked in probation.
func (s *controlServer) QuotaReport(_ QuotaReportRequest, resp *QuotaReportResponse) error {
	result, err := quotaobs.Collect()
	if err != nil {
		return err
	}
	resp.Agents = result.Report.Rendered(time.Now())
	resp.Warnings = result.Warnings()
	return nil
}

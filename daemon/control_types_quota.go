package daemon

import "github.com/sachiniyer/agent-factory/quota"

// QuotaReportRequest asks for the per-agent usage/quota report (#2983). It
// takes no arguments: the report is about THIS daemon host's af home, exactly
// what `af quota` reads locally.
type QuotaReportRequest struct{}

// QuotaReportResponse is the quota/usage report, already rendered into display
// rows. The words — the "not reported" entitlement, the observation, the
// detail sentence — are computed server-side by quota.Rendered so the TUI, the
// web, and a remote `af quota` all show the daemon's own vocabulary and none
// re-derives it.
type QuotaReportResponse struct {
	// Agents is one row per agent af knows about, sorted by program name.
	Agents []quota.RenderedAgent `json:"agents"`
	// Warnings are the completeness caveats (record files that could not be
	// read or parsed). A renderer must show them when present — an incomplete
	// report that hides them reads as authoritative.
	Warnings []string `json:"warnings,omitempty"`
}

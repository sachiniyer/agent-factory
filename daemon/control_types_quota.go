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
	// Accounts is the per-account evidence section — what af recorded in each
	// registered account's own rollout files (#6776). Empty when neither an
	// [account_handoff] table nor a handoff state file exists on the daemon's
	// host: the absent-table shape is no section at all.
	Accounts []quota.RenderedAccount `json:"accounts,omitempty"`
	// Mover is the rendered mover line — the configured mode plus any halt
	// the engine recorded (R16: a halted mover is never silent). Empty under
	// the same absent-table condition as Accounts.
	Mover string `json:"mover,omitempty"`
}

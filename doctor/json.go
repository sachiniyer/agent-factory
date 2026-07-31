package doctor

import (
	"io"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// JSONCheck is one check in `af doctor --json`.
//
// Actionable — not Status, "Remedy is non-empty", or automated fix support — is
// the field a script should branch on. It means doctor established a specific
// unhealthy condition that must be corrected before the run is healthy. Doctor
// deliberately emits advisory WARNs and UNKNOWN observations with useful
// inspection guidance while leaving the exit code 0. Conversely, a proven
// condition can be actionable even when its exact remedy is manual. Actionable
// is derived from the same rule as the exit code, so the two cannot disagree.
type JSONCheck struct {
	Name    string `json:"name"`
	Section string `json:"section"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
	// Remedy is what to do about it. Present on advisory rows too — the hint is
	// worth having either way — and empty only when there is nothing to do.
	Remedy string `json:"remedy"`
	// Actionable reports whether doctor established that this row requires
	// correction before the run is healthy. Rows with actionable=true are what
	// make the run exit nonzero.
	Actionable bool `json:"actionable"`
}

// JSONSummary counts the run by status. Unresolved is the count that drives the
// exit code, so a script can branch on it without re-deriving the rules.
//
// Unresolved counts underlying ACTIONABLE ISSUES, which is not always the
// number of actionable rows: without --verbose, many process findings collapse
// into one row. Advisory observations are excluded. Use Unresolved for "did
// doctor establish anything unhealthy", and per-row Actionable for "which
// rows".
type JSONSummary struct {
	Pass       int `json:"pass"`
	Warn       int `json:"warn"`
	Fail       int `json:"fail"`
	Fixed      int `json:"fixed"`
	Unresolved int `json:"unresolved"`
}

// JSONReport is the `af doctor --json` payload carried in the envelope's data
// member.
type JSONReport struct {
	Checks  []JSONCheck `json:"checks"`
	Summary JSONSummary `json:"summary"`
}

// BuildJSONReport projects a Report into its JSON shape. It reuses renderRows,
// the same projection the human output prints, so the two renderings can never
// disagree about what a run found — including which findings collapse into a
// summary row.
func BuildJSONReport(r *Report, fixMode, verbose bool) JSONReport {
	rows := renderRows(r, fixMode, verbose)
	out := JSONReport{Checks: make([]JSONCheck, 0, len(rows))}
	for _, row := range rows {
		remedy := row.remediation
		if row.status == StatusPass || row.status == StatusFixed {
			remedy = ""
		}
		out.Checks = append(out.Checks, JSONCheck{
			Name:       row.name,
			Section:    row.section,
			Status:     string(row.status),
			Detail:     row.detail,
			Remedy:     remedy,
			Actionable: row.actionable,
		})
		switch row.status {
		case StatusPass:
			out.Summary.Pass++
		case StatusWarn:
			out.Summary.Warn++
		case StatusFail:
			out.Summary.Fail++
		case StatusFixed:
			out.Summary.Fixed++
		}
	}
	out.Summary.Unresolved = r.UnresolvedCount()
	return out
}

// RenderJSON writes the report as a {data,error} envelope.
func RenderJSON(w io.Writer, r *Report, fixMode, verbose bool) error {
	return apiproto.WriteEnvelope(w, apiproto.Success(BuildJSONReport(r, fixMode, verbose)))
}

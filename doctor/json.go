package doctor

import (
	"io"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// JSONCheck is one check in `af doctor --json`. Remedy is empty on a passing
// check — there is nothing to do — so a script can treat "has a remedy" and
// "needs action" as the same question.
type JSONCheck struct {
	Name    string `json:"name"`
	Section string `json:"section"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
	Remedy  string `json:"remedy"`
}

// JSONSummary counts the run by status. Unresolved is the count that drives the
// exit code, so a script can branch on it without re-deriving the rules.
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
			Name:    row.name,
			Section: row.section,
			Status:  string(row.status),
			Detail:  row.detail,
			Remedy:  remedy,
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

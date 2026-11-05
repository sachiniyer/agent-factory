package handoff

// report.go renders the report_to surface (#6776, R15): a FIXED structured
// template over EventRow fields — the only text a report may carry. Pane,
// transcript, and composer text can never appear in one, same rule as the
// event log itself. halted and move-failed are the two verdicts that jump
// the digest window; everything else batches.

import (
	"sort"
	"strconv"
	"strings"
)

// ImmediateReport reports whether the row jumps the digest window and is
// delivered as its own prompt the moment it is emitted — the halt and the
// failed move, the two verdicts an operator cannot afford to learn about in
// a batch. Every other kind batches.
func ImmediateReport(r EventRow) bool {
	return r.Kind == EventHalted || r.Kind == EventMoveFailed
}

// EventRowSignature is the dedupe key an immediate send records under: kind,
// reason, and identity fields canonicalized — parameter ORDER cannot divide
// one fact into two signatures.
func EventRowSignature(r EventRow) string {
	var b strings.Builder
	b.WriteString(string(r.Kind))
	b.WriteByte('|')
	b.WriteString(r.ReasonCode)
	b.WriteByte('|')
	b.WriteString(r.SessionKey)
	b.WriteByte('|')
	b.WriteString(r.From)
	b.WriteByte('|')
	b.WriteString(r.To)
	b.WriteByte('|')
	keys := make([]string, 0, len(r.Params))
	for k := range r.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(r.Params[k])
		b.WriteByte(';')
	}
	return b.String()
}

// ReportLine renders one row as the fixed template:
//
//	[af handoff] kind=<kind> reason=<code> session="<title>" key=<key>
//	agent=<agent> from=<from> to=<to> mode=<mode> params=<k=v;k=v>
//
// Fields arrive already sanitized by the event-log writer (title capped and
// control-stripped); the template quotes the title so spaces cannot mangle
// the key=value scan.
func ReportLine(r EventRow) string {
	var b strings.Builder
	b.WriteString("[af handoff] kind=")
	b.WriteString(string(r.Kind))
	b.WriteString(" reason=")
	b.WriteString(r.ReasonCode)
	if r.Title != "" {
		b.WriteString(" session=\"")
		b.WriteString(r.Title)
		b.WriteByte('"')
	}
	if r.SessionKey != "" {
		b.WriteString(" key=")
		b.WriteString(r.SessionKey)
	}
	if r.Agent != "" {
		b.WriteString(" agent=")
		b.WriteString(r.Agent)
	}
	if r.From != "" {
		b.WriteString(" from=")
		b.WriteString(r.From)
	}
	if r.To != "" {
		b.WriteString(" to=")
		b.WriteString(r.To)
	}
	b.WriteString(" mode=")
	b.WriteString(r.Mode)
	if len(r.Params) > 0 {
		keys := make([]string, 0, len(r.Params))
		for k := range r.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(" params=")
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(';')
			}
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(r.Params[k])
		}
	}
	return b.String()
}

// ReportDigest renders the once-a-minute batch: a header with the count,
// then one ReportLine per row.
func ReportDigest(rows []EventRow) string {
	var b strings.Builder
	b.WriteString("[af handoff digest] ")
	b.WriteString(strconv.Itoa(len(rows)))
	if len(rows) == 1 {
		b.WriteString(" event\n")
	} else {
		b.WriteString(" events\n")
	}
	for i, row := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ReportLine(row))
	}
	return b.String()
}

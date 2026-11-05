package handoff

// report.go renders the report_to surface (#6776, R15): a FIXED structured
// template over EventRow fields — the only text a report may carry. Pane,
// transcript, and composer text can never appear in one, same rule as the
// event log itself. Every row batches through the digest — shadow mode has
// no immediate-delivery path; that returns with the live mover in slice 6,
// where halted and move-failed actually exist (#6818 review).

import (
	"sort"
	"strconv"
	"strings"
)

// ReportLine renders one row as the fixed template:
//
//	[af handoff] kind=<kind> reason=<code> session="<title>" key=<key>
//	agent=<agent> from=<from> to=<to> mode=<mode> params=<k=v;k=v>
//
// Every interpolated field passes through the same cap/control-strip
// normalizer the event log applies — the writer sanitizes its own copies,
// so the rows queued for reporting must be normalized here instead (#6818
// review). The template quotes the title so spaces cannot mangle the
// key=value scan; the quoted value escapes quote and backslash so a raw
// title cannot close the field early and forge a `kind=halted` tail.
func ReportLine(r EventRow) string {
	var b strings.Builder
	b.WriteString("[af handoff] kind=")
	b.WriteString(sanitizeEventLabel(string(r.Kind)))
	b.WriteString(" reason=")
	b.WriteString(sanitizeEventLabel(r.ReasonCode))
	if r.Title != "" {
		b.WriteString(" session=\"")
		b.WriteString(escapeReportQuoted(r.Title))
		b.WriteByte('"')
	}
	if r.SessionKey != "" {
		b.WriteString(" key=")
		b.WriteString(sanitizeEventLabel(r.SessionKey))
	}
	if r.Agent != "" {
		b.WriteString(" agent=")
		b.WriteString(sanitizeEventLabel(r.Agent))
	}
	if r.From != "" {
		b.WriteString(" from=")
		b.WriteString(sanitizeEventLabel(r.From))
	}
	if r.To != "" {
		b.WriteString(" to=")
		b.WriteString(sanitizeEventLabel(r.To))
	}
	b.WriteString(" mode=")
	b.WriteString(sanitizeEventLabel(r.Mode))
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
			b.WriteString(sanitizeEventLabel(k))
			b.WriteByte('=')
			b.WriteString(sanitizeEventLabel(r.Params[k]))
		}
	}
	return b.String()
}

// escapeReportQuoted sanitizes a value for the template's quoted slot:
// control-stripped and capped like every field, then quote/backslash
// escaped so the rendered value cannot terminate the field early. A title
// containing `" kind=halted` therefore stays inside its quotes instead of
// forging report fields (#6818 review).
func escapeReportQuoted(s string) string {
	s = sanitizeEventLabel(s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// reportDigestMaxRows and reportDigestMaxBytes bound one digest (#6818
// review): a fleet-wide reannouncement must never become one huge prompt —
// past the caps the digest reports the overflow count instead of the rows.
const (
	reportDigestMaxRows  = 24
	reportDigestMaxBytes = 8 << 10
)

// ReportDigest renders the once-a-minute batch: a header with the count,
// then one ReportLine per row, truncated to the row/byte caps with an
// overflow marker naming how many rows were not carried.
func ReportDigest(rows []EventRow) string {
	var b strings.Builder
	b.WriteString("[af handoff digest] ")
	b.WriteString(strconv.Itoa(len(rows)))
	if len(rows) == 1 {
		b.WriteString(" event\n")
	} else {
		b.WriteString(" events\n")
	}
	shown := 0
	for i, row := range rows {
		if shown >= reportDigestMaxRows {
			break
		}
		line := ReportLine(row)
		if b.Len()+len(line)+1 > reportDigestMaxBytes {
			break
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		shown++
	}
	if shown < len(rows) {
		b.WriteString("\n… +")
		b.WriteString(strconv.Itoa(len(rows) - shown))
		b.WriteString(" more not shown")
	}
	return b.String()
}

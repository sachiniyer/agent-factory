package ui

import (
	"strings"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/quota"
)

// The Usage section of the config overlay (#4361) — the TUI face of
// `af quota`, rendered from the daemon's QuotaReport so all three surfaces
// (CLI, TUI, web) show the same wording of the same evidence.
//
// READ-ONLY, AND NOT CONFIG. Usage is evidence about the attached host's
// sessions — what af has seen parked at a usage-limit wall, and when it saw it —
// not a setting this form can change. It therefore gets its own heading and its
// own row shape rather than rendering like a key, and it sits above Accounts:
// both are daemon-reported state rather than config entries, and the usage
// answer ("is anything parked?") is the one an operator opening this overlay
// after a dispatch failure is looking for.
//
// The pane holds no projection of its own. The daemon renders the rows —
// quota.Row is the wire shape — so a TUI attached to a remote daemon shows THAT
// host's walls, and no local session record is ever consulted for it. The note
// and caveats ride the same response: the note is the standing "QUOTA is what
// the provider reports, OBSERVED is what af saw" framing, and a caveat is an
// under-read warning that must never let a partial report look complete.

// usageSection is the pane's usage state: the rows the daemon rendered, the
// framing note, and the under-read caveats — all carried verbatim.
type usageSection struct {
	rows    []quota.Row
	note    string
	caveats []string

	loaded  bool
	loading bool
	// unavailable is why the report could not be read, rendered in place of the
	// rows. A silently empty section reads as "no limits anywhere" — the
	// confident answer a failed read must never produce.
	unavailable string
}

// usageHeading is what the section calls itself.
const usageHeading = "Usage"

// usageHeadingNote is the one-line gloss under the heading: which host's
// sessions this is evidence about. The full QUOTA/OBSERVED framing travels in
// the report's note field, rendered below it.
const usageHeadingNote = "What af has seen in this host's own sessions · read-only"

// SetUsage loads the section from the daemon's report. An error replaces the
// rows rather than emptying them silently — a report that cannot be read is
// itself the thing the operator needs to see.
func (c *ConfigPane) SetUsage(resp daemon.QuotaReportResponse, err error) {
	c.usage.loading = false
	c.usage.loaded = true
	c.usage.unavailable = ""
	c.usage.rows = nil
	c.usage.note = ""
	c.usage.caveats = nil
	if err != nil {
		c.usage.unavailable = err.Error()
	} else {
		c.usage.rows = resp.Rows
		c.usage.note = resp.Note
		c.usage.caveats = resp.Caveats
	}
	c.rebuildRows()
}

// SetUsageLoading clears an earlier opening's rows while a remote read runs.
func (c *ConfigPane) SetUsageLoading() {
	c.SetUsage(daemon.QuotaReportResponse{}, nil)
	c.usage.loading = true
}

// UsageLoaded reports whether the section has been initialized. Before the
// first read completes it stays hidden, so an overlay never flashes a Usage
// heading with nothing under it on a fast local load.
func (c *ConfigPane) UsageLoaded() bool { return c.usage.loaded }

// appendUsageRows flattens the section into the pane's row list, between the
// config tiers and the Accounts section. Usage rows carry no cursor and answer
// no key — they are evidence, not editable state.
func (c *ConfigPane) appendUsageRows() {
	if !c.usage.loaded {
		return
	}
	c.rows = append(c.rows, configRow{heading: usageHeading})
	for i := range c.usage.rows {
		row := c.usage.rows[i]
		c.rows = append(c.rows, configRow{usage: &row})
	}
}

// renderUsageHeadingLines renders everything under the Usage heading that is
// not a row: the loading/failure states, the framing note, and the caveats.
func (c *ConfigPane) renderUsageHeadingLines() []string {
	var lines []string
	split := func(s string) {
		lines = append(lines, strings.Split(strings.TrimSuffix(s, "\n"), "\n")...)
	}
	switch {
	case c.usage.loading:
		split(c.wrapIndented("Loading usage…", configHintStyle))
	case c.usage.unavailable != "":
		split(c.renderUsageUnavailable())
	default:
		if len(c.usage.rows) == 0 {
			split(c.wrapIndented("No agent CLIs are configured, so there is nothing to report.", configHintStyle))
			return lines
		}
		split(c.wrapIndented(usageHeadingNote, configHintStyle))
		if c.usage.note != "" {
			split(c.wrapIndented(c.usage.note, configHintStyle))
		}
		for _, caveat := range c.usage.caveats {
			split(c.wrapIndented("warning: "+caveat, configErrorStyle))
		}
	}
	return lines
}

// renderUsageRow renders one report row: the agent, its two verdicts, and the
// detail sentence — when it reset, when af saw it — wrapped under the row so a
// long observation never steals the line the agent name is on.
func (c *ConfigPane) renderUsageRow(row quota.Row) string {
	var b strings.Builder
	b.WriteString("  ")
	b.WriteString(configKeyStyle.Render(row.Agent))
	b.WriteString(configValueStyle.Render("  " + row.Quota + " · " + row.Observed))
	out := c.fitPaneLine(b.String()) + "\n"
	if row.Detail != "" {
		out += c.wrapIndented(row.Detail, configHintStyle)
	}
	return out
}

// renderUsageUnavailable renders the section's failure in place of rows — the
// same recovery block Accounts uses, so a daemon that cannot answer reads as a
// broken read, not as a host with no limits.
func (c *ConfigPane) renderUsageUnavailable() string {
	return DialogRecoveryContent("Cannot load usage", "The usage report could not be read: "+c.usage.unavailable, "Reopen settings to retry.", true, c.width)
}

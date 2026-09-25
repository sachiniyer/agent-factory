package ui

import (
	"strings"
)

// The Usage section (#2983) is the TUI's rendering of the daemon's QuotaReport:
// the same records→quota.Build read `af quota` prints, served pre-rendered so
// the pane displays the daemon's own words rather than re-deriving them.
//
// It lives in the config overlay for the same reason the Accounts section does
// (#3385): it is daemon-reported per-agent state, not a config key, so it rides
// under the manifest as a labelled section rather than impersonating a row you
// can edit. Its rows take the cursor without offering an action — selection is
// the list's only scroll driver, so a row the cursor cannot land on is a row
// the window cannot reach; enter does nothing because the row has no config
// entry. The detail line under each agent carries the whole answer.
const (
	quotaHeading     = "Usage"
	quotaHeadingNote = "What af can honestly say about each agent's quota — read on every open. " +
		"Quota is what the provider reports (today \"not reported\" everywhere); observed is what af's own sessions show. " +
		"af quota prints the same report."
)

// QuotaRow is one agent's line of the daemon's QuotaReport — the pane's own
// struct, like AccountRow, so ui/ carries no daemon dependency. The words are
// rendered server-side; the pane displays them, never re-derives them.
type QuotaRow struct {
	Program         string
	Quota           string
	Observed        string
	Sessions        int
	LimitedSessions int
	// ResetAt is the earliest recorded reset (RFC3339), empty when the parked
	// limit carried no reset time.
	ResetAt string
	Detail  string
}

// quotaSection mirrors accountsSection: the last fetched rows, the completeness
// warnings, and the read's own failure state — an absent answer must render as
// "af could not look", never as an empty section that reads as "all clear".
type quotaSection struct {
	rows        []QuotaRow
	warnings    []string
	loaded      bool
	loading     bool
	unavailable string
}

// SetQuota installs the daemon's report. A read failure becomes the section's
// own message — the config view is still useful when usage cannot be read, and
// an empty section would silently claim there is nothing parked.
func (c *ConfigPane) SetQuota(rows []QuotaRow, warnings []string, err error) {
	c.quota.loading = false
	c.quota.loaded = true
	if err != nil {
		c.quota.rows = nil
		c.quota.warnings = nil
		c.quota.unavailable = err.Error()
		c.rebuildRows()
		return
	}
	c.quota.rows = rows
	c.quota.warnings = warnings
	c.quota.unavailable = ""
	c.rebuildRows()
}

// SetQuotaLoading marks the section in-flight, matching the accounts section's
// loading row.
func (c *ConfigPane) SetQuotaLoading() {
	c.quota = quotaSection{loading: true}
	c.rebuildRows()
}

// appendQuotaRows appends the Usage heading and one row per agent. The section
// renders even when the read failed: an unavailable section that says so beats
// a silently absent one, which would look like "no usage to report".
func (c *ConfigPane) appendQuotaRows() {
	c.rows = append(c.rows, configRow{heading: quotaHeading})
	for i := range c.quota.rows {
		c.rows = append(c.rows, configRow{quota: &c.quota.rows[i]})
	}
}

// renderQuotaRow draws one agent line: cursor, name, "quota · observed" on the
// row and the daemon's detail sentence indented under it. The detail stays
// visible whether or not the row is selected — that is where the reset time
// lives, and hiding it behind selection would cost a parked session its answer.
func (c *ConfigPane) renderQuotaRow(i int, row QuotaRow) string {
	var b strings.Builder
	selected := i == c.selectedIdx

	cursor := "  "
	if selected {
		cursor = SelectionMarker("› ")
	}
	b.WriteString(cursor)

	programStyle := configKeyStyle
	if selected {
		programStyle = configSelectedStyle
	}
	b.WriteString(programStyle.Render(row.Program))
	b.WriteString(configValueStyle.Render("  " + row.Quota + " · "))
	stateStyle := configValueStyle
	switch {
	case selected:
		stateStyle = configSelectedStyle
	case row.LimitedSessions > 0:
		// A parked session is the report's whole point; it gets the dedicated
		// limit color the rail already uses for the same state.
		stateStyle = configLimitStyle
	}
	b.WriteString(stateStyle.Render(row.Observed))
	line := c.fitPaneLine(b.String())
	b.Reset()
	b.WriteString(line)
	b.WriteString("\n")
	b.WriteString(c.wrapIndented(row.Detail, configHintStyle))
	return b.String()
}

// renderQuotaUnavailable is the section's failure body — the reason the read
// failed plus what to do, never a bare red line.
func (c *ConfigPane) renderQuotaUnavailable() string {
	return DialogRecoveryContent(
		"Cannot read usage limits",
		"The daemon could not build the report: "+c.quota.unavailable,
		"Run af quota for the same read on this host, or retry after the daemon answers.",
		false, c.width)
}

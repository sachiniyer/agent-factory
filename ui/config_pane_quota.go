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
	// quotaAccountsHeading is the per-account evidence sub-section's label,
	// rendered after the agent table (#6776). It reads "Account evidence" —
	// distinct from the agent-identity Accounts section's "Accounts" heading so
	// an operator never sees two "Accounts" headings, and descriptive enough
	// to signal the rows are per-account usage evidence (the daemon's own
	// vocabulary, matching the CLI's "ACCOUNTS" block, which the closing note
	// also names). It rides under the Usage section, never as a top-level pane
	// heading.
	quotaAccountsHeading = "Account evidence"
	// quotaAccountColHeader is the column header the CLI's RenderAccountSection
	// prints above the rows; the pane shows the same words so the reader can
	// name each column without guessing.
	quotaAccountColHeader = "ACCOUNT  OBSERVED  WINDOWS  DETAIL"
	// quotaAccountsNoCodex is the empty-table line the CLI prints when the
	// handoff machinery is engaged (so the section exists) but no codex account
	// is registered to record windows for.
	quotaAccountsNoCodex = "(no codex accounts registered)"
	// quotaAccountsFootNote is the closing explanation the CLI's
	// RenderAccountSection prints after the section, so the pane cannot
	// disagree with `af quota` about what the ACCOUNTS block is.
	quotaAccountsFootNote = "ACCOUNTS is what af recorded in each account's own rollout files — " +
		"observed evidence, never a provider claim. It appears only when " +
		"[account_handoff] mode is enabled (or its machinery left a state file), " +
		"and is codex-only today: no other agent writes an equivalent record. " +
		"mover: is the configured mode plus any halt the engine recorded."
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

// QuotaAccountRow is one registered account's line of the per-account evidence
// section (#6776) — the pane's own mirror of quota.RenderedAccount, like
// QuotaRow mirrors the agent table. The words are rendered server-side; the
// pane displays them, never re-derives them.
type QuotaAccountRow struct {
	// Agent is the agent the account belongs to ("codex" today).
	Agent string
	// Name is the account name as registered under the af home.
	Name string
	// Observed is the evidence verdict ("observed", "unknown", "no evidence").
	Observed string
	// Windows is the rendered window list ("primary 42%, secondary 4%") or
	// "—" when there is nothing to show. Never blank.
	Windows string
	// Detail is the row's sentence — when the newest claim was recorded, or
	// why the verdict is not "observed".
	Detail string
}

// quotaSection mirrors accountsSection: the last fetched rows, the completeness
// warnings, and the read's own failure state — an absent answer must render as
// "af could not look", never as an empty section that reads as "all clear".
type quotaSection struct {
	rows     []QuotaRow
	warnings []string
	// accounts is the per-account evidence section the daemon served (#6776),
	// the same rows `af quota`'s ACCOUNTS block prints. nil/empty under the
	// absent-[account_handoff] condition, so the section never materializes
	// for a home the feature never touched.
	accounts []QuotaAccountRow
	// mover is the served mover line — the configured auto-handoff mode
	// plus any recorded halt (R16: a halted mover is never silent). Empty
	// under the absent-table condition, so the line never materializes for
	// a home the feature never touched. It rides inside the accounts section
	// (matching the CLI's RenderAccountSection) rather than under the Usage
	// heading, so the web/TUI/CLI all show it in the same place.
	mover       string
	loaded      bool
	loading     bool
	unavailable string
}

// SetQuota installs the daemon's report. A read failure becomes the section's
// own message — the config view is still useful when usage cannot be read, and
// an empty section would silently claim there is nothing parked.
func (c *ConfigPane) SetQuota(rows []QuotaRow, warnings []string, accounts []QuotaAccountRow, mover string, err error) {
	c.quota.loading = false
	c.quota.loaded = true
	if err != nil {
		c.quota.rows = nil
		c.quota.warnings = nil
		c.quota.accounts = nil
		c.quota.mover = ""
		c.quota.unavailable = err.Error()
		c.rebuildRows()
		return
	}
	c.quota.rows = rows
	c.quota.warnings = warnings
	c.quota.accounts = accounts
	c.quota.mover = mover
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
// a silently absent one, which would look like "no usage to report". But a
// pane whose host never kicked a read off renders NO section — SetEntries alone
// is used by flows that have no quota fetcher, and a permanent "Loading
// usage…" there would be a worse lie than absence.
func (c *ConfigPane) appendQuotaRows() {
	if !c.quota.loading && !c.quota.loaded {
		return
	}
	c.rows = append(c.rows, configRow{heading: quotaHeading})
	for i := range c.quota.rows {
		c.rows = append(c.rows, configRow{quota: &c.quota.rows[i]})
	}
	// Per-account evidence section (#6776): the accounts evidence and the
	// mover line ride together after the agent table, exactly as the CLI's
	// RenderAccountSection prints them. The section appears only when the
	// daemon served accounts OR a mover line — the absent-table shape (neither)
	// is no section at all, byte-identical to before the machinery existed.
	if len(c.quota.accounts) == 0 && c.quota.mover == "" {
		return
	}
	c.rows = append(c.rows, configRow{quotaAccountsHeading: true})
	if len(c.quota.accounts) > 0 {
		for i := range c.quota.accounts {
			c.rows = append(c.rows, configRow{quotaAccount: &c.quota.accounts[i]})
		}
	} else {
		// The empty-table line is its own selectable row so the cursor can
		// reach the section (and reveal the mover line below it) even when no
		// codex account is registered — the same reachability rule the agent
		// rows already enforce.
		c.rows = append(c.rows, configRow{quotaAccountsEmpty: true})
	}
	c.rows = append(c.rows, configRow{quotaAccountsEnd: true})
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

// renderQuotaAccountRow draws one per-account evidence line (#6776): cursor,
// "agent/name · observed · windows" on the row and the daemon's detail sentence
// indented under it. Mirrors renderQuotaRow so a parked-account row reads the
// same way a parked-agent row does — the detail (the recorded time or the
// reason the verdict is not "observed") stays visible off-selection.
func (c *ConfigPane) renderQuotaAccountRow(i int, row QuotaAccountRow) string {
	var b strings.Builder
	selected := i == c.selectedIdx

	cursor := "  "
	if selected {
		cursor = SelectionMarker("› ")
	}
	b.WriteString(cursor)

	nameStyle := configKeyStyle
	if selected {
		nameStyle = configSelectedStyle
	}
	b.WriteString(nameStyle.Render(row.Agent + "/" + row.Name))
	// The row line carries only the agent/name: it is the short identifier a
	// reader scans first, and clipping it loses at most a long account name.
	// observed · windows ride on their own wrapped line below rather than the
	// clipped row line, because at a narrow pane — or a name approaching the
	// 64-character limit — fitPaneLine truncates from the right and would
	// discard the verdict and window shares the section exists to show (#6776).
	line := c.fitPaneLine(b.String())
	b.Reset()
	b.WriteString(line)
	b.WriteString("\n")
	b.WriteString(c.wrapIndented(row.Observed+" · "+row.Windows, configValueStyle))
	b.WriteString(c.wrapIndented(row.Detail, configHintStyle))
	return b.String()
}

// renderQuotaAccountsEmpty draws the empty-table line the CLI prints when the
// section exists (the handoff machinery is engaged) but no codex account is
// registered. It is selectable so the cursor can reach the section and reveal
// the mover line trailing it — the same reachability rule the agent rows keep.
func (c *ConfigPane) renderQuotaAccountsEmpty(i int) string {
	var b strings.Builder
	selected := i == c.selectedIdx
	cursor := "  "
	if selected {
		cursor = SelectionMarker("› ")
	}
	b.WriteString(cursor)
	style := configHintStyle
	if selected {
		style = configSelectedStyle
	}
	b.WriteString(style.Render(quotaAccountsNoCodex))
	return c.fitPaneLine(b.String())
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

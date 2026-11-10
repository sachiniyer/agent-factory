package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
)

// The Usage section of the config overlay (#2983). What these pin is the part
// a screenshot cannot: that a quota row is not a config row — nothing on it
// opens the editor — but the cursor MUST reach it, because selection is the
// list's only scroll driver and a row the cursor cannot land on is a section
// the user can never scroll to. And that the report's honesty rules survive
// the surface change: "not reported" never reads as a zero, a failed read
// never renders as an empty section, and a partial report always carries its
// warning.

// quotaPane builds a pane with one config key and a populated Usage section.
func quotaPane(t *testing.T, rows []QuotaRow, warnings []string) *ConfigPane {
	t.Helper()
	pane := NewConfigPane()
	pane.SetSize(100, 40)
	pane.SetEntries([]config.ConfigEntry{{
		Key: "default_program", Value: "claude", Purpose: "the agent a new session runs", Tier: 1,
	}}, "/tmp/config.toml", nil)
	pane.SetAccounts(nil, nil, nil)
	pane.SetQuota(rows, warnings, nil, "", nil)
	pane.SetFocus(true)
	return pane
}

func TestConfigPaneQuota_RendersEveryAgentRowWithItsDetail(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "claude", Quota: "not reported", Observed: "no limit seen", Sessions: 2, Detail: "2 session(s) running, none parked at a limit"},
		{Program: "codex", Quota: "not reported", Observed: "limit reached", Sessions: 3, LimitedSessions: 1,
			ResetAt: "2026-01-01T10:00:00Z", Detail: "1 of 3 session(s) parked at a usage limit; earliest reset 2026-01-01T10:00:00Z (in 5m0s)"},
	}, nil)

	out := pane.String()
	for _, want := range []string{"Usage", "claude", "not reported", "no limit seen", "codex", "limit reached", "parked at a usage limit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered pane is missing %q:\n%s", want, out)
		}
	}
}

// Quota rows are selectable purely so the window can reach them: the cursor is
// the only scroll driver this list has, so a Usage section the cursor cannot
// land on below the last editable row renders but never scrolls into view.
// Reproduced in the play-test container — the cursor dead-ended on the last
// Accounts register row with the whole Usage section under "↓ n more".
func TestConfigPaneQuota_RowsAreSelectableSoTheWindowCanReachThem(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "claude", Quota: "not reported", Observed: "no sessions", Detail: "configured, but af has no sessions running it"},
	}, nil)

	var quotaIdx = -1
	for i, row := range pane.rows {
		if row.quota != nil {
			if !row.isSelectable() {
				t.Fatalf("quota row %d is not selectable: the cursor could never scroll the Usage section into view", i)
			}
			if quotaIdx < 0 {
				quotaIdx = i
			}
		}
	}
	if quotaIdx < 0 {
		t.Fatal("no quota rows")
	}
	// Navigation must be able to land on them, not just declare them.
	pane.selectedIdx = 0
	for pane.selectedIdx < quotaIdx {
		before := pane.selectedIdx
		pane.move(1)
		if pane.selectedIdx == before {
			t.Fatalf("navigation stalled at %d before reaching quota row %d", before, quotaIdx)
		}
	}
	// Selecting a quota row opens nothing: there is no entry to edit and no
	// account to log into.
	pane.selectedIdx = quotaIdx
	if pane.selectedEntry() != nil || pane.selectedAccount() != nil {
		t.Fatal("a quota row must carry neither a config entry nor an account")
	}
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if pane.IsEditing() {
		t.Fatal("enter on a quota row must not open the editor")
	}
}

// The failure must render as the section's own message — an absent section
// would read as "no usage to report", the fabricated-answer shape this report
// exists to refuse.
func TestConfigPaneQuota_FailedReadRendersUnavailableRatherThanEmpty(t *testing.T) {
	pane := quotaPane(t, nil, nil)
	pane.SetQuota(nil, nil, nil, "", errors.New("daemon: cannot read session records: boom"))

	out := pane.String()
	if !strings.Contains(out, "Cannot read usage limits") || !strings.Contains(out, "boom") {
		t.Fatalf("a failed read must surface its reason:\n%s", out)
	}
}

// The served mover line is fleet state the section must not drop: a recorded
// halt renders where the operator opens the config view (#6776 R16), and an
// absent line renders nothing — the absent-table shape stays no line at all.
// After the accounts section wiring, the mover rides in the per-account
// evidence section (matching the CLI's RenderAccountSection), so this also
// confirms the section materializes for the mover-only case.
func TestConfigPaneQuota_MoverLineRendersWhenServed(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "codex", Quota: "not reported", Observed: "limit reached", Sessions: 1, LimitedSessions: 1, Detail: "parked"},
	}, nil)
	pane.SetQuota(pane.quota.rows, nil, nil, "halted: move-failed since 2026-01-01T09:00:00Z", nil)

	out := pane.String()
	if !strings.Contains(out, "mover: halted: move-failed") {
		t.Fatalf("a served mover line must render in the Usage section:\n%s", out)
	}
}

func TestConfigPaneQuota_NoMoverLineWhenUnserved(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "codex", Quota: "not reported", Observed: "no sessions", Detail: "none"},
	}, nil)

	out := pane.String()
	if strings.Contains(out, "mover:") {
		t.Fatalf("the absent-table shape is no mover line at all:\n%s", out)
	}
}

// A partial report must carry its caveat where the reader sees it.
func TestConfigPaneQuota_WarningsRenderUnderTheHeading(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "claude", Quota: "not reported", Observed: "no limit seen", Sessions: 1, Detail: "1 session(s) running, none parked at a limit"},
	}, []string{"1 project record file(s) could not be read, so this report is INCOMPLETE"})

	out := pane.String()
	if !strings.Contains(out, "INCOMPLETE") {
		t.Fatalf("an incomplete report must say so on screen:\n%s", out)
	}
}

// The section is ordered before Accounts — both are daemon-reported state
// under the manifest, newest last, but Usage's rows take the cursor (the
// list's only scroll driver), so it must sit AHEAD of Accounts to keep the
// account register rows the list's last selectable rows. Were Usage after
// Accounts, navigate-to-bottom would land on a quota row where Enter is a
// no-op instead of on a register row that opens the inline field.
func TestConfigPaneQuota_RendersBeforeAccounts(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "claude", Quota: "not reported", Observed: "no sessions", Detail: "no sessions"},
	}, nil)

	var accountsIdx, quotaIdx int = -1, -1
	for i, row := range pane.rows {
		if row.heading == accountsHeading {
			accountsIdx = i
		}
		if row.heading == quotaHeading {
			quotaIdx = i
		}
	}
	if accountsIdx < 0 || quotaIdx < 0 || quotaIdx > accountsIdx {
		t.Fatalf("Usage must render before Accounts: accounts at %d, quota at %d", accountsIdx, quotaIdx)
	}
}

// registerQuotaPane builds a config pane shaped like an ordinary successful open
// of the config overlay: the manifest tiers, the Accounts section offering a
// register row for every agent on the roster, and — when withQuota is set — the
// Usage section the daemon always returns one row per supported agent CLI for.
// Sized tall so the window does not clip the bottom of the list.
//
// This is the shape the bug hid in: every test that exercised the
// navigate-to-bottom register drive ran against a pane this helper's
// withQuota=false arm describes, so the Usage section was absent and the
// register rows were last by accident, not by the ordering the invariant pins.
func registerQuotaPane(t *testing.T, agents []string, quota []QuotaRow) *ConfigPane {
	t.Helper()
	pane := NewConfigPane()
	pane.SetSize(100, 200)
	pane.SetEntries([]config.ConfigEntry{{
		Key: "default_program", Value: "claude", Purpose: "the agent a new session runs", Tier: 1,
	}}, "/tmp/config.toml", nil)
	pane.SetAccounts(nil, agents, nil)
	if quota != nil {
		pane.SetQuota(quota, nil, nil, "", nil)
	}
	pane.SetFocus(true)
	return pane
}

// driveToBottom moves the cursor down until it stops advancing — the
// navigate-to-bottom drive the register flow relies on (and that
// app/account_busy_test.go's accountPaneLastRow blind-fires 100 Down keys to
// park). Returns the index it parked on.
func driveToBottom(t *testing.T, c *ConfigPane) int {
	t.Helper()
	for i := 0; i < 200; i++ {
		before := c.selectedIdx
		pressKey(c, "down")
		if c.selectedIdx == before {
			break
		}
	}
	return c.selectedIdx
}

// describeConfigRow labels a row for a failure message, naming the section it
// belongs to so a wrong last-selectable row reads as the section that owns it
// rather than an opaque struct dump.
func describeConfigRow(row configRow) string {
	switch {
	case row.account != nil:
		if row.account.Register {
			return "register row for " + row.account.Agent
		}
		return "account row " + row.account.Agent + "/" + row.account.Name
	case row.quota != nil:
		return "quota row " + row.quota.Program
	case row.host != nil:
		if row.host.Action != "" {
			return "host action " + row.host.Action
		}
		return "host row " + row.host.Name
	case row.entry != nil:
		return "config row " + row.entry.Key
	case row.heading != "":
		return "heading " + row.heading
	}
	return "empty row"
}

// TestConfigPaneRegisterRowsAreLastSelectableWithQuota pins the invariant the bug
// broke: with the Usage section loaded (the default state on a successful open —
// the daemon reports a row per supported agent CLI), the account register rows
// must stay the list's last selectable rows. Were Usage appended after Accounts,
// the last selectable row would be a quota row, where Enter is a silent no-op, and
// the navigate-to-bottom register drive would dead-end — which is exactly the
// collision the e7d224c5 commit introduced. Usage therefore sits AHEAD of Accounts.
func TestConfigPaneRegisterRowsAreLastSelectableWithQuota(t *testing.T) {
	pane := registerQuotaPane(t,
		[]string{"codex"},
		[]QuotaRow{{Program: "claude", Quota: "not reported", Observed: "no sessions", Detail: "none"}},
	)

	lastSelectable := -1
	for i, row := range pane.rows {
		if row.isSelectable() {
			lastSelectable = i
		}
	}
	if lastSelectable < 0 {
		t.Fatal("no selectable rows in the pane")
	}
	row := pane.rows[lastSelectable]
	if row.account == nil || !row.account.Register {
		t.Fatalf("the last selectable row must be a register row — the register flow navigates to the bottom; "+
			"row %d is %s", lastSelectable, describeConfigRow(row))
	}
}

// The invariant must hold at realistic scale too: the daemon's quota report is
// never empty (one row per supported agent CLI — 7 minimum), so several quota
// rows below the register rows is the ordinary shape, not an edge case.
func TestConfigPaneRegisterRowsAreLastSelectableWithManyQuotaRows(t *testing.T) {
	pane := registerQuotaPane(t,
		[]string{"codex"},
		[]QuotaRow{
			{Program: "claude", Quota: "not reported", Observed: "no sessions", Detail: "none"},
			{Program: "codex", Quota: "not reported", Observed: "no sessions", Detail: "none"},
			{Program: "gemini", Quota: "not reported", Observed: "no sessions", Detail: "none"},
			{Program: "cursor", Quota: "not reported", Observed: "no sessions", Detail: "none"},
			{Program: "copilot", Quota: "not reported", Observed: "no sessions", Detail: "none"},
			{Program: "aider", Quota: "not reported", Observed: "no sessions", Detail: "none"},
			{Program: "opencode", Quota: "not reported", Observed: "no sessions", Detail: "none"},
		},
	)

	lastSelectable := -1
	for i, row := range pane.rows {
		if row.isSelectable() {
			lastSelectable = i
		}
	}
	row := pane.rows[lastSelectable]
	if row.account == nil || !row.account.Register {
		t.Fatalf("with seven quota rows the last selectable row must STILL be a register row; "+
			"row %d is %s", lastSelectable, describeConfigRow(row))
	}
}

// TestConfigPaneNavigateToBottomOpensRegisterWithQuota is the end-to-end symptom
// of the invariant: walking the cursor to the bottom and pressing Enter must
// land on a register row and open the inline register field. Before the fix
// this dead-ended on the last quota row, where Enter is a documented no-op while
// the footer advertised a "↵ edit" verb that did nothing.
func TestConfigPaneNavigateToBottomOpensRegisterWithQuota(t *testing.T) {
	pane := registerQuotaPane(t,
		[]string{"codex"},
		[]QuotaRow{{Program: "claude", Quota: "not reported", Observed: "no sessions", Detail: "none"}},
	)
	pane.selectedIdx = 0
	driveToBottom(t, pane)

	row := pane.rows[pane.selectedIdx]
	if row.account == nil || !row.account.Register {
		t.Fatalf("the bottom selectable row must be a register row, got %s at idx %d",
			describeConfigRow(row), pane.selectedIdx)
	}
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if !pane.IsEditing() {
		t.Fatalf("enter at the bottom must open the inline register field; "+
			"landed on %s (idx %d) and IsEditing stayed false", describeConfigRow(row), pane.selectedIdx)
	}
	if !pane.accounts.registering || pane.accounts.registerFor != "codex" {
		t.Fatalf("the register field opened for the wrong agent: registering=%v registerFor=%q",
			pane.accounts.registering, pane.accounts.registerFor)
	}
}

// The no-quota regression: the navigate-to-bottom register drive worked before
// the Usage section existed, and the fix must not break it. This is the shape
// every app-level test runs against (none loads quota rows), so it must stay
// green.
func TestConfigPaneNavigateToBottomOpensRegisterWithoutQuota(t *testing.T) {
	pane := registerQuotaPane(t, []string{"codex"}, nil)
	pane.selectedIdx = 0
	driveToBottom(t, pane)

	row := pane.rows[pane.selectedIdx]
	if row.account == nil || !row.account.Register {
		t.Fatalf("the bottom selectable row must be a register row, got %s at idx %d",
			describeConfigRow(row), pane.selectedIdx)
	}
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if !pane.IsEditing() {
		t.Fatalf("enter at the bottom must open the inline register field; "+
			"landed on %s (idx %d) and IsEditing stayed false", describeConfigRow(row), pane.selectedIdx)
	}
}

// The per-account evidence section (#6776): the daemon pre-renders the same
// rows `af quota`'s ACCOUNTS block prints, and the pane displays the daemon's
// own words verbatim — agent/name, observed, windows, detail — after the agent
// table. This is the bug the wiring fixed: the daemon served the field but the
// TUI dropped it, so an operator with handoff engaged saw the evidence only via
// `af quota`, never in the config view.
func TestConfigPaneQuota_AccountsEvidenceRendersAfterAgentRows(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "codex", Quota: "not reported", Observed: "limit reached", Sessions: 1, LimitedSessions: 1, Detail: "parked"},
	}, nil)
	pane.SetQuota(pane.quota.rows, nil, []QuotaAccountRow{
		{Agent: "codex", Name: "work", Observed: "observed", Windows: "primary 42%, secondary 4%", Detail: "recorded 2026-01-01T09:00:00Z"},
		{Agent: "codex", Name: "personal", Observed: "no evidence", Windows: "—", Detail: "no rate_limits claims found"},
	}, "shadow", nil)

	out := pane.String()
	// The section label, column header, and both rows' rendered words must all
	// appear — a client that re-derives vocabulary from the rows is exactly the
	// drift the served-rendered-rows design prevents.
	for _, want := range []string{
		quotaAccountsHeading,
		quotaAccountColHeader,
		"codex/work",
		"observed",
		"primary 42%, secondary 4%",
		"recorded 2026-01-01T09:00:00Z",
		"codex/personal",
		"no evidence",
		"no rate_limits claims found",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("per-account evidence section is missing %q:\n%s", want, out)
		}
	}
	// The mover rides inside the section, matching the CLI's RenderAccountSection.
	if !strings.Contains(out, "mover: shadow") {
		t.Fatalf("the mover line must render inside the per-account evidence section:\n%s", out)
	}
	// The closing explanation the CLI prints must ride after the rows.
	if !strings.Contains(out, "ACCOUNTS is what af recorded") {
		t.Fatalf("the section's closing note must render:\n%s", out)
	}
}

// Per-account evidence rows take the cursor for the same scroll-reach reason
// the agent rows do: selection is the only scroll driver this list has, so a
// non-selectable section the cursor cannot land on renders but never scrolls
// into view. Enter does nothing — there is no entry to edit.
func TestConfigPaneQuota_AccountRowsAreSelectable(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "codex", Quota: "not reported", Observed: "no sessions", Detail: "none"},
	}, nil)
	pane.SetQuota(pane.quota.rows, nil, []QuotaAccountRow{
		{Agent: "codex", Name: "work", Observed: "observed", Windows: "primary 42%", Detail: "recorded 2026-01-01T09:00:00Z"},
	}, "", nil)

	var acctIdx = -1
	for i, row := range pane.rows {
		if row.quotaAccount != nil {
			if !row.isSelectable() {
				t.Fatalf("account row %d is not selectable: the cursor could never scroll the section into view", i)
			}
			acctIdx = i
		}
	}
	if acctIdx < 0 {
		t.Fatal("no account rows")
	}
	// Navigation must be able to land on the account row, not just declare it.
	pane.selectedIdx = 0
	for pane.selectedIdx < acctIdx {
		before := pane.selectedIdx
		pane.move(1)
		if pane.selectedIdx == before {
			t.Fatalf("navigation stalled at %d before reaching account row %d", before, acctIdx)
		}
	}
	pane.selectedIdx = acctIdx
	if pane.selectedEntry() != nil || pane.selectedAccount() != nil {
		t.Fatal("an account evidence row must carry neither a config entry nor an account")
	}
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if pane.IsEditing() {
		t.Fatal("enter on an account evidence row must not open the editor")
	}
}

// The empty-table line the CLI prints when the machinery is engaged but no
// codex account is registered must surface in the pane too — otherwise an
// operator with a recorded mover halt and zero accounts sees the halt line
// but no signal that the accounts section is the empty table, not absent.
func TestConfigPaneQuota_NoCodexAccountsLineWhenMoverServedButNoAccounts(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "codex", Quota: "not reported", Observed: "no sessions", Detail: "none"},
	}, nil)
	pane.SetQuota(pane.quota.rows, nil, nil, "halted: move-failed since 2026-01-01T09:00:00Z", nil)

	out := pane.String()
	if !strings.Contains(out, quotaAccountsNoCodex) {
		t.Fatalf("the empty-table line must render when the mover is served but no account is registered:\n%s", out)
	}
	if !strings.Contains(out, "mover: halted: move-failed") {
		t.Fatalf("the mover line must still render in the empty-accounts case:\n%s", out)
	}
}

// The absent-table shape — no accounts AND no mover — is no per-account
// evidence section at all, byte-identical to before the machinery existed. A
// home the feature never touched must not gain a phantom section.
func TestConfigPaneQuota_NoAccountsSectionWhenAbsentTable(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "codex", Quota: "not reported", Observed: "no sessions", Detail: "none"},
	}, nil)
	// No accounts, no mover — the absent-table condition.
	pane.SetQuota(pane.quota.rows, nil, nil, "", nil)

	out := pane.String()
	for _, absent := range []string{quotaAccountsHeading, quotaAccountColHeader, quotaAccountsNoCodex, "ACCOUNTS is what af recorded"} {
		if strings.Contains(out, absent) {
			t.Fatalf("the absent-table shape is no per-account evidence section at all: found %q\n%s", absent, out)
		}
	}
}

// The per-account evidence section's trailing rows (mover line + footnote) are
// each selectable, so the cursor can land on them; selection is the list's only
// scroll driver, so non-selectable trailing rows would sit permanently behind
// the "↓ more" cue when the cursor rests on the last selectable row above them.
// The selection span extends through the trailing rows so window() can scroll
// them into view — the reach the empty-table row's own comment already
// promises (#6776).
func TestConfigPaneQuota_TrailingAccountsEndRowReachableFromLastAccountRow(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "codex", Quota: "not reported", Observed: "no sessions", Detail: "none"},
	}, nil)
	pane.SetQuota(pane.quota.rows, nil, []QuotaAccountRow{
		{Agent: "codex", Name: "work", Observed: "observed", Windows: "primary 42%", Detail: "recorded 2026-01-01T09:00:00Z"},
		{Agent: "codex", Name: "personal", Observed: "no evidence", Windows: "—", Detail: "no rate_limits claims found"},
	}, "halted: move-failed since 2026-01-01T09:00:00Z", nil)

	var lastAcct, moverIdx, footIdx int = -1, -1, -1
	for i, row := range pane.rows {
		if row.quotaAccount != nil {
			lastAcct = i
		}
		if row.quotaAccountsMover {
			moverIdx = i
		}
		if row.quotaAccountsFoot {
			footIdx = i
		}
	}
	if lastAcct < 0 || moverIdx < 0 || footIdx < 0 {
		t.Fatalf("expected an account row followed by the mover and footnote rows: lastAcct=%d moverIdx=%d footIdx=%d", lastAcct, moverIdx, footIdx)
	}
	if moverIdx != lastAcct+1 || footIdx != moverIdx+1 {
		t.Fatalf("expected last account, then mover, then footnote: lastAcct=%d moverIdx=%d footIdx=%d", lastAcct, moverIdx, footIdx)
	}

	// Cursor on the last account row: the span must reach the end of all lines
	// (the trailing rows are last in the section), covering the mover line and
	// footnote so window() scrolls them into view rather than clipping them
	// behind "↓ more".
	pane.selectedIdx = lastAcct
	lines, selStart, selEnd := pane.renderRowLines()
	if selStart < 0 {
		t.Fatal("expected a selection span")
	}
	if selEnd != len(lines) {
		t.Fatalf("selection span must extend through the trailing rows: selEnd=%d lines=%d", selEnd, len(lines))
	}
}

// The same reach must hold for the empty-accounts shape: the empty-table row is
// the first selectable row in the section, and the mover line + footnote ride
// on the trailing selectable rows below it.
func TestConfigPaneQuota_TrailingAccountsEndRowReachableFromEmptyRow(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "codex", Quota: "not reported", Observed: "no sessions", Detail: "none"},
	}, nil)
	pane.SetQuota(pane.quota.rows, nil, nil, "halted: move-failed since 2026-01-01T09:00:00Z", nil)

	var emptyIdx, moverIdx, footIdx int = -1, -1, -1
	for i, row := range pane.rows {
		if row.quotaAccountsEmpty {
			emptyIdx = i
		}
		if row.quotaAccountsMover {
			moverIdx = i
		}
		if row.quotaAccountsFoot {
			footIdx = i
		}
	}
	if emptyIdx < 0 || moverIdx < 0 || footIdx < 0 {
		t.Fatalf("expected an empty row followed by the mover and footnote rows: emptyIdx=%d moverIdx=%d footIdx=%d", emptyIdx, moverIdx, footIdx)
	}
	if moverIdx != emptyIdx+1 || footIdx != moverIdx+1 {
		t.Fatalf("expected empty row, then mover, then footnote: emptyIdx=%d moverIdx=%d footIdx=%d", emptyIdx, moverIdx, footIdx)
	}
	pane.selectedIdx = emptyIdx
	lines, selStart, selEnd := pane.renderRowLines()
	if selStart < 0 {
		t.Fatal("expected a selection span")
	}
	if selEnd != len(lines) {
		t.Fatalf("selection span must extend through the trailing rows: selEnd=%d lines=%d", selEnd, len(lines))
	}
}

// At a narrow pane the composed account row once exceeded the pane and
// fitPaneLine truncated it from the right, discarding the verdict and window
// shares the section exists to show. observed · windows now ride on a wrapped
// (non-clipped) line so they survive at any width (#6776). Checked at the
// renderRowLines level so the list's window does not hide the row's lines
// behind a "↓ more" cue — the bug is in the row's own rendering, not the
// window.
func TestConfigPaneQuota_AccountRowObservedAndWindowsSurviveNarrowWidth(t *testing.T) {
	pane := NewConfigPane()
	pane.SetSize(30, 40)
	pane.SetEntries([]config.ConfigEntry{{
		Key: "default_program", Value: "claude", Purpose: "the agent a new session runs", Tier: 1,
	}}, "/tmp/config.toml", nil)
	pane.SetAccounts(nil, nil, nil)
	pane.SetQuota([]QuotaRow{
		{Program: "codex", Quota: "not reported", Observed: "no sessions", Detail: "none"},
	}, nil, []QuotaAccountRow{
		{Agent: "codex", Name: "work", Observed: "observed", Windows: "primary 42%, secondary 4%", Detail: "recorded 2026-01-01T09:00:00Z"},
	}, "shadow", nil)
	pane.SetFocus(true)

	lines, _, _ := pane.renderRowLines()
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	out := b.String()
	// observed and both window shares must survive — fitPaneLine used to drop
	// everything right of the name; wrapping keeps each fragment on its own line.
	for _, want := range []string{"codex/work", "observed", "primary 42%", "secondary 4%"} {
		if !strings.Contains(out, want) {
			t.Fatalf("narrow pane must not clip away %q:\n%s", want, out)
		}
	}
}

// The per-account evidence section renders AFTER the agent table, matching the
// CLI's RenderAccountSection (which prints after RenderRows) — a reader who
// knows `af quota` sees the same shape in the pane.
func TestConfigPaneQuota_AccountsSectionRendersAfterAgentRows(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "codex", Quota: "not reported", Observed: "limit reached", Sessions: 1, LimitedSessions: 1, Detail: "parked"},
	}, nil)
	pane.SetQuota(pane.quota.rows, nil, []QuotaAccountRow{
		{Agent: "codex", Name: "work", Observed: "observed", Windows: "primary 42%", Detail: "recorded 2026-01-01T09:00:00Z"},
	}, "shadow", nil)

	var lastAgentIdx, headingIdx int = -1, -1
	for i, row := range pane.rows {
		if row.quota != nil {
			lastAgentIdx = i
		}
		if row.quotaAccountsHeading {
			headingIdx = i
		}
	}
	if lastAgentIdx < 0 || headingIdx < 0 {
		t.Fatalf("expected an agent table followed by the accounts section: last agent %d, heading %d", lastAgentIdx, headingIdx)
	}
	if headingIdx <= lastAgentIdx {
		t.Fatalf("per-account evidence section must render after the agent table: last agent at %d, heading at %d", lastAgentIdx, headingIdx)
	}
}

// The trailing mover line and the closing footnote are each their own
// selectable row (#6776) — split from a single end row — so the cursor can
// land on each and window() can scroll each into view independently.
// Selection is the list's only scroll driver; a single row carrying both
// blocks left the footnote permanently behind the "↓ more" cue at a narrow or
// short terminal, because window() pins an over-tall selection to its start.
// Verified by rendering the span through window() with a budget smaller than
// the section, not just by inspecting selEnd: window() pins scrollTop to
// selStart for an over-tall span, which a selEnd-only check would miss. Both
// the mover line AND the footnote must be reachable this way — the prior
// single-row fix only asserted the mover line.
func TestConfigPaneQuota_TrailingAccountsEndRowSelectableAndScrollsIntoView(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "codex", Quota: "not reported", Observed: "no sessions", Detail: "none"},
	}, nil)
	pane.SetQuota(pane.quota.rows, nil, []QuotaAccountRow{
		{Agent: "codex", Name: "work", Observed: "observed", Windows: "primary 42%", Detail: "recorded 2026-01-01T09:00:00Z"},
	}, "halted: move-failed since 2026-01-01T09:00:00Z", nil)

	var lastAcct, moverIdx, footIdx int = -1, -1, -1
	for i, row := range pane.rows {
		if row.quotaAccount != nil {
			lastAcct = i
		}
		if row.quotaAccountsMover {
			moverIdx = i
		}
		if row.quotaAccountsFoot {
			footIdx = i
		}
	}
	if moverIdx < 0 || footIdx < 0 {
		t.Fatal("expected a trailing mover row and footnote row")
	}
	if !pane.rows[moverIdx].isSelectable() {
		t.Fatal("the trailing mover row must be selectable so the cursor can scroll it into view")
	}
	if !pane.rows[footIdx].isSelectable() {
		t.Fatal("the trailing footnote row must be selectable so the cursor can scroll it into view")
	}
	// Pressing down from the last account row must land on the mover row, not
	// skip past it into the Accounts section — the move the previous
	// non-selectable end row blocked.
	pane.selectedIdx = lastAcct
	pane.move(1)
	if pane.selectedIdx != moverIdx {
		t.Fatalf("down from the last account row must reach the mover row: got selectedIdx=%d want %d", pane.selectedIdx, moverIdx)
	}

	// Render through window() with a budget smaller than the section so the
	// over-tall-span branch is exercised. With the cursor on the mover row,
	// window() must reveal the mover line rather than clipping it behind the
	// "↓ more" cue.
	visible := windowSelectedLines(t, pane, moverIdx, 3)
	if !strings.Contains(visible, "mover:") {
		t.Fatalf("window() must reveal the mover line when the mover row is selected (budget=3):\n%s", visible)
	}

	// Pressing down from the mover row must land on the footnote row, not skip
	// past it — the reach the single end row denied the footnote at a narrow
	// or short terminal.
	pane.move(1)
	if pane.selectedIdx != footIdx {
		t.Fatalf("down from the mover row must reach the footnote row: got selectedIdx=%d want %d", pane.selectedIdx, footIdx)
	}
	// With the cursor on the footnote row, window() must reveal the closing
	// explanation rather than clipping it behind the "↓ more" cue — the
	// symptom the finding reported (the prior test asserted only mover:).
	visible = windowSelectedLines(t, pane, footIdx, 3)
	if !strings.Contains(visible, "ACCOUNTS is what af recorded") {
		t.Fatalf("window() must reveal the footnote when the footnote row is selected (budget=3):\n%s", visible)
	}
}

// windowSelectedLines renders the pane's row lines, sets the selection to the
// given row, and returns the lines window() reveals at the given budget — so a
// test can assert the cursor scrolls a specific trailing row into view rather
// than reading selEnd alone (which window()'s over-tall-span pin would hide).
func windowSelectedLines(t *testing.T, pane *ConfigPane, selectedIdx, budget int) string {
	t.Helper()
	pane.selectedIdx = selectedIdx
	pane.scrollTop = 0
	lines, selStart, selEnd := pane.renderRowLines()
	visible, _, _ := pane.window(lines, selStart, selEnd, budget)
	if selStart < 0 {
		t.Fatal("expected a selection span")
	}
	var b strings.Builder
	for _, l := range visible {
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

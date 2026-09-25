package ui

import (
	"errors"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
)

// The Usage section of the config overlay (#2983). What these pin is the part
// a screenshot cannot: that a quota row is not a config row — the cursor cannot
// land on it and nothing on it opens the editor — and that the report's honesty
// rules survive the surface change: "not reported" never reads as a zero, a
// failed read never renders as an empty section, and a partial report always
// carries its warning.

// quotaPane builds a pane with one config key and a populated Usage section.
func quotaPane(t *testing.T, rows []QuotaRow, warnings []string) *ConfigPane {
	t.Helper()
	pane := NewConfigPane()
	pane.SetSize(100, 40)
	pane.SetEntries([]config.ConfigEntry{{
		Key: "default_program", Value: "claude", Purpose: "the agent a new session runs", Tier: 1,
	}}, "/tmp/config.toml")
	pane.SetAccounts(nil, nil, nil)
	pane.SetQuota(rows, warnings, nil)
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

// A quota row must never take the cursor: it is a report line, and letting the
// selection land on it would either dead-end the keypress or imply an editor
// the section does not have.
func TestConfigPaneQuota_RowsAreNotSelectable(t *testing.T) {
	pane := quotaPane(t, []QuotaRow{
		{Program: "claude", Quota: "not reported", Observed: "no sessions", Detail: "configured, but af has no sessions running it"},
	}, nil)

	for i, row := range pane.rows {
		if row.quota != nil && row.isSelectable() {
			t.Fatalf("quota row %d is selectable: a report line must never take the cursor", i)
		}
	}
	// And navigation must skip them rather than stall: the last selectable row
	// stays reachable from the first.
	pane.selectedIdx = 0
	for i := 0; i < len(pane.rows); i++ {
		pane.move(1)
	}
	if pane.selectedIdx >= len(pane.rows) || !pane.rows[pane.selectedIdx].isSelectable() {
		t.Fatalf("navigation landed on index %d, a non-selectable row", pane.selectedIdx)
	}
}

// The failure must render as the section's own message — an absent section
// would read as "no usage to report", the fabricated-answer shape this report
// exists to refuse.
func TestConfigPaneQuota_FailedReadRendersUnavailableRatherThanEmpty(t *testing.T) {
	pane := quotaPane(t, nil, nil)
	pane.SetQuota(nil, nil, errors.New("daemon: cannot read session records: boom"))

	out := pane.String()
	if !strings.Contains(out, "Cannot read usage limits") || !strings.Contains(out, "boom") {
		t.Fatalf("a failed read must surface its reason:\n%s", out)
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

// The section is ordered after Accounts — both are daemon-reported state under
// the manifest, newest last.
func TestConfigPaneQuota_RendersAfterAccounts(t *testing.T) {
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
	if accountsIdx < 0 || quotaIdx < 0 || quotaIdx < accountsIdx {
		t.Fatalf("Usage must render after Accounts: accounts at %d, quota at %d", accountsIdx, quotaIdx)
	}
}

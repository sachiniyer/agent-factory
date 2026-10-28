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

package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/quota"
)

// The Usage section of the config overlay (#4361). What these pin is the part
// the wire shape exists for: the pane renders the daemon's OWN wording —
// quota.Row cells verbatim, note and caveats included — never a re-reading of
// the policy, and a failed or in-flight read can never pass for an empty one.

// usagePane builds a pane with one config key and a Usage section loaded from
// the given response, sized so rendering is not degenerate.
func usagePane(t *testing.T, resp daemon.QuotaReportResponse, err error) *ConfigPane {
	t.Helper()
	pane := NewConfigPane()
	pane.SetSize(110, 50)
	pane.SetEntries([]config.ConfigEntry{{
		Key: "default_program", Value: "claude", Purpose: "the agent a new session runs", Tier: 1,
	}}, "/tmp/config.toml")
	pane.SetUsage(resp, err)
	pane.SetFocus(true)
	return pane
}

func stubUsageReport() daemon.QuotaReportResponse {
	return daemon.QuotaReportResponse{
		Rows: []quota.Row{
			{Agent: "claude", Quota: "not reported", Observed: "no limit seen",
				Detail: "3 session(s) running, none parked at a limit"},
			{Agent: "codex", Quota: "not reported", Observed: "limit reached",
				Detail: "1 of 2 session(s) parked at a usage limit; earliest reset 2026-09-20T00:00:00Z (in 6h); observed 2026-09-14T00:00:00Z (6d ago)"},
		},
		Note: quota.ReportNote,
	}
}

// Every row the daemon rendered shows its own words — the agent, both verdict
// cells, and the detail sentence naming when the wall was observed — because
// the wire shape exists so the TUI cannot drift into its own reading.
func TestUsageSectionRendersTheDaemonsRowsVerbatim(t *testing.T) {
	pane := usagePane(t, stubUsageReport(), nil)
	view := pane.String()
	for _, fragment := range []string{
		"Usage",
		"claude", "not reported", "no limit seen", "none parked at a limit",
		"codex", "limit reached", "6d ago", "observed", "2026-",
		"QUOTA is what the provider reports",
	} {
		if !strings.Contains(view, fragment) {
			t.Errorf("the Usage section must render %q, got:\n%s", fragment, view)
		}
	}
}

// The section sits above Accounts — the order the issue asked for, and the
// order a reader hunting "is anything parked?" needs before the credentials.
func TestUsageSectionRendersAboveAccounts(t *testing.T) {
	pane := usagePane(t, stubUsageReport(), nil)
	pane.SetAccounts([]AccountRow{{Agent: "claude", Name: "work", LoggedIn: true}}, []string{"claude"}, nil)
	view := pane.String()
	usageAt := strings.Index(view, "Usage")
	accountsAt := strings.Index(view, "Accounts")
	if usageAt < 0 || accountsAt < 0 {
		t.Fatalf("both sections must render, got:\n%s", view)
	}
	if usageAt > accountsAt {
		t.Errorf("Usage must render above Accounts, got:\n%s", view)
	}
}

// A caveat is an under-read warning, not a footnote: it must reach the operator
// verbatim, styled as a warning, or a partial report reads as a complete one.
func TestUsageSectionCarriesTheDaemonsCaveats(t *testing.T) {
	resp := stubUsageReport()
	resp.Caveats = []string{"1 project record file(s) could not be parsed and were skipped, so this report is INCOMPLETE"}
	pane := usagePane(t, resp, nil)
	if !strings.Contains(pane.String(), "INCOMPLETE") {
		t.Errorf("the caveat must reach the operator, got:\n%s", pane.String())
	}
}

// Usage rows are evidence, not state: the cursor never lands on one, and enter
// on the pane opens no editor over them. A selectable usage row would imply an
// action the section does not have.
func TestUsageRowsAreNotSelectable(t *testing.T) {
	pane := usagePane(t, stubUsageReport(), nil)
	for i, row := range pane.rows {
		if row.usage != nil && row.isSelectable() {
			t.Fatalf("usage row %d (%s) must not be selectable", i, row.usage.Agent)
		}
	}
	// And navigation skips them: j from the last config entry must land on a
	// selectable row past the whole section, not inside it.
	pane.selectedIdx = 0
	for !pane.rows[pane.selectedIdx].isSelectable() {
		pane.selectedIdx++
	}
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if row := pane.rows[pane.selectedIdx]; row.usage != nil || row.heading != "" {
		t.Fatalf("navigation must skip usage rows and headings, landed on %+v", row)
	}
}

// A failed read is a failure line, not an empty section — "no rows" and
// "cannot read" need different actions from the operator, and rendering one as
// the other is how a dead daemon reads as a healthy host.
func TestUsageSectionShowsAFailureInPlace(t *testing.T) {
	pane := usagePane(t, daemon.QuotaReportResponse{}, errors.New("dial: no daemon"))
	view := pane.String()
	if !strings.Contains(view, "Cannot load usage") || !strings.Contains(view, "dial: no daemon") {
		t.Errorf("a failed read must say so, got:\n%s", view)
	}
	if strings.Contains(view, "no limit seen") {
		t.Errorf("a failed read must not render rows, got:\n%s", view)
	}
}

// Before the first answer the section says it is loading — a blank section
// under a remote target would read as "no limits anywhere" for as long as the
// daemon takes to answer.
func TestUsageSectionShowsLoading(t *testing.T) {
	pane := NewConfigPane()
	pane.SetSize(110, 50)
	pane.SetEntries([]config.ConfigEntry{{
		Key: "default_program", Value: "claude", Purpose: "p", Tier: 1,
	}}, "/tmp/config.toml")
	pane.SetUsageLoading()
	pane.SetFocus(true)
	if !strings.Contains(pane.String(), "Loading usage…") {
		t.Errorf("a read in flight must say so, got:\n%s", pane.String())
	}
}

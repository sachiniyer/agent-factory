package ui

import (
	"fmt"
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/config"
)

// The config list scrolls only to reveal the selection, and window() pins a
// selection taller than the window to its top. On a 40x10 terminal the config
// overlay's content box is 34x6 (DialogStyle's border plus its 1x2 padding),
// which leaves a one-line list: every multi-line row is over-tall there. Until
// ↑/↓ could scroll within such a row, everything below its first line could
// never be brought into view — the account-evidence rows' observed · windows
// and detail lines, and the trailing mover line and footnote the selection span
// carries (#7056 review). These tests pin the property, not a mechanism: every
// line of the section renders in some frame of a plain ↓-then-↑ walk.

const (
	// overlayW40x10 and overlayH40x10 are the config overlay's content box on
	// a 40x10 terminal: 40 - 2 (border) - 4 (padding) by 10 - 2 - 2.
	overlayW40x10 = 34
	overlayH40x10 = 6
)

// evidencePane builds the pane the Usage section's account evidence renders in,
// at the given size, with accounts (nil for the empty-accounts shape) and a
// served mover line carrying a halt — the line R16 says is never silent.
func evidencePane(t *testing.T, width, height int, accounts []QuotaAccountRow) *ConfigPane {
	t.Helper()
	pane := NewConfigPane()
	pane.SetSize(width, height)
	pane.SetEntries([]config.ConfigEntry{{
		Key: "default_program", Value: "claude", Purpose: "the agent a new session runs", Tier: 1,
	}}, "/tmp/config.toml", nil)
	pane.SetAccounts(nil, []string{"codex"}, nil)
	pane.SetQuota([]QuotaRow{
		{Program: "codex", Quota: "not reported", Observed: "no sessions", Detail: "configured, but af has no sessions running it"},
	}, nil, accounts, "halted: move-failed since 2026-01-01T09:00:00Z", nil)
	pane.SetFocus(true)
	return pane
}

// walkFrames renders the pane the way the program does — once per key — while
// driving ↓ until neither the cursor nor the view moves any more, then ↑ back
// to the top. It returns every line any frame showed, ANSI-stripped and trimmed.
func walkFrames(t *testing.T, c *ConfigPane) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	record := func() {
		for _, l := range strings.Split(c.String(), "\n") {
			seen[strings.TrimSpace(xansi.Strip(l))] = true
		}
	}
	record()
	for _, key := range []string{"down", "up"} {
		for i := 0; ; i++ {
			if i > 1000 {
				t.Fatalf("%s never settled: the cursor or the view kept moving", key)
			}
			beforeIdx, beforeTop := c.selectedIdx, c.scrollTop
			pressKey(c, key)
			record()
			if c.selectedIdx == beforeIdx && c.scrollTop == beforeTop {
				break
			}
		}
	}
	return seen
}

// wrappedLines is what wrapIndented renders for text, ANSI-stripped and trimmed:
// the lines a frame must have shown for that text to have been readable.
func wrappedLines(c *ConfigPane, text string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSuffix(c.wrapIndented(text, configHintStyle), "\n"), "\n") {
		out = append(out, strings.TrimSpace(xansi.Strip(l)))
	}
	return out
}

func assertEveryLineShown(t *testing.T, seen map[string]bool, label string, want []string) {
	t.Helper()
	for _, w := range want {
		if w == "" {
			continue
		}
		if !seen[w] {
			t.Errorf("%s: line %q never rendered in any frame of a full ↓/↑ walk", label, w)
		}
	}
}

func TestConfigPaneQuota_EveryAccountEvidenceLineReachableAt40x10(t *testing.T) {
	accounts := []QuotaAccountRow{
		{Agent: "codex", Name: "work", Observed: "observed", Windows: "primary 42%, secondary 4%", Detail: "recorded 2026-01-01T09:00:00Z"},
		{Agent: "codex", Name: "personal", Observed: "no evidence", Windows: "—", Detail: "no rate_limits claims found in this account's rollout files"},
	}
	pane := evidencePane(t, overlayW40x10, overlayH40x10, accounts)
	seen := walkFrames(t, pane)

	for _, a := range accounts {
		assertEveryLineShown(t, seen, a.Name+" observed · windows", wrappedLines(pane, a.Observed+" · "+a.Windows))
		assertEveryLineShown(t, seen, a.Name+" detail", wrappedLines(pane, a.Detail))
	}
	assertEveryLineShown(t, seen, "mover line", wrappedLines(pane, "mover: "+pane.quota.mover))
	assertEveryLineShown(t, seen, "closing footnote", wrappedLines(pane, quotaAccountsFootNote))
}

// The empty-accounts shape: the empty-table row is the section's only stop, and
// the mover line and footnote ride on the non-selectable end row below it.
func TestConfigPaneQuota_EmptyAccountsTrailingEvidenceReachableAt40x10(t *testing.T) {
	pane := evidencePane(t, overlayW40x10, overlayH40x10, nil)
	seen := walkFrames(t, pane)

	assertEveryLineShown(t, seen, "mover line", wrappedLines(pane, "mover: "+pane.quota.mover))
	assertEveryLineShown(t, seen, "closing footnote", wrappedLines(pane, quotaAccountsFootNote))
}

// The same property one size up, where the list is a few lines tall rather
// than one: the footnote alone still outgrows it.
func TestConfigPaneQuota_TrailingEvidenceReachableOnAShortPane(t *testing.T) {
	for _, size := range [][2]int{{34, 10}, {34, 14}, {60, 12}} {
		pane := evidencePane(t, size[0], size[1], []QuotaAccountRow{
			{Agent: "codex", Name: "work", Observed: "observed", Windows: "primary 42%", Detail: "recorded 2026-01-01T09:00:00Z"},
		})
		seen := walkFrames(t, pane)
		label := func(s string) string { return fmt.Sprintf("%s at %dx%d", s, size[0], size[1]) }
		assertEveryLineShown(t, seen, label("mover line"), wrappedLines(pane, "mover: "+pane.quota.mover))
		assertEveryLineShown(t, seen, label("closing footnote"), wrappedLines(pane, quotaAccountsFootNote))
	}
}

// An over-tall row still opens at its top — the line naming it — and ↓ then
// reads down through it a line at a time before the cursor moves on; ↑ from the
// next row comes back to its top. The scroll is bounded by the row's own span.
func TestConfigPaneOverTallRow_OpensAtItsTopThenScrollsThenMoves(t *testing.T) {
	pane := evidencePane(t, overlayW40x10, overlayH40x10, []QuotaAccountRow{
		{Agent: "codex", Name: "work", Observed: "observed", Windows: "primary 42%, secondary 4%", Detail: "recorded 2026-01-01T09:00:00Z"},
		{Agent: "codex", Name: "personal", Observed: "no evidence", Windows: "—", Detail: "none"},
	})
	first := -1
	for i, row := range pane.rows {
		if row.quotaAccount != nil {
			first = i
			break
		}
	}
	for i := 0; pane.selectedIdx != first; i++ {
		if i > 200 {
			t.Fatalf("↓ never reached the first account-evidence row (row %d)", first)
		}
		pressKey(pane, "down")
		_ = pane.String()
	}
	if got := xansi.Strip(pane.String()); !strings.Contains(got, "codex/work") {
		t.Fatalf("an over-tall row must open at its top, the line that names it:\n%s", got)
	}

	_, selStart, selEnd := pane.renderRowLines()
	budget := max(1, pane.windowBudget(pane.renderHeader(), pane.renderStatus()+pane.renderHints()))
	if selEnd-selStart <= budget {
		t.Fatalf("fixture is not over-tall: span %d lines, window %d", selEnd-selStart, budget)
	}
	steps := selEnd - selStart - budget
	for i := 0; i < steps; i++ {
		top := pane.scrollTop
		pressKey(pane, "down")
		_ = pane.String()
		if pane.selectedIdx != first {
			t.Fatalf("↓ %d of %d left the row before its last line was shown", i+1, steps)
		}
		if pane.scrollTop != top+1 {
			t.Fatalf("↓ %d must scroll one line within the row: top %d -> %d", i+1, top, pane.scrollTop)
		}
	}
	pressKey(pane, "down")
	_ = pane.String()
	if pane.selectedIdx == first {
		t.Fatal("once the row's last line is shown, ↓ must move the cursor on")
	}
	pressKey(pane, "up")
	if got := xansi.Strip(pane.String()); pane.selectedIdx != first || !strings.Contains(got, "codex/work") {
		t.Fatalf("↑ must return to the over-tall row at its top (selected %d, want %d):\n%s", pane.selectedIdx, first, got)
	}
}

// Where every selection fits the window, ↑/↓ are exactly what they were: each
// press moves the cursor, even though the list itself overflows the pane.
func TestConfigPaneFittingSelection_EveryPressMovesTheCursor(t *testing.T) {
	pane := evidencePane(t, 100, 20, []QuotaAccountRow{
		{Agent: "codex", Name: "work", Observed: "observed", Windows: "primary 42%", Detail: "recorded 2026-01-01T09:00:00Z"},
	})
	if lines, _, _ := pane.renderRowLines(); len(lines) <= pane.height {
		t.Fatalf("fixture must overflow the pane: %d lines in a %d-line pane", len(lines), pane.height)
	}
	var selectable []int
	for i, row := range pane.rows {
		if row.isSelectable() {
			selectable = append(selectable, i)
		}
	}
	for n, want := range selectable[1:] {
		pressKey(pane, "down")
		_ = pane.String()
		if pane.selectedIdx != want {
			t.Fatalf("↓ %d must move the cursor to row %d, got %d", n+1, want, pane.selectedIdx)
		}
	}
	for n := len(selectable) - 2; n >= 0; n-- {
		pressKey(pane, "up")
		_ = pane.String()
		if pane.selectedIdx != selectable[n] {
			t.Fatalf("↑ must move the cursor to row %d, got %d", selectable[n], pane.selectedIdx)
		}
	}
}

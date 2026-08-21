package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/config"
)

// TestConfigPaneDisplayValueStaysOneLine is the Codex P2 on #3428: moving
// displayValue onto a LINE-oriented truncator has to keep the total capped.
//
// lipgloss.Width reports the WIDEST line of a multi-line string, so a value made
// of many short lines measures as narrow — fitLine would pass it through whole
// and one list row would expand into several. The rune budget it replaced capped
// the total, so a straight swap traded a width overflow for a height one. An
// unrestricted string key (on_archive_command) genuinely accepts newlines.
//
// Both halves are asserted, because either alone can pass while the pane is
// broken: the value renders on ONE line, and the pane still renders one row per
// entry.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: displayValue returns the newlines untouched,
// and the entry's value spills onto rows of its own.
func TestConfigPaneDisplayValueStaysOneLine(t *testing.T) {
	const key = "on_archive_command"
	// 12 short lines: every individual line is well inside the budget, and the
	// total is far outside it. That is exactly the shape a widest-line
	// measurement cannot see.
	value := strings.TrimSuffix(strings.Repeat("echo archived-a-session\n", 12), "\n")

	entries := config.ManifestWithValues(config.DefaultConfig())
	seeded := false
	for i := range entries {
		if entries[i].Key == key {
			entries[i].Value = value
			seeded = true
		}
	}
	if !seeded {
		t.Fatalf("%s left the manifest — this test is vacuous", key)
	}

	const w = 72
	c := NewConfigPane()
	c.SetSize(w, paneHeight)
	c.SetEntries(entries, "/tmp/config.toml")
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()

	shown := c.displayValue(config.ConfigEntry{Key: key, Value: value})
	if strings.ContainsAny(shown, "\n\r\t") {
		t.Errorf("displayValue kept control whitespace, so the row is not one line: %q", shown)
	}
	if got := lipgloss.Width(shown); got > w {
		t.Errorf("displayValue returned %d cells for a %d-cell pane: %q", got, w, shown)
	}
	if strings.Count(value, "\n") > 0 && len(shown) >= len(value) {
		t.Errorf("displayValue did not cap a multiline value at all (%d bytes shown of %d)", len(shown), len(value))
	}

	// And the rendered pane: no row may carry the value's tail on a line of its
	// own, which is what a passed-through newline produces.
	for step := 0; step < 45; step++ {
		for _, line := range strings.Split(c.String(), "\n") {
			if strings.Contains(line, "echo archived-a-session") && !strings.Contains(line, key) {
				t.Fatalf("step %d: a multiline value spilled onto a row of its own: %q", step, line)
			}
		}
		c.move(1)
	}
}

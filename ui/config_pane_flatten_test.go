package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	muesliansi "github.com/muesli/ansi"

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

// Clusters whose two measurements disagree, built from escapes so the source
// stays readable and greppable:
//
//	zwjFamily: four emoji joined by three ZWJs. Grapheme-aware measurement calls
//	it 2 cells; per-codepoint measurement calls it 8.
//	regionalFlag: a two-codepoint flag. 2 cells grouped, 4 apart.
const (
	zeroWidthJoiner = "\u200d"
	zeroWidthSpace  = "\u200b"
	zwjFamily       = "\U0001F468" + zeroWidthJoiner + "\U0001F469" +
		zeroWidthJoiner + "\U0001F467" + zeroWidthJoiner + "\U0001F466"
	regionalFlag = "\U0001F1EF\U0001F1F5"
)

// TestConfigPanePreviewIsBoundedForHostileValues covers the three remaining ways
// an untrusted config value broke the list preview (#3421 review). Each is a
// separate mechanism, so each gets its own assertion rather than one "looks fine".
func TestConfigPanePreviewIsBoundedForHostileValues(t *testing.T) {
	const budget = 44

	t.Run("escape sequences are stripped", func(t *testing.T) {
		// A cell-measuring truncator preserves sequences across its cut on purpose;
		// preserved here, ED clears the screen from inside a list row. TOML writes an
		// escape with a \u sequence, so this is a value a user can genuinely store.
		value := "echo hi\x1b[2J\x1b[H" + strings.Repeat("x", 200)
		got := truncateConfigPreview(value, budget)
		if strings.ContainsRune(got, 0x1b) {
			t.Fatalf("an escape sequence survived into the rendered row: %q", got)
		}
		if !strings.Contains(got, "echo hi") {
			t.Fatalf("stripping ate the visible text too: %q", got)
		}
	})

	t.Run("zero-width runs are volume-capped", func(t *testing.T) {
		// Zero-width under every width measure, so a width cut alone keeps all of it
		// and every repaint pays for the whole string.
		value := strings.Repeat(zeroWidthSpace, 50000) + "tail"
		got := truncateConfigPreview(value, budget)
		if n := len([]rune(got)); n > maxConfigPreviewRunes {
			t.Fatalf("preview kept %d runes, over the %d-rune cap", n, maxConfigPreviewRunes)
		}
	})

	t.Run("joined emoji fit the compositor's measure too", func(t *testing.T) {
		// ui/overlay.PlaceOverlay measures the foreground with
		// ansi.PrintableRuneWidth (per codepoint) and returns the FOREGROUND ALONE
		// when it reads wider than the background, so a modal measured at 200 columns
		// erases the frame behind it. tmux advances per codepoint as well, so the
		// pessimistic count is the one a real terminal agrees with. Bounding by it
		// must also bound the grapheme measure.
		got := truncateConfigPreview(strings.Repeat(zwjFamily, 40), budget)
		if w := muesliansi.PrintableRuneWidth(got); w > budget {
			t.Errorf("preview is %d cells under ui/overlay.PlaceOverlay's own measure, over the %d-cell budget — the compositor would drop the frame behind the modal", w, budget)
		}
		if w := lipgloss.Width(got); w > budget {
			t.Errorf("preview is %d cells under the grapheme measure, over the %d-cell budget", w, budget)
		}
	})

	t.Run("an ordinary value is untouched", func(t *testing.T) {
		// The hardening must not truncate what already fits.
		value := "/usr/local/bin/code-server"
		if got := truncateConfigPreview(value, budget); got != value {
			t.Fatalf("a value that fits was altered: %q -> %q", value, got)
		}
	})
}

// TestConfigPaneRowsFitBothWidthMeasures pins the compositor half at the pane
// level: every rendered line must fit the pane under ansi.PrintableRuneWidth as
// well as lipgloss.Width, because PlaceOverlay uses the former to decide whether
// the modal still fits over the frame.
func TestConfigPaneRowsFitBothWidthMeasures(t *testing.T) {
	const w = 72
	entries := config.ManifestWithValues(config.DefaultConfig())
	seeded := 0
	for i := range entries {
		switch entries[i].Key {
		case "on_archive_command":
			entries[i].Value = strings.Repeat(zwjFamily, 40)
			seeded++
		case "vscode_server_binary":
			entries[i].Value = "/opt/" + strings.Repeat(regionalFlag, 40) + "/bin"
			seeded++
		}
	}
	if seeded != 2 {
		t.Fatalf("seeded %d of 2 values - a manifest rename made this test vacuous", seeded)
	}

	c := NewConfigPane()
	c.SetSize(w, paneHeight)
	c.SetEntries(entries, "/tmp/config.toml")
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()

	for step := 0; step < 45; step++ {
		for _, line := range strings.Split(c.String(), "\n") {
			if got := muesliansi.PrintableRuneWidth(line); got > w {
				t.Fatalf("step %d: a line is %d cells under ui/overlay.PlaceOverlay's own measure, in a %d-cell pane - the compositor would drop the frame behind the modal.\n  line: %q",
					step, got, w, line)
			}
		}
		c.move(1)
	}
}

package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
)

// The 'e' explain view (#4803): the TUI half of the same ResolvedValue the CLI
// prints and the daemon serves. These pin the pane's plumbing — the seam is
// injected exactly like `save`, so the test drives the real key routing and
// rendering against a canned trace rather than re-resolving config (the
// resolver has its own coverage in config/).

func explainFixture() *config.ResolvedValue {
	return &config.ResolvedValue{
		Key:        "default_program",
		Value:      "codex",
		Default:    "claude",
		Merge:      "replace",
		Precedence: []string{"global"},
		Candidates: []config.CandidateTrace{
			{Layer: "built-in", Present: true, Allowed: true, Value: "claude", Result: "shadowed", Reason: "overridden by higher-precedence global"},
			{Layer: "global", Path: "/home/u/.agent-factory/config.toml", KeyPath: "default_program", Present: true, Allowed: true, Value: "codex", Result: "winner", Reason: "highest-precedence present allowed source"},
		},
	}
}

func pressKey(c *ConfigPane, s string) {
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func TestConfigPaneExplainRendersTheCandidateTrace(t *testing.T) {
	c := newTestConfigPane(t)
	c.explain = func(key string) (*config.ResolvedValue, error) {
		if key != "default_program" {
			t.Fatalf("explained %q, the selected row's key", key)
		}
		return explainFixture(), nil
	}
	selectKey(t, c, "default_program")
	pressKey(c, "e")

	out := c.String()
	for _, want := range []string{
		"default_program = codex",
		"policy: replace",
		"built-in: shadowed",
		"global: winner",
		"highest-precedence present allowed source",
		"/home/u/.agent-factory/config.toml:default_program",
		"running daemon's value was not checked",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("explain view missing %q:\n%s", want, out)
		}
	}
}

func TestConfigPaneExplainSurfacesTheRefusalRatherThanAFabricatedTrace(t *testing.T) {
	c := newTestConfigPane(t)
	c.explain = func(key string) (*config.ResolvedValue, error) {
		return nil, errors.New("the daemon at http://box:8443 does not serve the ExplainConfig route")
	}
	selectKey(t, c, "default_program")
	pressKey(c, "e")

	out := c.String()
	if c.explaining {
		t.Fatal("a failed explain must stay in the list view — never render a blank trace")
	}
	if !strings.Contains(out, "does not serve the ExplainConfig route") {
		t.Fatalf("the refusal must be visible verbatim:\n%s", out)
	}
}

func TestConfigPaneExplainEscReturnsToTheRowItExplained(t *testing.T) {
	c := newTestConfigPane(t)
	c.explain = func(key string) (*config.ResolvedValue, error) { return explainFixture(), nil }
	selectKey(t, c, "default_program")
	pressKey(c, "e")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})

	if c.explaining {
		t.Fatal("esc must leave the explain view")
	}
	if entry := c.selectedEntry(); entry == nil || entry.Key != "default_program" {
		t.Fatalf("esc must return to the explained row, got %+v", entry)
	}
	// And the list is back: the row's own hint row, not the explain footer.
	if out := c.String(); !strings.Contains(out, "e explain") {
		t.Fatalf("the list view must be restored:\n%s", out)
	}
}

func TestConfigPaneExplainToggleKeyAndEnterAlsoExit(t *testing.T) {
	for _, key := range []string{"e", "enter"} {
		c := newTestConfigPane(t)
		c.explain = func(string) (*config.ResolvedValue, error) { return explainFixture(), nil }
		selectKey(t, c, "default_program")
		pressKey(c, "e")
		if !c.explaining {
			t.Fatalf("'e' did not open the explain view")
		}
		if key == "enter" {
			c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
		} else {
			pressKey(c, key)
		}
		if c.explaining {
			t.Fatalf("%s must exit the explain view", key)
		}
	}
}

func TestConfigPaneExplainIsANoOpOnARowWithNoKey(t *testing.T) {
	c := newTestConfigPane(t)
	c.explain = func(string) (*config.ResolvedValue, error) {
		t.Fatal("explain must not be called for a row with no config key")
		return nil, nil
	}
	// A tier heading has no config key: nothing exists to resolve. (The cursor
	// cannot normally rest on one — clampSelection skips it — but the guard is
	// load-bearing for rows that share the shape, like account rows.)
	for i, row := range c.rows {
		if row.heading != "" {
			c.selectedIdx = i
			break
		}
	}
	pressKey(c, "e")
	if c.explaining {
		t.Fatal("a heading row has no config key — nothing to explain")
	}
}

func TestConfigPaneExplainStripsControlBytesFromRenderedValues(t *testing.T) {
	// A free-form value (on_archive_command) holds whatever TOML expressed —
	// including \u001B escapes that, rendered verbatim, would emit screen clears
	// or OSC writes into the TUI. The trace must render what the value IS, not
	// what it does (the list's truncateConfigPreview strip+flatten, minus the
	// truncation).
	c := newTestConfigPane(t)
	c.explain = func(string) (*config.ResolvedValue, error) {
		v := explainFixture()
		v.Value = "echo \x1b[2Jdone\n\twith: control"
		v.Candidates[1].Value = v.Value
		v.Candidates[1].Path = "/tmp/evil \x1b]8;;http://x\x07path"
		return v, nil
	}
	selectKey(t, c, "default_program")
	pressKey(c, "e")

	out := c.String()
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x1b]8") {
		t.Fatalf("the explain view must not emit the value's escape bytes:\n%q", out)
	}
	// The printable text survives — one line of it, whitespace flattened.
	if !strings.Contains(out, "echo done  with: control") {
		t.Fatalf("stripping must keep the value's printable text:\n%s", out)
	}
}

func TestConfigPaneClosingDropsAnOpenExplanation(t *testing.T) {
	c := newTestConfigPane(t)
	c.explain = func(string) (*config.ResolvedValue, error) { return explainFixture(), nil }
	selectKey(t, c, "default_program")
	pressKey(c, "e")
	c.SetFocus(false)

	if c.explaining || len(c.explainLines) != 0 {
		t.Fatal("closing the overlay must drop the trace — reopening onto a stale one would explain a file that may have changed")
	}
}

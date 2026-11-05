package ui

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
)

// TestConfigPaneEditRestoreIsNoOp pins the raw-value no-op arm in commitEdit.
//
// For a sanitizer-stable single-line value (no newlines, no control runes), the
// field seed equals the raw entry.Value. If the user edits the field and then
// restores it back to that original text, dirty stays true from the edit even
// though the field once again holds the stored value. Before the raw-value arm
// was added, commitEdit's guard was `!c.dirty && value == editSeed` — dirty was
// true, so the guard failed and Enter ran the config writer / daemon apply path
// (and risked an admission refusal) for no semantic change. The raw-value arm
// treats value == entry.Value as a no-op regardless of dirty, so restoring an
// ordinary value is silent.
//
// This does not weaken the multi-line replace-with-flattened-text case
// (TestConfigPaneMultiLineValueEditStillWrites): a multi-line entry.Value can
// never equal the single-line field, so that edit never hits this arm.
func TestConfigPaneEditRestoreIsNoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	path := filepath.Join(home, "config.toml")
	body := "default_program = 'claude'\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	orig := []byte(body)

	c := openPaneOn(t, path, "default_program")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if !c.IsEditing() {
		t.Fatal("enter must open the value field on default_program")
	}
	seed := c.input.Value()
	if seed != "claude" {
		t.Fatalf("seed = %q, want %q (the raw single-line value)", seed, "claude")
	}

	// Edit the field to something else, then restore it back to the original
	// text. dirty is now true, but the field holds the stored value again.
	c.input.SetValue("")
	typeInto(c, "codex")
	if c.input.Value() != "codex" {
		t.Fatalf("edit did not take: got %q", c.input.Value())
	}
	c.input.SetValue("")
	typeInto(c, "claude")
	if c.input.Value() != "claude" {
		t.Fatalf("restore did not take: got %q", c.input.Value())
	}

	var writes int
	c.save = func(k, v string) (*config.SetResult, string, error) {
		writes++
		return &config.SetResult{Key: k, Value: v, Path: path, RequiresRestart: true},
			config.EffectNotice(k, config.ApplyOutcome{DaemonApply: config.DaemonApplyApplied}), nil
	}
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	if writes != 0 {
		t.Errorf("restoring the original single-line value issued %d write(s); it must issue none — dirty stayed true but the field held the raw value, so this is a no-op", writes)
	}
	if c.restartNotice != "" {
		t.Error("a restore told the user to restart for a value they put back to what it already was")
	}
	if c.status != "" {
		t.Errorf("a restore echoed a status for no semantic change: %q", c.status)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(orig) {
		t.Errorf("restoring the original value rewrote the file.\n--- before ---\n%s\n--- after ---\n%s", orig, after)
	}
}

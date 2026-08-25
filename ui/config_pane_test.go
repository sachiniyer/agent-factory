package ui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/config"
)

// newTestConfigPane builds a pane over the real manifest and the default config,
// with the advanced tier expanded so every key is reachable.
func newTestConfigPane(t *testing.T) *ConfigPane {
	t.Helper()
	c := NewConfigPane()
	// Deliberately taller than the list. The window is real and tested
	// separately (TestConfigPaneKeepsTheSelectionVisible); here it must not be
	// the thing under test — at a realistic height the anti-drift test would
	// start failing because a key scrolled off, which reads as "the manifest
	// drifted" when nothing drifted at all.
	c.SetSize(100, 200)
	c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml")
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()
	return c
}

// typeInto sends each rune of s to the pane as a key press.
func typeInto(c *ConfigPane, s string) {
	for _, r := range s {
		c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// selectKey moves the cursor onto the named key, failing if it is not reachable.
func selectKey(t *testing.T, c *ConfigPane, key string) {
	t.Helper()
	for i, row := range c.rows {
		if row.entry != nil && row.entry.Key == key {
			c.selectedIdx = i
			return
		}
	}
	t.Fatalf("key %q is not a row in the config editor", key)
}

// TestConfigPaneRendersEveryManifestKey is the TUI half of the anti-drift
// guarantee, and it is the reason this pane renders from the manifest instead of
// a hand-written form.
//
// A hand-maintained form drifts the moment someone adds a config key — the key
// exists, `af config set` takes it, and the editor silently does not show it.
// This test makes that a build failure: it iterates config.Manifest() (which is
// itself pinned to config_types.go by TestManifestCoversEveryConfigKey) and
// demands every key appear on screen. Adding a key to config_types.go therefore
// reaches this surface with no edit to config_pane.go, or CI goes red.
func TestConfigPaneRendersEveryManifestKey(t *testing.T) {
	c := newTestConfigPane(t)
	view := c.String()

	for _, e := range config.Manifest() {
		if !strings.Contains(view, e.Key) {
			t.Errorf("config key %q is in the manifest but the TUI editor does not render it — "+
				"a user cannot see or set a key that exists", e.Key)
		}
	}
}

// TestConfigPaneRendersEveryTierAndFoldsAdvanced pins the tier presentation: the
// core is what a user came for, so it leads; the advanced tier is folded until
// asked for rather than burying the handful of keys that matter.
func TestConfigPaneRendersEveryTierAndFoldsAdvanced(t *testing.T) {
	c := NewConfigPane()
	c.SetSize(100, 40)
	c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml")
	c.SetFocus(true)

	folded := c.String()
	if !strings.Contains(folded, "default_program") {
		t.Error("the core tier must be visible without expanding anything")
	}
	if strings.Contains(folded, "daemon_poll_interval") {
		t.Error("an advanced key must stay folded until asked for")
	}
	if !strings.Contains(folded, "show advanced") {
		t.Error("the fold must be discoverable — the hint names the key that opens it")
	}

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	expanded := c.String()
	if !strings.Contains(expanded, "daemon_poll_interval") {
		t.Error("'a' must reveal the advanced tier")
	}
}

// TestConfigPaneEditWritesThroughTheRealPathAndEchoes is the end-to-end contract
// for the TUI surface: a committed edit goes through the REAL
// config.SetGlobalConfigValue (validated, file-locked, atomic), the file it
// writes still loads, and the pane echoes `key = value` from the write path's own
// result rather than from what it believes it sent.
//
// It writes to a throwaway AGENT_FACTORY_HOME — never the user's real config.
func TestConfigPaneEditWritesThroughTheRealPathAndEchoes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(tomlPath, []byte("# hand-written\ndefault_program = 'claude'\n"), 0644); err != nil {
		t.Fatal(err)
	}

	c := newTestConfigPane(t)
	selectKey(t, c, "default_program")

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if !c.IsEditing() {
		t.Fatal("enter must open the value field on a settable key")
	}
	// Clear the pre-filled value, then type the new one.
	c.input.SetValue("")
	typeInto(c, "codex")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	if c.IsEditing() {
		t.Fatal("a successful save must close the value field")
	}
	if c.statusIsError {
		t.Fatalf("unexpected error: %s", c.status)
	}
	// The echo contract: `key = value`, same as `af config set` and the config agent.
	if want := "set default_program = codex"; !strings.Contains(c.status, want) {
		t.Errorf("the pane must echo what was written.\n got: %q\nwant substring: %q", c.status, want)
	}

	// It reached the real file, through the real writer.
	written, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "default_program = 'codex'") {
		t.Errorf("the edit did not reach config.toml:\n%s", written)
	}
	if !strings.Contains(string(written), "# hand-written") {
		t.Errorf("the edit destroyed a hand-written comment — config.toml must stay hand-editable:\n%s", written)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("config.toml does not load after a TUI edit: %v", err)
	}
	if cfg.DefaultProgram != "codex" {
		t.Errorf("loaded default_program = %q, want codex", cfg.DefaultProgram)
	}
}

// TestConfigPaneFormerlyImmutableKeysRoundTrip proves the TUI half of #3345
// through the real config-set path. Each row that used to be read-only must
// open a field, save, survive a real load, and refresh to the saved value.
func TestConfigPaneFormerlyImmutableKeysRoundTrip(t *testing.T) {
	want := config.DefaultConfig()
	want.Theme.Accent = "#112233"
	if want.ProgramOverrides == nil {
		want.ProgramOverrides = map[string]string{}
	}
	want.ProgramOverrides["codex"] = "codex --model gpt-5"
	want.SessionEnvPassthrough = []string{"AF_TEST_ONE", "AF_TEST_TWO"}
	want.LimitPatterns = map[string]string{"claude": "usage limit reached"}
	want.RootAgents = map[string]config.RootAgentConfig{"/tmp/repo": {Program: "codex"}}
	want.RootAgent = config.RootAgent{Enabled: true, Program: "codex"}
	want.Keys = map[string]any{"quit": "Q"}

	for _, key := range []string{
		"theme",
		"program_overrides",
		"session_env_passthrough",
		"limit_patterns",
		"root_agents",
		"root_agent",
		"keys",
	} {
		t.Run(key, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("# hand-written\n"), 0644); err != nil {
				t.Fatal(err)
			}
			value, ok := config.CurrentValue(want, key)
			if !ok {
				t.Fatalf("CurrentValue cannot render %s", key)
			}

			c := newTestConfigPane(t)
			selectKey(t, c, key)
			c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
			if !c.IsEditing() {
				t.Fatalf("enter did not open %s for editing", key)
			}
			c.input.SetValue(value)
			c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
			if c.IsEditing() || c.statusIsError {
				t.Fatalf("save %s failed: %s", key, c.status)
			}

			got, err := config.LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, ok := config.CurrentValue(got, key)
			if !ok || roundTrip != value {
				t.Fatalf("%s round-tripped as %q (ok=%v), want %q", key, roundTrip, ok, value)
			}
		})
	}
}

// TestConfigPaneSurfacesRestartNoticeAtTheMomentOfTheEdit is requirement 3 for
// the TUI.
//
// Since #2480 a TUI edit is applied to the running daemon in place, so the pane
// must CONFIRM the save at the moment of the edit and state what is deferred —
// and it must NOT drop the user to a shell to run a command (#2479). Saying
// nothing, or telling the user to run `af daemon restart`, is the failure this
// requirement guards against.
func TestConfigPaneSurfacesRestartNoticeAtTheMomentOfTheEdit(t *testing.T) {
	c := newTestConfigPane(t)
	selectKey(t, c, "default_program")

	// Stub the writer: this test is about what the pane SAYS, not about the file.
	// It returns the per-key effect notice the real write path (applyingConfigSet)
	// hands back, so this pins that the pane renders THAT, not a hardcoded string.
	c.save = func(key, value string) (*config.SetResult, string, error) {
		return &config.SetResult{Key: key, Value: value, Path: "/tmp/config.toml", RequiresRestart: true},
			config.EffectNotice(key, true), nil
	}

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	c.input.SetValue("codex")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	view := c.String()
	if strings.Contains(view, "daemon restart") {
		t.Errorf("the notice must NOT tell the user to run a command — since #2480 the daemon applies the write in place (#2479).\n--- view ---\n%s", view)
	}
	// default_program is applied live, so the notice confirms it is live NOW rather
	// than showing one canned "restart to apply" sentence.
	if !strings.Contains(view, "using the new value now") {
		t.Errorf("the pane must surface the per-key effect notice it was handed.\n--- view ---\n%s", view)
	}
}

// TestConfigPaneRejectsInvalidValueWithTheValidatorsOwnError is requirement 2 for
// the TUI: a bad value is refused by the same validation the CLI uses, the
// message is the validator's own, and the field stays open with the bad value in
// it so the user can fix it.
func TestConfigPaneRejectsInvalidValueWithTheValidatorsOwnError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, "config.toml")
	orig := "default_program = 'claude'\n"
	if err := os.WriteFile(tomlPath, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}

	c := newTestConfigPane(t)
	selectKey(t, c, "update_channel")

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	c.input.SetValue("nightly")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	if !c.statusIsError {
		t.Fatal("an invalid value must be rejected in the UI, not written and discovered at startup")
	}
	if !strings.Contains(c.status, "update_channel must be one of") {
		t.Errorf("the pane must show the validator's own message, not one of its own.\n got: %q", c.status)
	}
	if !c.IsEditing() {
		t.Error("a rejected value must leave the field open so the user can correct it")
	}

	after, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != orig {
		t.Errorf("a REJECTED edit still touched config.toml.\n got: %q\nwant: %q", after, orig)
	}
}

// TestConfigPaneOffersEveryManifestRowForEditing pins the TUI half of the
// removed read-only class: every entry row accepts the cursor and Enter opens
// its complete CurrentValue, including structured JSON values.
func TestConfigPaneOffersEveryManifestRowForEditing(t *testing.T) {
	c := newTestConfigPane(t)
	for i, row := range c.rows {
		if row.entry == nil {
			continue
		}
		c.selectedIdx = i
		c.clampSelection()
		if landed := c.selectedEntry(); landed == nil || landed.Key != row.entry.Key {
			t.Errorf("the cursor skipped editable row %q", row.entry.Key)
		}
		c.beginEdit()
		if !c.IsEditing() {
			t.Errorf("enter did not open editable row %q", row.entry.Key)
		}
		c.cancelEdit()
	}
}

// TestConfigPaneEditFieldTakesArbitraryValueText is the reason this pane owns the
// keyboard while open: a config value is arbitrary text, and the global key map
// must not eat it.
//
// "127.0.0.1:8080" is the real case (network.listen_addr) — it contains ".", ":" and
// digits, and the digits 1-9 are the TUI's tab-jump keys. If the pane did not
// consume them, typing an address would jump tabs instead.
func TestConfigPaneEditFieldTakesArbitraryValueText(t *testing.T) {
	c := newTestConfigPane(t)
	selectKey(t, c, "network.listen_addr")

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	c.input.SetValue("")
	typeInto(c, "127.0.0.1:8080")

	if got := c.input.Value(); got != "127.0.0.1:8080" {
		t.Errorf("the value field must take arbitrary text verbatim.\n got: %q\nwant: %q", got, "127.0.0.1:8080")
	}
}

// TestConfigPaneQuitKeyIsTypeableWhileEditing pins the divergence from the hooks
// and tasks overlays, which root-route the configured quit key even mid-edit.
//
// The pane must CONSUME "q" while a value is being typed. Config values are
// arbitrary user strings — a vscode binary at /home/quentin/bin/code, a branch
// prefix — and a user typing one must get their "q", not an exit. The app relies
// on IsEditing() to know this; see handleStateConfigEditor.
func TestConfigPaneQuitKeyIsTypeableWhileEditing(t *testing.T) {
	c := newTestConfigPane(t)
	selectKey(t, c, "vscode_server_binary")

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	c.input.SetValue("")
	typeInto(c, "/home/quentin/bin/code")

	if got := c.input.Value(); got != "/home/quentin/bin/code" {
		t.Errorf("a path containing the quit key must be typeable.\n got: %q", got)
	}
	if !c.IsEditing() {
		t.Error("typing 'q' into a value must not leave edit mode")
	}
}

// TestConfigPaneEscClosesByDroppingFocus pins the close idiom the app depends on:
// the pane drops its own focus and the app reads that as "close the overlay".
func TestConfigPaneEscClosesByDroppingFocus(t *testing.T) {
	c := newTestConfigPane(t)
	if !c.HasFocus() {
		t.Fatal("precondition: pane starts focused")
	}
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	if c.HasFocus() {
		t.Error("esc must drop focus so the app closes the overlay")
	}
}

// TestConfigPaneEscDuringEditCancelsWithoutClosing pins the two-level escape: the
// first esc abandons the edit, not the overlay. Closing the whole editor on a
// mistyped character would be a hostile way to lose someone's place.
func TestConfigPaneEscDuringEditCancelsWithoutClosing(t *testing.T) {
	c := newTestConfigPane(t)
	selectKey(t, c, "default_program")

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	typeInto(c, "xyz")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})

	if c.IsEditing() {
		t.Error("esc must abandon the edit")
	}
	if !c.HasFocus() {
		t.Error("esc during an edit must NOT close the whole editor")
	}
}

// errStubRejected stands in for the validator refusing a value.
var errStubRejected = errors.New(`update_channel must be one of [stable, preview], got "nightly"`)

// paneHeight is a realistic overlay height: the config list is taller than this
// once the advanced tier is open, which is the whole point.
const paneHeight = 20

// TestConfigPaneKeepsTheSelectionVisible is the guard for a selection you cannot
// see — which is a selection you will change by accident.
//
// The list runs to ~31 lines with the advanced tier open. In a 20-line pane an
// unwindowed render walks the cursor off the bottom: the user presses ↓ until the
// marker is gone, then presses enter and edits a row they cannot see. This walks
// the whole list, as a user holding ↓ would, and demands the cursor be on screen
// at every step.
func TestConfigPaneKeepsTheSelectionVisible(t *testing.T) {
	c := NewConfigPane()
	c.SetSize(64, paneHeight)
	c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml")
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()

	seen := map[string]bool{}
	for step := 0; step < 40; step++ {
		sel := c.selectedEntry()
		if sel == nil {
			t.Fatalf("step %d: no selectable row", step)
		}
		seen[sel.Key] = true

		view := c.String()
		lines := strings.Split(view, "\n")
		if len(lines) > paneHeight+1 {
			t.Fatalf("step %d (%s): the pane rendered %d lines into a %d-line box — it must window, not overflow",
				step, sel.Key, len(lines), paneHeight)
		}
		if !strings.Contains(view, "›") {
			t.Fatalf("step %d: the cursor is off screen while %q is selected — a selection you cannot see is one you will change by accident.\n--- view ---\n%s",
				step, sel.Key, view)
		}
		// The selected key itself must be readable, not merely the marker.
		if !strings.Contains(view, sel.Key) {
			t.Fatalf("step %d: %q is selected but not rendered.\n--- view ---\n%s", step, sel.Key, view)
		}
		c.move(1)
	}

	if len(seen) < 5 {
		t.Fatalf("the walk only reached %d keys — it is not exercising the scroll", len(seen))
	}

	// And back up again: the window must follow the cursor in both directions.
	for step := 0; step < 40; step++ {
		c.move(-1)
		sel := c.selectedEntry()
		view := c.String()
		if !strings.Contains(view, "›") || !strings.Contains(view, sel.Key) {
			t.Fatalf("walking back up, %q went off screen.\n--- view ---\n%s", sel.Key, view)
		}
	}
}

// TestConfigPaneWindowSaysWhatIsHidden pins the cue. A list that silently shows
// two thirds of itself reads as the whole thing — a user who cannot see
// worktree_root concludes af has no such setting.
func TestConfigPaneWindowSaysWhatIsHidden(t *testing.T) {
	c := NewConfigPane()
	c.SetSize(64, paneHeight)
	c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml")
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()

	if !strings.Contains(c.String(), "more") {
		t.Errorf("with the list scrolled, the pane must say content is hidden.\n--- view ---\n%s", c.String())
	}
}

// TestConfigPaneClosingClearsTheLastWritesStatus is the guard for a stale echo
// bleeding into the next open.
//
// The bug: esc closed the overlay by assigning hasFocus directly, skipping
// SetFocus's reset. Reopening the editor then showed "set default_program =
// codex" and a restart notice for an edit made minutes earlier — telling the user
// something had just happened when nothing had, and pointing at a restart they
// may already have done.
//
// It drives the REAL close path (the esc key), not SetFocus, because assigning
// the field directly was the bug: a test that called SetFocus would have passed
// against it.
func TestConfigPaneClosingClearsTheLastWritesStatus(t *testing.T) {
	c := newTestConfigPane(t)
	selectKey(t, c, "default_program")
	c.save = func(k, v string) (*config.SetResult, string, error) {
		return &config.SetResult{Key: k, Value: v, Path: "/tmp/config.toml", RequiresRestart: true},
			config.EffectNotice(k, true), nil
	}

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	c.input.SetValue("codex")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if c.status == "" {
		t.Fatal("precondition: a write must leave an echo")
	}

	// Close the way the app closes it.
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	if c.HasFocus() {
		t.Fatal("precondition: esc drops focus")
	}

	// Reopen the way showConfigEditor reopens it.
	c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml")
	c.SetFocus(true)

	view := c.String()
	if strings.Contains(view, "set default_program = codex") {
		t.Errorf("a reopened editor showed the PREVIOUS session's echo.\n--- view ---\n%s", view)
	}
	if strings.Contains(view, "using the new value now") {
		t.Errorf("a reopened editor showed a stale apply notice for an edit the user cannot see.\n--- view ---\n%s", view)
	}
}

// TestConfigPaneClosingClearsAStaleError is the same guard for the error line: a
// rejected value's message must not greet the user on their next open.
func TestConfigPaneClosingClearsAStaleError(t *testing.T) {
	c := newTestConfigPane(t)
	selectKey(t, c, "update_channel")
	c.save = func(k, v string) (*config.SetResult, string, error) {
		return nil, "", errStubRejected
	}

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	c.input.SetValue("nightly")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if !c.statusIsError {
		t.Fatal("precondition: a rejected value must leave an error")
	}

	// esc abandons the edit; a second esc closes the editor.
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc})
	c.SetFocus(true)

	if c.statusIsError || c.status != "" {
		t.Errorf("a stale error survived the close: %q", c.status)
	}
}

// TestConfigPaneNeverRendersALineWiderThanThePane is the width half of "fits in
// its box", and it is what makes the height window's arithmetic true.
//
// The window budgets by counting the lines renderRowLines produces. If a line is
// wider than the pane, the overlay frame wraps it into several physical rows —
// so the count is a lie, the pane overflows anyway, and the selection scrolls off
// exactly as it did before the window existed. This is not hypothetical:
// worktree_root's purpose is 147 characters, over 2x a 72-column pane, and the
// theme value serialized to ~700.
//
// It walks the list with a status and a restart notice up, because those are the
// longest strings on screen.
func TestConfigPaneNeverRendersALineWiderThanThePane(t *testing.T) {
	const w = 72
	c := NewConfigPane()
	c.SetSize(w, paneHeight)
	c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "~/.agent-factory/config.toml")
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()
	c.status = `update_channel must be one of [stable, preview], got "nightly". To preserve a custom path or flags, set it to the agent name and move the command into program_overrides.`
	c.statusIsError = true

	for step := 0; step < 40; step++ {
		for _, line := range strings.Split(c.String(), "\n") {
			if got := lipgloss.Width(line); got > w {
				t.Fatalf("step %d (%s): rendered a %d-cell line into a %d-cell pane — the frame will wrap it and break the height window.\n  line: %s",
					step, c.selectedEntry().Key, got, w, line)
			}
		}
		c.move(1)
	}
}

// longConfigValue is longer than any display width and longer than the 512-char
// CharLimit that used to silently truncate it.
func longConfigValue() string { return strings.Repeat("/very/long/path/segment", 40) } // ~920 chars

// seedConfigWithLongValue writes a throwaway config.toml holding a >512-char
// value and returns its path. Never the real AF home.
func seedConfigWithLongValue(t *testing.T) (path, long string) {
	t.Helper()
	long = longConfigValue()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	path = filepath.Join(home, "config.toml")
	body := "# hand-written\ndefault_program = 'claude'\nvscode_server_binary = '" + long + "'\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path, long
}

// openPaneOn opens the editor over the config on disk, cursor on `key`.
func openPaneOn(t *testing.T, path, key string) *ConfigPane {
	t.Helper()
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	c := NewConfigPane()
	c.SetSize(72, paneHeight)
	c.SetEntries(config.ManifestWithValues(cfg), path)
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()
	selectKey(t, c, key)
	return c
}

// TestEditingOneKeyLeavesALongValueByteIdentical is the user story: you open the
// editor to change ONE thing, and everything you did not touch is exactly as you
// left it.
//
// It passes because the blast radius of a save is one key's bytes —
// SetGlobalConfigValue edits the value in place rather than re-marshaling the
// struct, so a key the user never selected is never rewritten and cannot be
// mangled by anything this pane does. That is the property worth pinning: it is
// what makes the editor safe to open at all.
func TestEditingOneKeyLeavesALongValueByteIdentical(t *testing.T) {
	path, long := seedConfigWithLongValue(t)

	c := openPaneOn(t, path, "default_program")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	c.input.SetValue("codex")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if c.statusIsError {
		t.Fatalf("save failed: %s", c.status)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), long) {
		t.Fatalf("editing default_program mangled vscode_server_binary — a key the user never touched.\n--- file ---\n%s", raw)
	}
	if !strings.Contains(string(raw), "# hand-written") {
		t.Error("the edit destroyed a hand-written comment")
	}
	if !strings.Contains(string(raw), "default_program = 'codex'") {
		t.Error("the edit itself did not land")
	}
}

// TestOpeningAndSavingALongValueDoesNotTruncateIt is the loss that was REAL, and
// the reason the CharLimit is gone.
//
// The field carried CharLimit = 512, and textinput.SetValue silently drops
// everything past it — so pre-filling a 920-character path and pressing enter,
// WITHOUT typing a character, wrote back 512 and destroyed the rest. The user
// asked for nothing and lost their value.
//
// This drives the real key path (enter to open, enter to commit) rather than
// calling save directly: the truncation happened in SetValue, so a test that
// passed the value straight to the writer would have sailed past it.
func TestOpeningAndSavingALongValueDoesNotTruncateIt(t *testing.T) {
	path, long := seedConfigWithLongValue(t)

	c := openPaneOn(t, path, "vscode_server_binary")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	if got := c.input.Value(); got != long {
		t.Fatalf("the value field truncated on OPEN: %d chars of %d. Saving now writes the short version.", len(got), len(long))
	}

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), long) {
		t.Fatalf("opening a long value and saving it unchanged DESTROYED it.\n--- file ---\n%s", raw)
	}
}

// TestSavingAnUntouchedFieldWritesNothing is the belt-and-braces half: a key
// nobody edited is never rewritten at all, so no future mangling bug in this pane
// can reach a value the user only looked at.
//
// It asserts on the WRITER, not the file: "the bytes are unchanged" would also
// hold if we wrote the identical value back, and the point is that we do not
// write.
func TestSavingAnUntouchedFieldWritesNothing(t *testing.T) {
	path, _ := seedConfigWithLongValue(t)
	c := openPaneOn(t, path, "vscode_server_binary")

	var writes int
	c.save = func(k, v string) (*config.SetResult, string, error) {
		writes++
		return &config.SetResult{Key: k, Value: v, Path: path, RequiresRestart: true},
			config.EffectNotice(k, true), nil
	}

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // commit without typing

	if writes != 0 {
		t.Errorf("saving an untouched field issued %d write(s); it must issue none", writes)
	}
	if c.restartNotice != "" {
		t.Error("a no-op told the user to restart for a change they did not make")
	}
}

// TestConfigPaneWindowsWithAZeroBudget pins the degenerate size. A pane sized
// before SetSize has run, or squeezed to nothing by a tiny terminal, must not
// panic or slice out of range — and must still keep the selection on screen when
// there is any room at all.
func TestConfigPaneWindowsWithAZeroBudget(t *testing.T) {
	// Heights that give a REAL but tiny box. h=0 is a different case — SetSize
	// has not meaningfully run, there is no box to fit, and that is covered by
	// TestConfigPaneUnsizedRendersEverything below.
	for _, h := range []int{1, 2, 5, 8} {
		c := NewConfigPane()
		c.SetSize(72, h)
		c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml")
		c.SetFocus(true)
		c.showAdvanced = true
		c.rebuildRows()

		for step := 0; step < 25; step++ {
			view := c.String() // must not panic
			// A real box, however small, must BOUND THE LIST. A non-positive
			// budget used to mean "render everything", so a 5-line pane emitted
			// all 38 lines — the window silently switching itself off at exactly
			// the size where it matters most.
			//
			// The bound is on the list, not the render: the chrome (a header, a
			// footer, and the two cue rows) cannot shrink below itself, so a
			// 1-line pane still renders ~7 lines. What must not happen is the
			// twenty-odd ROWS landing in it.
			const chrome = 4 + cueRows // header(2) + footer(2) + cues
			budget := h - chrome
			if budget < 1 {
				budget = 1
			}
			if n := len(strings.Split(view, "\n")); n > chrome+budget+1 {
				t.Fatalf("height %d: rendered %d lines (chrome %d + budget %d) — a tiny box must still window, not dump the list",
					h, n, chrome, budget)
			}
			if h >= 8 && !strings.Contains(view, "›") {
				t.Errorf("height %d: the cursor went off screen", h)
				break
			}
			c.move(1)
		}
	}
}

// TestConfigPaneUnsizedRendersEverything pins the OTHER degenerate case, and why
// it differs from a tiny box: with no size at all there is nothing to fit the
// list into, so the pane renders it rather than inventing a window from a height
// of zero. It must not panic, and it must not hide anything.
func TestConfigPaneUnsizedRendersEverything(t *testing.T) {
	c := NewConfigPane()
	c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml")
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()

	view := c.String() // must not panic
	for _, e := range config.Manifest() {
		if !strings.Contains(view, e.Key) {
			t.Errorf("unsized, the pane must render everything; %q is missing", e.Key)
		}
	}
}

// configPaneSweepWidths are the widths #3430's measurement table names. A single
// width is not enough: the pane composes several rows whose arithmetic differs,
// so a fix can hold at 72 and break at 36 (which is exactly what #3430 was).
var configPaneSweepWidths = []int{30, 36, 44, 50, 56, 64, 72}

// TestConfigPaneFitsItsBoxAtEveryWidth is #3430: the hard invariant is that the
// pane NEVER renders a line wider than itself, at any width, in any state.
//
// The hint row is what broke it. renderHints composed five hints, shed the one
// hintDropOrder could shed (#1936), and the remainder was still 43 cells — so
// below ~44 the hint row was the widest line the pane drew with nothing left to
// drop. Reachable on a ~70-column terminal, not a pathological one: the config
// overlay takes the #1821 full-screen fallback there, which hands the pane the
// terminal width minus the frame.
//
// Once the frame wraps that row the height window's line count is a lie — the
// window budgets by counting the lines renderRowLines produces — so the pane
// overflows its box and the selection scrolls off, the same mechanism
// TestConfigPaneNeverRendersALineWiderThanThePane documents at 72.
//
// The walk covers the editing hint row too, because it is the same function's
// other return path and was unconditional.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: a 43-cell hint row at widths 30 and 36.
func TestConfigPaneFitsItsBoxAtEveryWidth(t *testing.T) {
	for _, w := range configPaneSweepWidths {
		t.Run(fmt.Sprintf("w=%d", w), func(t *testing.T) {
			c := NewConfigPane()
			c.SetSize(w, paneHeight)
			c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "~/.agent-factory/config.toml")
			c.SetFocus(true)
			c.showAdvanced = true
			c.rebuildRows()
			c.status = `update_channel must be one of [stable, preview], got "nightly".`
			c.statusIsError = true

			assertFits := func(step int, state string) {
				t.Helper()
				for _, line := range strings.Split(c.String(), "\n") {
					if got := lipgloss.Width(line); got > w {
						t.Fatalf("step %d (%s, %s): rendered a %d-cell line into a %d-cell pane — the frame wraps it, which makes the height window's line count a lie (#3430).\n  line: %q",
							step, c.selectedEntry().Key, state, got, w, line)
					}
				}
			}

			for step := 0; step < 45; step++ {
				assertFits(step, "list")
				// The edit hint row is the same function's other return path.
				c.beginEdit()
				assertFits(step, "editing")
				c.cancelEdit()
				c.move(1)
			}
		})
	}
}

// TestConfigPaneHintsAlwaysAdvertiseTheExit is the priority half of #3430: what
// the degradation ladder is allowed to take, and what it must never take.
//
// A hint row that has shed everything must still tell the user how to LEAVE. A
// modal whose escape route is invisible is the #2830 failure — an advertised key
// that is not live where focus is — run backwards: the key still works, but a
// user who cannot see it is stuck in a pane they did not know how to close. So
// `esc` survives every width, and the assistant button (#2453, deliberately
// always-on) is the last thing dropped before it.
func TestConfigPaneHintsAlwaysAdvertiseTheExit(t *testing.T) {
	for _, w := range append([]int{4, 9, 12, 20, 23, 32, 43}, configPaneSweepWidths...) {
		c := NewConfigPane()
		c.SetSize(w, paneHeight)
		c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml")
		c.SetFocus(true)
		c.rebuildRows()

		hints := c.renderHints()
		if !strings.Contains(hints, "esc") {
			t.Errorf("w=%d: the hint row shed the exit: %q", w, hints)
		}
		c.beginEdit()
		editing := c.renderHints()
		if !strings.Contains(editing, "esc") {
			t.Errorf("w=%d: the editing hint row shed the exit: %q", w, editing)
		}
		c.cancelEdit()
	}
}

// TestConfigPaneEditFieldFitsItsRowWithoutClipping tests sizeEditField's
// arithmetic directly, WITHOUT the fitPaneLine backstop in the way.
//
// This is the assertion the width sweep cannot make. A field one cell too wide
// still leaves the pane fitting its box — the backstop clips the row — so the
// sweep passes while the user loses the last character of the value they are
// editing. The play-test caught exactly that (`…TAILMAR…` where TAILMARK
// should have been), which is what a focused textinput's extra cursor cell costs
// if the budget does not account for it.
//
// The floor is the one exemption: at a narrow pane with a long key there is no
// arithmetic that fits, and the backstop is then the honest answer.
func TestConfigPaneEditFieldFitsItsRowWithoutClipping(t *testing.T) {
	for _, w := range configPaneSweepWidths {
		c := NewConfigPane()
		c.SetSize(w, paneHeight)
		c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml")
		c.SetFocus(true)
		c.showAdvanced = true
		c.rebuildRows()

		for step := 0; step < 45; step++ {
			e := c.selectedEntry()
			if e == nil {
				c.move(1)
				continue
			}
			c.beginEdit()
			keyBudget, _ := c.editRowSplit(e.Key)
			row := entryRowChromeWidth + keyBudget + lipgloss.Width(c.input.View())
			// No exemption: since editRowSplit yields the KEY rather than the field
			// (#3430 review), every width in the table has a split that fits, so the
			// backstop must never be what saves this row.
			if row > w {
				t.Errorf("w=%d %s: the open value field composes a %d-cell row — the pane clips it, so the value loses its tail (keyBudget=%d, input.Width=%d, prompt=%d, view=%d)",
					w, e.Key, row, keyBudget, c.input.Width, lipgloss.Width(c.input.Prompt), lipgloss.Width(c.input.View()))
			}
			if c.input.Width < minEditFieldWidth {
				t.Errorf("w=%d %s: the value field was squeezed to %d cells, below the %d-cell floor",
					w, e.Key, c.input.Width, minEditFieldWidth)
			}
			c.cancelEdit()
			c.move(1)
		}
	}
}

// TestConfigPaneDisplayRowsStayOneLine covers the height axis of the same
// invariant: a value holding a newline must not turn one list row into several.
//
// An unrestricted string key accepts embedded newlines, and lipgloss.Width
// reports the WIDEST line of a multi-line string — so a line-oriented clip
// measures such a value as narrow and passes it through whole. The row count then
// disagrees with what renderRowLines reported, which is the same way the height
// window breaks.
func TestConfigPaneDisplayRowsStayOneLine(t *testing.T) {
	entries := config.ManifestWithValues(config.DefaultConfig())
	seeded := false
	for i := range entries {
		if entries[i].Key == "on_archive_command" {
			entries[i].Value = "echo one\necho two\techo three\r\necho four"
			seeded = true
		}
	}
	if !seeded {
		t.Fatal("on_archive_command left the manifest — this test is vacuous")
	}

	c := NewConfigPane()
	c.SetSize(72, paneHeight)
	c.SetEntries(entries, "/tmp/config.toml")
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()

	for step := 0; step < 45; step++ {
		for _, line := range strings.Split(c.String(), "\n") {
			if strings.Contains(line, "echo one") && strings.Contains(line, "echo four") {
				// Everything on ONE row is the point; the clip may cut it shorter.
				break
			}
			if strings.Contains(line, "echo two") && !strings.Contains(line, "echo one") {
				t.Fatalf("step %d: a multiline value spilled onto its own row: %q", step, line)
			}
		}
		c.move(1)
	}
}

// TestConfigPaneEditFieldSurvivesTheTightestRow is the #3430 review's P1: making
// the row fit must not fit it by deleting the thing the user is looking at.
//
// af renders normally down to layout.HardMinWidth (40 columns), which
// app/render.go turns into 34 content cells for this pane. The cursor,
// network.require_loopback_token (30 cells) and the gap consume all 34 by
// themselves — so clipping the composed row from the right satisfies the width
// invariant by removing the entire focused input, and the user types into a field
// they cannot see. The key yields instead.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the row renders as the full key followed by
// an ellipsis, with no prompt, no value and no cursor.
func TestConfigPaneEditFieldSurvivesTheTightestRow(t *testing.T) {
	const (
		w   = 34 // a 40-column terminal, af's supported minimum
		key = "network.require_loopback_token"
	)
	c := NewConfigPane()
	c.SetSize(w, paneHeight)
	c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml")
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()

	found := false
	for step := 0; step < 60; step++ {
		e := c.selectedEntry()
		if e != nil && e.Key == key {
			found = true
			break
		}
		c.move(1)
	}
	if !found {
		t.Fatalf("%s left the manifest — this test is vacuous", key)
	}

	c.beginEdit()
	// Find it by the SELECTION CURSOR, not by the key: the key is truncated by
	// the fix, and a prefix match would land on the shorter network.require_token.
	var row string
	for _, line := range strings.Split(c.String(), "\n") {
		if strings.Contains(line, "› ") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("the editing row did not render at all")
	}
	if got := lipgloss.Width(row); got > w {
		t.Errorf("the editing row is %d cells in a %d-cell pane: %q", got, w, row)
	}
	// The field must be there: its prompt, and the value it was filled with.
	if !strings.Contains(row, c.input.Prompt) {
		t.Errorf("the value field's prompt was clipped away — the user cannot see what they are editing: %q", row)
	}
	if !strings.Contains(row, "false") {
		t.Errorf("the value was clipped away, so the field is invisible while focused: %q", row)
	}
	// And the key is what paid for it.
	if strings.Contains(row, key) {
		t.Errorf("the full key survived, so the field cannot have had room: %q", row)
	}
	if !strings.Contains(row, "…") {
		t.Errorf("the key was not truncated, so nothing yielded: %q", row)
	}
}

// TestConfigPaneEditFieldReflowsOnResize is the #3430 review's reflow finding.
//
// textinput recomputes its horizontal viewport in handleOverflow, which runs from
// SetValue and SetCursor and NOT from a bare Width assignment. So narrowing the
// terminal mid-edit updated Width while offset/offsetRight still described the old
// width: View() kept rendering the old, wider slice, fitPaneLine clipped it from
// the right, and the value's tail and the cursor vanished until the next keystroke
// happened to recompute the viewport.
//
// The assertion is on the FIELD, not the row: the row fits either way because the
// backstop clips it, which is exactly why this needs its own test.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: after narrowing 72 -> 38, the field still
// renders its 72-cell slice.
func TestConfigPaneEditFieldReflowsOnResize(t *testing.T) {
	const key = "vscode_server_binary"
	long := "/opt/" + strings.Repeat("very-long-path-segment/", 8) + "bin"

	// EVERY cursor position, not just the end. handleOverflow moves the offsets only
	// when the cursor is OUTSIDE the window it already has (pos < offset, or
	// pos >= offsetRight), so a test that leaves the cursor at CursorEnd always
	// satisfies the second condition and passes while an interior cursor is still
	// stranded — which is what the first version of this test did.
	//
	// A FRESH pane per case, deliberately: sharing one leaks viewport state from the
	// previous case, and a case that inherits an already-narrow window passes without
	// exercising anything. Measured — with the pane shared, the interior case passed
	// against an implementation the start case had just failed.
	for _, tc := range []struct {
		name string
		// posOf picks the cursor position from the value's length.
		posOf func(n int) int
	}{
		{name: "cursor at the start", posOf: func(int) int { return 0 }},
		{name: "cursor in the interior", posOf: func(n int) int { return n / 2 }},
		{name: "cursor at the end", posOf: func(n int) int { return n }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := config.ManifestWithValues(config.DefaultConfig())
			seeded := false
			for i := range entries {
				if entries[i].Key == key {
					entries[i].Value = long
					seeded = true
				}
			}
			if !seeded {
				t.Fatalf("%s left the manifest — this test is vacuous", key)
			}

			c := NewConfigPane()
			c.SetSize(72, paneHeight)
			c.SetEntries(entries, "/tmp/config.toml")
			c.SetFocus(true)
			c.showAdvanced = true
			c.rebuildRows()
			for step := 0; step < 60; step++ {
				if e := c.selectedEntry(); e != nil && e.Key == key {
					break
				}
				c.move(1)
			}
			if e := c.selectedEntry(); e == nil || e.Key != key {
				t.Fatalf("never landed on %s", key)
			}
			c.beginEdit()
			pos := tc.posOf(len(c.input.Value()))
			c.input.SetCursor(pos)

			// The terminal narrows while the edit is open. SetSize is what a resize calls.
			c.SetSize(38, paneHeight)

			budget := c.input.Width + lipgloss.Width(c.input.Prompt) + editFieldCursorWidth
			if got := lipgloss.Width(c.input.View()); got > budget {
				t.Fatalf("after narrowing to 38 with the %s, the field renders %d cells for a %d-cell budget (Width=%d) — the viewport still describes the old width, so the row gets clipped and the value's tail and cursor disappear",
					tc.name, got, budget, c.input.Width)
			}
			// And the reflow must not have dragged the cursor.
			if c.input.Position() != pos {
				t.Errorf("the reflow moved the cursor: position %d, want %d", c.input.Position(), pos)
			}
		})
	}
}

// TestConfigPaneEditFieldUnboundedWhileUnsized is the review's other half: an
// unsized pane constrains nothing.
//
// fitPaneLine, fitHints and window all pass content through at width 0, and
// textinput reads Width 0 as unbounded. Deriving a field width from a pane with no
// width put this one member out of step with all three and rendered a narrow
// scrolling tail where there is no box to fit.
func TestConfigPaneEditFieldUnboundedWhileUnsized(t *testing.T) {
	long := "/opt/" + strings.Repeat("very-long-path-segment/", 8) + "bin"
	entries := config.ManifestWithValues(config.DefaultConfig())
	for i := range entries {
		if entries[i].Key == "vscode_server_binary" {
			entries[i].Value = long
		}
	}

	// No SetSize at all.
	c := NewConfigPane()
	c.SetEntries(entries, "/tmp/config.toml")
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()
	c.beginEdit()
	if c.input.Width != 0 {
		t.Errorf("an unsized pane bounded its value field to %d cells", c.input.Width)
	}

	// And a pane that loses its size goes back to unbounded rather than keeping a
	// stale width.
	c.SetSize(72, paneHeight)
	if c.input.Width == 0 {
		t.Fatal("a sized pane left the field unbounded")
	}
	c.SetSize(0, 0)
	if c.input.Width != 0 {
		t.Errorf("a pane resized to nothing kept a %d-cell field width", c.input.Width)
	}
}

package ui

import (
	"errors"
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
	c.SetSize(100, 40)
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

// TestConfigPaneSurfacesRestartNoticeAtTheMomentOfTheEdit is requirement 3 for
// the TUI.
//
// The daemon reads config.toml at STARTUP. An editor that changes a value the
// running daemon then ignores, and says nothing, is a lie by omission — the same
// class as a doctor that passes because it cannot see. So the notice appears on
// the successful write, next to the echo, and it names the command to run.
func TestConfigPaneSurfacesRestartNoticeAtTheMomentOfTheEdit(t *testing.T) {
	c := newTestConfigPane(t)
	selectKey(t, c, "default_program")

	// Stub the writer: this test is about what the pane SAYS, not about the file.
	c.save = func(key, value string) (*config.SetResult, error) {
		return &config.SetResult{Key: key, Value: value, Path: "/tmp/config.toml", RequiresRestart: true}, nil
	}

	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	c.input.SetValue("codex")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	view := c.String()
	if !strings.Contains(view, "af daemon restart") {
		t.Errorf("the restart notice must name the command to run — telling a user to 'restart' without saying what leaves them guessing.\n--- view ---\n%s", view)
	}
	if !strings.Contains(view, "read config.toml at startup") {
		t.Errorf("the notice must say WHY the edit is not live yet.\n--- view ---\n%s", view)
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

// TestConfigPaneNeverOffersAKeyTheWriterWouldRefuse pins that the pane trusts
// ConfigEntry.Editable — the honest, allowlist-derived answer — rather than the
// manifest's Settable.
//
// The distinction is the whole finding: program_overrides is Settable, because
// its LEAVES are settable. The bare key holds a table. Keying the cursor off
// Settable let it land there, opened a field pre-filled with the map's JSON, and
// had the writer refuse it on save — a dead end found only by pressing enter.
//
// So: the cursor must never come to rest on a non-editable row, and every such
// row must say what to do instead.
//
// HONEST LIMIT: this test cannot catch the drift itself. It iterates rows that
// are ALREADY marked non-editable, so a regression that wrongly marks a key
// editable simply removes it from this loop and the test stays green — watched
// doing exactly that. What catches the drift is
// config.TestEditableIsNeverAKeyTheWriterWouldRefuse, which comes from the other
// direction: it asks the REAL writer to accept the value every Editable key
// shows, and fails when one is refused. This test's job is narrower and still
// worth having — it pins that clampSelection and beginEdit actually honor the
// flag once it is right.
func TestConfigPaneNeverOffersAKeyTheWriterWouldRefuse(t *testing.T) {
	c := newTestConfigPane(t)

	var readOnly int
	for i, row := range c.rows {
		if row.entry == nil || row.entry.Editable {
			continue
		}
		readOnly++

		// Park the cursor on it and let the pane settle: it must move away.
		c.selectedIdx = i
		c.clampSelection()
		if landed := c.selectedEntry(); landed != nil && !landed.Editable {
			t.Errorf("the cursor rested on %q, which the writer would refuse — enter there can only dead-end", landed.Key)
		}

		// And pressing enter on it must not open a field.
		c.selectedIdx = i
		c.beginEdit()
		if c.IsEditing() {
			t.Errorf("enter opened an edit field on %q, whose save the writer would refuse", row.entry.Key)
			c.cancelEdit()
		}

		if row.entry.EditHint == "" {
			t.Errorf("%q is read-only but says nothing about how to change it", row.entry.Key)
		}
	}
	if readOnly == 0 {
		t.Fatal("no read-only rows found — this test is asserting nothing")
	}
}

// TestConfigPaneNamesTheCommandForDynamicTables pins the COPY for a dynamic
// family. "hand-edited in config.toml" would be false here: `af config set
// program_overrides.claude …` works, and sending a user to a text editor for
// something af does for them is a smaller lie than the dead-end field, but still
// one.
func TestConfigPaneNamesTheCommandForDynamicTables(t *testing.T) {
	c := newTestConfigPane(t)
	view := c.String()

	if !strings.Contains(view, "af config set program_overrides.<name>") {
		t.Errorf("the editor must name the command that WORKS for a dynamic table.\n--- view ---\n%s", view)
	}
}

// TestConfigPaneEditFieldTakesArbitraryValueText is the reason this pane owns the
// keyboard while open: a config value is arbitrary text, and the global key map
// must not eat it.
//
// "127.0.0.1:8080" is the real case (listen_addr) — it contains ".", ":" and
// digits, and the digits 1-9 are the TUI's tab-jump keys. If the pane did not
// consume them, typing an address would jump tabs instead.
func TestConfigPaneEditFieldTakesArbitraryValueText(t *testing.T) {
	c := newTestConfigPane(t)
	selectKey(t, c, "listen_addr")

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
	c.save = func(k, v string) (*config.SetResult, error) {
		return &config.SetResult{Key: k, Value: v, Path: "/tmp/config.toml", RequiresRestart: true}, nil
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
	if strings.Contains(view, "daemon restart") {
		t.Errorf("a reopened editor showed a stale restart notice for an edit the user cannot see.\n--- view ---\n%s", view)
	}
}

// TestConfigPaneClosingClearsAStaleError is the same guard for the error line: a
// rejected value's message must not greet the user on their next open.
func TestConfigPaneClosingClearsAStaleError(t *testing.T) {
	c := newTestConfigPane(t)
	selectKey(t, c, "update_channel")
	c.save = func(k, v string) (*config.SetResult, error) {
		return nil, errStubRejected
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

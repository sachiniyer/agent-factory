package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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

// TestConfigPaneCannotEditHandEditedKeys pins that the pane trusts the manifest's
// Settable flag — which is itself pinned against the real `af config set`
// allowlist — rather than deriving a second opinion.
//
// A key the CLI will not set must not be offered as editable here: the write
// would be rejected, so an editable-looking field would be a dead end. The pane
// says the key is hand-edited instead, because it is, by design.
func TestConfigPaneCannotEditHandEditedKeys(t *testing.T) {
	c := newTestConfigPane(t)

	var readOnly []string
	for _, e := range config.Manifest() {
		if !e.Settable {
			readOnly = append(readOnly, e.Key)
		}
	}
	if len(readOnly) == 0 {
		t.Skip("no read-only keys in the manifest")
	}

	view := c.String()
	if !strings.Contains(view, "hand-edited in config.toml") {
		t.Error("a read-only key must say WHY it cannot be edited here")
	}

	// The cursor must never land on one: enter there could only fail.
	for i, row := range c.rows {
		if row.entry != nil && !row.entry.Settable {
			c.selectedIdx = i
			c.clampSelection()
			landed := c.selectedEntry()
			if landed != nil && !landed.Settable {
				t.Errorf("the cursor landed on read-only key %q; enter there can only be refused by the writer", landed.Key)
			}
		}
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

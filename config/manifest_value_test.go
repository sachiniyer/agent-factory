package config

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestCurrentValueCoversEveryManifestKey is one half of the anti-drift
// guarantee, and it is the half the EDITORS depend on.
//
// TestManifestCoversEveryConfigKey already proves every toml-tagged Config field
// has a manifest entry. This proves the entry is READABLE: that its Key resolves
// to a real field through the reflection walk both editor surfaces use to fill
// their form. A manifest entry whose Key is a typo would satisfy the coverage
// test's field→entry direction via some other entry and still render "unknown"
// in both UIs; this closes that.
//
// Adding a key to config_types.go therefore cannot ship a half-working editor:
// either it has an entry that reads (both surfaces render it), or a test fails.
func TestCurrentValueCoversEveryManifestKey(t *testing.T) {
	cfg := DefaultConfig()
	for _, e := range Manifest() {
		if _, ok := CurrentValue(cfg, e.Key); !ok {
			t.Errorf("manifest key %q does not resolve to a Config field — both config editors would render it as unreadable", e.Key)
		}
	}
}

// TestCurrentValueRejectsUnknownAndNil pins the !ok contract the editors rely on
// to avoid showing a default as if it were the user's live value.
func TestCurrentValueRejectsUnknownAndNil(t *testing.T) {
	if v, ok := CurrentValue(nil, "default_program"); ok {
		t.Errorf("nil cfg must not resolve, got %q", v)
	}
	if v, ok := CurrentValue(DefaultConfig(), "no_such_key_at_all"); ok {
		t.Errorf("unknown key must not resolve, got %q", v)
	}
}

// TestCurrentValueRoundTripsThroughConfigSet is the round-trip contract behind
// both editors: for every settable scalar key, the value the editor RENDERS into
// its field is a value the REAL write path (SetGlobalConfigValue — the same
// locked, validated, atomic path `af config set` uses) accepts back unchanged.
//
// This is not a tautology. It is what stops an editor from writing its own
// display decorations into config.toml: rendering an unset string with the
// briefing's `""` would round-trip two literal quote characters into the user's
// file, and the user would never have typed them. Opening the editor and
// pressing save on an untouched field must be a no-op, for every key.
//
// It runs over Manifest() rather than a fixed list, so a new settable key is
// covered the day it is added — including one whose default the editor cannot
// render or the validator will not take.
//
// THE COMPARISON IS AGAINST THE CONFIG FIELD, NOT THE RENDERED STRING, and that
// is load-bearing rather than incidental. An earlier version of this test
// compared CurrentValue before against CurrentValue after and was WATCHED
// PASSING against the exact bug it exists to catch: with CurrentValue wired to
// the briefing renderer, `vscode_server_binary` rendered as `""`, wrote
// `vscode_server_binary = '""'` to the file, reloaded as a two-character string
// — and still compared equal, because a systematic decoration bug is stable
// under its own renderer. Reading the field through reflection breaks that
// symmetry: the Go value was "" before and `""` after, which is the corruption
// the user would actually meet.
func TestCurrentValueRoundTripsThroughConfigSet(t *testing.T) {
	for _, e := range Manifest() {
		if !e.Settable {
			continue
		}
		_, ok := settableKeySpecs[e.Key]
		if !ok {
			t.Errorf("manifest marks %q settable but there is no spec — TestManifestAgreesWithSettableKeys should have caught this", e.Key)
			continue
		}
		t.Run(e.Key, func(t *testing.T) {
			writeTempConfig(t, "# a hand-written config\n")

			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			shown, ok := CurrentValue(cfg, e.Key)
			if !ok {
				t.Fatalf("editor cannot render a value for settable key %q", e.Key)
			}
			fieldBefore, ok := configFieldByTomlKey(cfg, e.Key)
			if !ok {
				t.Fatalf("%s names no Config field", e.Key)
			}
			valueBefore := fieldBefore.Interface()

			res, err := SetGlobalConfigValue(e.Key, shown)
			if err != nil {
				t.Fatalf("the editor renders %q for %s, but the real write path rejects it: %v\n"+
					"an editor field pre-filled with this value would fail to save untouched", shown, e.Key, err)
			}
			if res.Value != shown {
				t.Fatalf("%s: set canonicalized %q to %q — the editor would show a value the file does not hold", e.Key, shown, res.Value)
			}

			reloaded, err := LoadConfig()
			if err != nil {
				t.Fatalf("config does not load after writing %s: %v", e.Key, err)
			}
			fieldAfter, ok := configFieldByTomlKey(reloaded, e.Key)
			if !ok {
				t.Fatalf("%s: unreadable after write", e.Key)
			}
			valueAfter := fieldAfter.Interface()

			// Compare the Go values, not their rendering — see the doc comment.
			// Theme's unexported preset metadata records whether the source used a
			// named scalar or a custom table. Sending its displayed JSON deliberately
			// chooses the table shape, so compare the nineteen user-facing slots; a
			// named preset gets its own scalar-shape round-trip test.
			equal := reflect.DeepEqual(valueBefore, valueAfter)
			if e.Key == "theme" {
				before := valueBefore.(ThemeConfig)
				after := valueAfter.(ThemeConfig)
				before.preset, before.explicitPreset = "", false
				after.preset, after.explicitPreset = "", false
				equal = reflect.DeepEqual(before, after)
			}
			if !equal {
				t.Fatalf("saving %s untouched CHANGED it: the editor showed %q, and writing that back turned %#v into %#v.\n"+
					"an editor must be able to save a field the user never touched without altering it", e.Key, shown, valueBefore, valueAfter)
			}
		})
	}
}

// TestCurrentValueCommaJoinsOnlyOptedInLists is the #2564-review lock: the comma
// form is PER-KEY opt-in (isCommaListKey), never inferred from the []string type.
// A settable comma-list key (cors_allowed_origins) renders comma-joined; a
// []string key that has NOT opted in (session_env_passthrough) keeps the
// unambiguous compact-JSON form — so a future list whose elements can contain a
// comma is not silently displayed as more entries than it holds.
func TestCurrentValueCommaJoinsOnlyOptedInLists(t *testing.T) {
	cfg := &Config{
		CORSAllowedOrigins:    []string{"https://a.example.com", "https://b.example.com"},
		SessionEnvPassthrough: []string{"FOO", "BAR"},
	}

	if got, _ := CurrentValue(cfg, "cors_allowed_origins"); got != "https://a.example.com,https://b.example.com" {
		t.Errorf("an opted-in comma-list key must render comma-joined, got %q", got)
	}
	// session_env_passthrough is a []string but is NOT a cfgStringList settable
	// key, so it must NOT render comma-joined — it keeps compact JSON.
	if got, _ := CurrentValue(cfg, "session_env_passthrough"); got != `["FOO","BAR"]` {
		t.Errorf("a non-opted-in []string key must keep compact JSON (not comma-joined), got %q", got)
	}
}

// TestEditorWriteKeepsConfigHandEditable is the external-user guard: config.toml
// is a file the README tells people to hand-edit, and an edit made from a UI must
// leave it exactly as hand-editable as it found it — every comment, blank line,
// and the user's own key ordering intact, with only the edited value's bytes
// changed.
//
// The editors get this for free by calling SetGlobalConfigValue rather than
// re-marshaling Config (which would strip every comment). This test is the lock
// that says so from the editors' side: it is the property a future "just
// toml.Marshal the struct back" shortcut would silently destroy.
func TestEditorWriteKeepsConfigHandEditable(t *testing.T) {
	orig := "# my notes, do not lose these\n" +
		"\n" +
		"# the agent I actually use\n" +
		"default_program = 'claude'   # trailing note\n" +
		"auto_update = false\n" +
		"\n" +
		"# overrides below\n" +
		"[program_overrides]\n" +
		"claude = '/bin/claude'  # custom build\n"
	path := writeTempConfig(t, orig)

	if _, err := SetGlobalConfigValue("default_program", "codex"); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)

	for _, mustKeep := range []string{
		"# my notes, do not lose these",
		"# the agent I actually use",
		"# trailing note",
		"# overrides below",
		"# custom build",
		"[program_overrides]",
		"claude = '/bin/claude'",
	} {
		if !strings.Contains(text, mustKeep) {
			t.Errorf("an edit from the config editor destroyed hand-written content: %q is gone\n--- file ---\n%s", mustKeep, text)
		}
	}
	if !strings.Contains(text, "default_program = 'codex'") {
		t.Errorf("the edit did not land.\n--- file ---\n%s", text)
	}
	if strings.Contains(text, "default_program = 'claude'") {
		t.Errorf("the old value survived the edit.\n--- file ---\n%s", text)
	}

	// Still valid TOML that the loader accepts — not merely textually plausible.
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("config.toml is no longer loadable after an editor write: %v", err)
	}
}

// TestEditorRejectsInvalidValueWithTheCLIsOwnError pins requirement 2 from the
// editors' side: a bad value is refused BEFORE the write, with the validator's
// own message, and nothing reaches the file.
//
// The editors do not carry their own validation table — they call
// SetGlobalConfigValue and surface the error verbatim. That is the point: a
// second copy of the rules is how a UI comes to accept a value the loader will
// reject at startup, which the user then meets as a crash instead of a form
// error.
func TestEditorRejectsInvalidValueWithTheCLIsOwnError(t *testing.T) {
	cases := []struct {
		key, value, wantErrContains string
	}{
		{"default_program", "emacs", "default_program"},
		{"update_channel", "nightly", "update_channel must be one of"},
		{"daemon_poll_interval", "0", "must be positive"},
		// A syntactically invalid duration is caught by canonicalizeScalar before
		// the range validator ever runs, so the message differs from "0" above.
		{"daemon_poll_interval", "abc", `expected a Go duration`},
		{"log_max_backups", "-1", "must be a non-negative integer"},
		{"auto_yes", "yes-please", "auto_yes was removed"},
		{"worktree_root", "somewhere-else", "worktree_root must be one of"},
		{"listen_addr", "not a valid addr", "listen_addr"},
	}

	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			orig := "default_program = 'claude'\n"
			path := writeTempConfig(t, orig)

			_, err := SetGlobalConfigValue(tc.key, tc.value)
			if err == nil {
				t.Fatalf("%s = %q was accepted; the editor would write a value the loader rejects at startup", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("error must name the problem for a user reading it in a form field.\n got: %v\nwant substring: %q", err, tc.wantErrContains)
			}

			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != orig {
				t.Fatalf("a REJECTED edit still touched config.toml.\n got: %q\nwant: %q", got, orig)
			}
		})
	}
}

// TestSetResultEchoesKeyAndValue pins requirement 4: the write path reports back
// exactly what it set, so every surface can echo `key = value` from the same
// source rather than echoing what it *believes* it sent.
func TestSetResultEchoesKeyAndValue(t *testing.T) {
	writeTempConfig(t, "default_program = 'claude'\n")

	res, err := SetGlobalConfigValue("default_program", "codex")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if res.Key != "default_program" || res.Value != "codex" {
		t.Fatalf("echo must carry key and value, got %+v", res)
	}
	if !res.RequiresRestart {
		t.Fatal("RequiresRestart must stay true: every save surface uses it to show the writer's exact per-key effect notice")
	}
}

// TestEveryManifestValueIsAcceptedByTheWriter is the honesty lock behind every
// control in both panes. There is no read-only escape hatch: every value the
// manifest renders must be accepted by the real writer unchanged.
func TestEveryManifestValueIsAcceptedByTheWriter(t *testing.T) {
	for _, e := range ManifestWithValues(DefaultConfig()) {
		t.Run(e.Key, func(t *testing.T) {
			writeTempConfig(t, "# hand-written\n")
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			shown, ok := CurrentValue(cfg, e.Key)
			if !ok {
				t.Fatalf("%s has no readable editor value", e.Key)
			}
			if _, err := SetGlobalConfigValue(e.Key, shown); err != nil {
				t.Fatalf("both editors render %s, but saving the value they show is refused: %v", e.Key, err)
			}
		})
	}
}

// TestEveryManifestRowIsSettableAndHasNoReadOnlyClass is the owner directive
// from #3345. The wire manifest must not recreate the removed Editable/EditHint
// class, and every global row must name a writer spec.
func TestEveryManifestRowIsSettableAndHasNoReadOnlyClass(t *testing.T) {
	for _, e := range ManifestWithValues(DefaultConfig()) {
		if !e.Settable {
			t.Errorf("%s has no config-set writer", e.Key)
		}
	}
	payload, err := json.Marshal(ManifestWithValues(DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{`"editable"`, `"edit_hint"`} {
		if strings.Contains(string(payload), retired) {
			t.Errorf("wire manifest still contains retired read-only field %s: %s", retired, payload)
		}
	}
}

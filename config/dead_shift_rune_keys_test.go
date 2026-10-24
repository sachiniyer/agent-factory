package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/keys"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/require"
)

// TestDeadShiftRuneOverrideLoadsAndWarns pins the "warn now, reject later"
// policy (#4599): a config that already contains a shift+<rune> override
// Bubble Tea can never emit (Key has no Shift field, so Shift+A is emitted as
// "A", never "shift+a") must still load — the binding was inert before and
// stays inert, just loudly — instead of refusing to start (or, in the daemon's
// config reads, refusing to load). Each dead key is named in a warning that
// says it will never fire and is dropped; a reachable override alongside it
// survives. `af config set keys` still rejects WRITING a new dead binding —
// that path calls keys.ValidateOverrides directly, which TestRuneSpecsMatchBubbleTea
// and TestApplyOverridesRejectsShiftRuneDeadBinding in keys/keys_test.go pin.
func TestDeadShiftRuneOverrideLoadsAndWarns(t *testing.T) {
	for _, spec := range []string{"shift+a", "ctrl+shift+a", "alt+shift+a", "alt+ctrl+shift+a", "shift+0", "shift+å"} {
		t.Run(spec, func(t *testing.T) {
			warnings := captureLog(t, &aflog.WarningLog)
			source := t.TempDir() + "/config.toml"
			t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

			// quit carries the dead key alongside a reachable one, so the load
			// drops only the dead spelling and keeps Q; new is a reachable
			// override on a different action, proving the warn-and-skip is
			// scoped to the dead binding and not the whole table.
			cfg, err := parseConfigTOML([]byte(fmt.Sprintf(
				"[keys]\nquit = [\"%s\", \"Q\"]\nnew = \"alt+a\"\n", spec)), source)
			require.NoError(t, err, "a config with a dead shift+<rune> override must still load")
			require.Equal(t, []string{"Q"}, cfg.KeymapOverrides()["quit"],
				"the dead binding %q must be dropped, leaving the reachable Q", spec)
			require.Equal(t, []string{"alt+a"}, cfg.KeymapOverrides()["new"],
				"a reachable override alongside the dead one must survive")
			require.NotContains(t, cfg.KeymapOverrides()["quit"], spec)

			require.NoError(t, keys.ApplyOverrides(cfg.KeymapOverrides()),
				"the cleaned overrides must apply")
			require.Equal(t, keys.KeyQuit, keys.GlobalKeyStringsMap["Q"],
				"the reachable Q dispatches quit")
			require.Equal(t, keys.KeyNew, keys.GlobalKeyStringsMap["alt+a"],
				"the reachable alt+a override dispatches new")
			require.NotContains(t, keys.GlobalKeyStringsMap, spec,
				"the dead spelling must not be installed into the dispatch map")

			require.Contains(t, warnings.String(), spec,
				"the warning must name the dead key")
			require.Contains(t, warnings.String(), "will never fire",
				"the warning must say the binding will never fire")
			require.Equal(t, 1, strings.Count(warnings.String(), "will never fire"),
				"warn once per dead key per source")
		})
	}
}

// TestDeadShiftRuneOverrideLoadOncePerSource pins the once-per-source memo: the
// daemon reloads config on every session-create, so a dead binding must warn
// once per (source, action, key), not once per load (#2496). Two loads of the
// same config produce one warning; a second dead binding in the same file still
// warns.
func TestDeadShiftRuneOverrideLoadOncePerSource(t *testing.T) {
	warnings := captureLog(t, &aflog.WarningLog)
	source := t.TempDir() + "/config.toml"
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })
	data := []byte("[keys]\nquit = [\"shift+a\", \"Q\"]\nnew = [\"shift+z\", \"alt+z\"]\n")
	for range 2 {
		_, err := parseConfigTOML(data, source)
		require.NoError(t, err, "the config must load on every reload")
	}
	require.Equal(t, 1, strings.Count(warnings.String(), "\"shift+a\""),
		"the shift+a warning fires once across reloads of the same source")
	require.Equal(t, 1, strings.Count(warnings.String(), "\"shift+z\""),
		"the shift+z warning fires once across reloads of the same source")
}

// TestDeadShiftRuneOverrideAllDeadKeepsUnknownActionVisible pins the Codex
// finding on the warn-and-skip: when EVERY binding for an action is a dead
// shift+<rune> spec, dropping them all must not omit a typo'd action from
// cleaned and hide it from keys.ValidateOverrides (unknown actions are
// otherwise hard errors). A typo'd action whose only binding is dead still
// fails to load naming the unknown action — the warning fires but no longer
// buries the typo — while a KNOWN action whose only binding is dead still
// loads, warns, and resolves to its default (the upgrade case the
// warn-and-skip exists for).
func TestDeadShiftRuneOverrideAllDeadKeepsUnknownActionVisible(t *testing.T) {
	t.Run("unknown action whose only binding is dead is rejected, not hidden", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		source := t.TempDir() + "/config.toml"
		_, err := parseConfigTOML([]byte("[keys]\ntypo = \"shift+a\"\n"), source)
		require.Error(t, err, "a typo'd action hidden behind a dead shift+<rune> binding must not load")
		require.Contains(t, err.Error(), "unknown action", "the unknown-action error must surface, not be hidden behind the dead-key warning")
		require.Contains(t, err.Error(), "typo", "the error must name the typo'd action")
		require.Contains(t, warnings.String(), "shift+a", "the dead-key warning still fires for the spec")
		require.Contains(t, warnings.String(), "will never fire")
	})

	t.Run("known action whose only binding is dead still loads and resolves to default", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		source := t.TempDir() + "/config.toml"
		t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })
		cfg, err := parseConfigTOML([]byte("[keys]\nquit = [\"shift+a\"]\n"), source)
		require.NoError(t, err, "a known action whose only binding is a dead shift+<rune> must still load (warn-and-skip)")
		require.Empty(t, cfg.KeymapOverrides()["quit"], "the dead binding is dropped; quit resolves to its default")
		require.NoError(t, keys.ApplyOverrides(cfg.KeymapOverrides()), "the cleaned overrides must apply")
		require.Equal(t, keys.KeyQuit, keys.GlobalKeyStringsMap["q"], "quit's default q is restored")
		require.NotContains(t, keys.GlobalKeyStringsMap, "shift+a", "the dead spelling must not be installed")
		require.Contains(t, warnings.String(), "shift+a", "the dead-key warning names the spec")
		require.Contains(t, warnings.String(), "will never fire")
		require.Equal(t, 1, strings.Count(warnings.String(), "will never fire"), "warn once per dead key per source")
	})
}

// TestDeadShiftRuneOverrideEditableValueReflectsCleaned pins the Codex finding on
// the warn-and-skip: dropping the dead binding only from the local overrides
// would leave the raw [keys] table (config.Keys) pre-filling the config editor
// (CurrentValue/ManifestWithValues) with the dead spec. An unchanged pane save
// routes that value through SetGlobalConfigValue's structured keys validation
// (keys.ValidateOverrides), which rejects the dead spec the loader just warned
// and skipped — so an upgrade config that loaded could not be edited through the
// built-in editor. The dead binding is dropped from the raw table too, so the
// editor shows exactly what the loader applied and an unchanged save round-trips.
func TestDeadShiftRuneOverrideEditableValueReflectsCleaned(t *testing.T) {
	t.Run("list with one dead and one reachable binding", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		source := filepath.Join(t.TempDir(), "config.toml")
		cfg, err := parseConfigTOML([]byte("[keys]\nquit = [\"shift+a\", \"Q\"]\nnew = \"alt+a\"\n"), source)
		require.NoError(t, err, "a config with a dead shift+<rune> override alongside a reachable one must load")
		require.Contains(t, warnings.String(), "will never fire", "the dead binding warns")

		value, ok := CurrentValue(cfg, "keys")
		require.True(t, ok, "CurrentValue must render the keys table")
		require.NotContains(t, value, "shift+a", "the editable value must not pre-fill the dead spec the loader warned and skipped")
		require.Contains(t, value, "Q", "the reachable binding must survive in the editable value")
		require.Contains(t, value, "alt+a", "an unrelated reachable binding must survive in the editable value")

		// The whole point of the fix: an unchanged pane save routes CurrentValue
		// through SetGlobalConfigValue's structured keys validation
		// (keys.ValidateOverrides), which would reject the dead spec. The cleaned
		// value must round-trip.
		writeTempConfig(t, "")
		if _, err := SetGlobalConfigValue("keys", value); err != nil {
			t.Fatalf("an unchanged save of the cleaned keys value must round-trip, got: %v", err)
		}
	})

	t.Run("known action whose only binding is dead resolves to default in the editor", func(t *testing.T) {
		_ = captureLog(t, &aflog.WarningLog)
		source := filepath.Join(t.TempDir(), "config.toml")
		cfg, err := parseConfigTOML([]byte("[keys]\nquit = [\"shift+a\"]\n"), source)
		require.NoError(t, err, "a known action whose only binding is dead must load (warn-and-skip)")

		value, ok := CurrentValue(cfg, "keys")
		require.True(t, ok)
		require.Equal(t, "{}", value, "the all-dead known action is dropped from the raw table; the editor shows no override, so quit resolves to its default")
	})
}

// TestDeadShiftRuneWarningQuotesActionName pins the Codex finding on the
// warning: a quoted TOML action name can carry a terminal control sequence
// (ESC, \x1b), and the warning runs before keys.ValidateOverrides rejects the
// unknown action. The raw action must be rendered with %q (not %s) so the
// escape sequence is escaped in the message, not emitted verbatim through the
// interactive writer (which af config validate mirrors to stderr) — validating
// a malicious or corrupted config must not execute terminal escape sequences.
func TestDeadShiftRuneWarningQuotesActionName(t *testing.T) {
	warnings := captureLog(t, &aflog.WarningLog)
	source := filepath.Join(t.TempDir(), "config.toml")
	// A quoted TOML key decodes \u001b to an ESC byte in the action name.
	_, err := parseConfigTOML([]byte("[keys]\n\"\\u001b[2J\" = \"shift+a\"\n"), source)
	require.Error(t, err, "an unknown action with a dead binding must still be rejected")
	require.Contains(t, err.Error(), "unknown action", "the unknown-action error must surface, not be hidden behind the dead-key warning")
	require.Contains(t, warnings.String(), "will never fire", "the dead-key warning fires")
	require.NotContains(t, warnings.String(), "\x1b[2J", "the raw ESC control sequence must not be emitted verbatim in the warning")
	require.Contains(t, warnings.String(), `\x1b`, "the action name must be %q-quoted so the escape sequence is escaped, not executed")
}

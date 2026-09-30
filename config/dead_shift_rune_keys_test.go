package config

import (
	"fmt"
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

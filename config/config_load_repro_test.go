package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression test for the in-lock empty-config.json shadow bug in
// convertJSONToTOML (config/config_load.go). When the in-lock re-read of
// config.json finds it empty or gone (an external in-place editor truncated it
// in the inter-read window — the #4483 in-place-rewrite fingerprint), the load
// must answer in-memory defaults and write NOTHING, mirroring loadConfig's
// pre-lock zero-byte path. The prior code called materializeDefaultConfig,
// which created a brand-new config.toml of built-in defaults during the load;
// that file is canonical on the next load and shadows the user's config.json
// even after the editor rewrites it.
//
// This test recreates the inter-read window directly (via convertRaceHookForTest)
// rather than racing for it, the same way TestExposureWarningJudgesTheFileInsideTheLock
// and TestConversion_LostRaceAdoptsWinnersTOML pin their locked-body windows.

func TestLoadConfig_HookBasedShadowRepro(t *testing.T) {
	home := t.TempDir()
	fastShell(t)
	jsonPath := filepath.Join(home, ConfigFileName)
	bakPath := jsonPath + ".bak"
	tomlPath := filepath.Join(home, TomlConfigFileName)
	const jsonBody = `{"schema_version":1,"default_program":"codex"}`

	require.NoError(t, os.WriteFile(jsonPath, []byte(jsonBody), 0o644))
	t.Setenv("AGENT_FACTORY_HOME", home)

	// Between the pre-lock read (saw the real config.json) and the in-lock
	// re-read: an external editor truncated config.json and has not yet
	// written its content. The race is transient — it fires during ONE load
	// — so the hook is cleared after that load (below), not left armed for
	// the clean reload.
	convertRaceHookForTest = func() {
		_ = os.WriteFile(jsonPath, nil, 0o644)
	}
	t.Cleanup(func() { convertRaceHookForTest = nil })

	// The first load catches the editor mid-flight.
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "claude", cfg.DefaultProgram, "an empty config.json answers built-in defaults in memory")
	// The load-time write that is the bug: a defaults config.toml materialized
	// from a truncated/in-flight config.json would be canonical next load and
	// shadow the user's file. No config.toml may be installed by this branch.
	assert.NoFileExists(t, tomlPath, "the in-lock empty branch must not install a canonical config.toml")

	// The transient race is over: stop simulating the editor mid-flight.
	convertRaceHookForTest = nil

	// The user's editor lands its real content after the momentary truncation.
	require.NoError(t, os.WriteFile(jsonPath, []byte(jsonBody), 0o644))

	cfg2, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg2.DefaultProgram, "the user's config.json must not be shadowed by a defaults config.toml")

	// No data loss. With the fix the second load runs the normal one-time
	// conversion (no shadowing config.toml exists), which writes config.toml
	// with the user's settings and moves config.json aside to .bak. So the
	// user's content survives at .bak. (On the buggy code the second load
	// takes the canonical-toml branch and leaves config.json in place, so the
	// content survives at the original path.) Either way it must be intact.
	jsonData, jsonErr := os.ReadFile(jsonPath)
	bakData, bakErr := os.ReadFile(bakPath)
	switch {
	case jsonErr == nil:
		assert.Equal(t, jsonBody, string(jsonData), "config.json content must be preserved when the canonical-toml branch leaves it in place")
	case bakErr == nil:
		assert.Equal(t, jsonBody, string(bakData), "config.json content must be preserved in the conversion backup when it is renamed")
	default:
		t.Fatalf("config.json was lost: not present at %s or %s (jsonErr=%v, bakErr=%v)", jsonPath, bakPath, jsonErr, bakErr)
	}

	// Any config.toml created during the race must carry the user's settings,
	// not built-in defaults — a defaults-only config.toml is the shadow.
	if tomlData, statErr := os.ReadFile(tomlPath); statErr == nil {
		assert.True(t, bytes.Contains(tomlData, []byte("codex")),
			"a config.toml created during the race must reflect the user's settings, not defaults; got %q", string(tomlData))
		assert.NotContains(t, string(tomlData), "default_program = 'claude'",
			"no canonical defaults config.toml may shadow the user's in-flight config.json; got %q", string(tomlData))
	}
}

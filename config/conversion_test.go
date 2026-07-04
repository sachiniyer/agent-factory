package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aflog "github.com/sachiniyer/agent-factory/log"
)

// Regression + behavior tests for the one-time json→toml conversion (#1030
// PR 2). Sachin's decision: on conversion, config.toml becomes canonical and
// the original config.json is renamed to config.json.bak.

// seedJSONHome points a fresh AGENT_FACTORY_HOME at a temp dir holding only a
// config.json with the given content, and returns the config dir.
func seedJSONHome(t *testing.T, jsonContent string) string {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	fastShell(t)
	configDir, err := GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(configDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, ConfigFileName), []byte(jsonContent), 0644))
	return configDir
}

func TestConversion_MigratesLegacyJSON(t *testing.T) {
	configDir := seedJSONHome(t, `{
		"default_program": "codex",
		"auto_yes": true,
		"daemon_poll_interval": 2500,
		"program_overrides": {"claude": "/opt/claude --dsp"}
	}`)
	infoBuf := captureLog(t, &aflog.InfoLog)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Settings are preserved through the conversion.
	assert.Equal(t, "codex", cfg.DefaultProgram)
	assert.True(t, cfg.AutoYes)
	assert.Equal(t, 2500, cfg.DaemonPollInterval)
	assert.Equal(t, "/opt/claude --dsp", cfg.ProgramOverrides["claude"])

	// config.toml is now canonical; config.json is moved aside to .bak.
	assert.FileExists(t, filepath.Join(configDir, TomlConfigFileName))
	assert.NoFileExists(t, filepath.Join(configDir, ConfigFileName))
	assert.FileExists(t, filepath.Join(configDir, ConfigFileName+".bak"))
	assert.Contains(t, infoBuf.String(), "migrated config to TOML")

	// The .bak still holds the original bytes, verbatim.
	bak, err := os.ReadFile(filepath.Join(configDir, ConfigFileName+".bak"))
	require.NoError(t, err)
	assert.Contains(t, string(bak), `"default_program": "codex"`)

	// Re-loading now reads config.toml canonically, with no duplicate-config
	// warning (config.json is gone).
	warnBuf := captureLog(t, &aflog.WarningLog)
	cfg2, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg2.DefaultProgram)
	assert.NotContains(t, warnBuf.String(), "both", "a clean conversion leaves no duplicate-config warning")
}

func TestConversion_InvalidJSONIsNotConvertedOrRenamed(t *testing.T) {
	// A config.json that fails to parse must NOT be converted and NOT renamed:
	// the user needs to fix it in place, and clobbering it would lose data.
	configDir := seedJSONHome(t, `{"default_program": "codex", oops}`)

	cfg, err := LoadConfig()
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "parse config file")

	assert.FileExists(t, filepath.Join(configDir, ConfigFileName), "invalid config.json must stay put")
	assert.NoFileExists(t, filepath.Join(configDir, TomlConfigFileName), "no config.toml on a failed conversion")
	assert.NoFileExists(t, filepath.Join(configDir, ConfigFileName+".bak"))
}

func TestConversion_EnumErrorIsNotConverted(t *testing.T) {
	// A config.json that parses but fails validation (legacy path-in-program)
	// is likewise left in place with an actionable error, never converted.
	configDir := seedJSONHome(t, `{"default_program": "/usr/bin/claude --flag"}`)

	cfg, err := LoadConfig()
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "program_overrides")

	assert.FileExists(t, filepath.Join(configDir, ConfigFileName))
	assert.NoFileExists(t, filepath.Join(configDir, TomlConfigFileName))
}

func TestConversion_WarnsOnUnknownJSONKeyBeforeDropping(t *testing.T) {
	// The frozen JSON reader must name a key it does not recognize, so a
	// setting added to an old config.json is not silently lost on conversion.
	seedJSONHome(t, `{"default_program": "codex", "some_future_key": "v"}`)
	warnBuf := captureLog(t, &aflog.WarningLog)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg.DefaultProgram)
	assert.Contains(t, warnBuf.String(), "some_future_key")
	assert.Contains(t, warnBuf.String(), "unknown key")
}

func TestConversion_CrashAfterWriteBeforeRenameLeavesTOMLCanonical(t *testing.T) {
	// Simulate a crash between the atomic config.toml write and the .bak
	// rename: both files remain. The current load still returns the converted
	// config, and the NEXT load treats config.toml as canonical (with the
	// duplicate-config warning) — no data is lost in either state.
	configDir := seedJSONHome(t, `{"default_program": "codex"}`)

	convertRenameFailForTest = func() error { return assert.AnError }
	warnBuf := captureLog(t, &aflog.WarningLog)
	cfg, err := LoadConfig()
	convertRenameFailForTest = nil
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg.DefaultProgram)

	// config.toml written; config.json still present (rename "failed").
	assert.FileExists(t, filepath.Join(configDir, TomlConfigFileName))
	assert.FileExists(t, filepath.Join(configDir, ConfigFileName))
	assert.Contains(t, warnBuf.String(), "could not move the original aside")

	// Next load: config.toml wins, config.json flagged as ignored.
	warn2 := captureLog(t, &aflog.WarningLog)
	cfg2, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg2.DefaultProgram)
	assert.Contains(t, warn2.String(), "both")
	assert.Contains(t, warn2.String(), "is canonical")
}

func TestConversion_DowngradeThenReupgradePreservesTOML(t *testing.T) {
	// After conversion the user downgrades to a pre-TOML af, which finds no
	// config.json and materializes a defaults config.json beside the user's
	// config.toml (the #837 loud-materialize path in the old binary). On the
	// next new-af run, config.toml must still win — the user's real settings
	// survive the round trip; only a "both files" warning results.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	fastShell(t)
	configDir, err := GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(configDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, TomlConfigFileName), []byte("default_program = 'codex'\n"), 0644))
	// The stand-in for what an old binary would regenerate: a defaults JSON.
	require.NoError(t, os.WriteFile(filepath.Join(configDir, ConfigFileName), []byte(`{"default_program": "claude"}`), 0644))

	warnBuf := captureLog(t, &aflog.WarningLog)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", cfg.DefaultProgram, "the user's config.toml must win over a downgrade-regenerated config.json")
	assert.Contains(t, warnBuf.String(), "both")
}

func TestConversion_LostRaceAdoptsWinnersTOML(t *testing.T) {
	// A concurrent process wins the conversion lock first: by the time this
	// process enters the lock body, config.toml already exists and config.json
	// has been renamed. This process must adopt the winner's config.toml and
	// NOT re-convert or re-rename.
	configDir := seedJSONHome(t, `{"default_program": "codex"}`)

	convertRaceHookForTest = func() {
		// The winner's effect: config.toml written, config.json moved to .bak.
		require.NoError(t, os.WriteFile(filepath.Join(configDir, TomlConfigFileName), []byte("default_program = 'aider'\n"), 0644))
		require.NoError(t, os.Rename(filepath.Join(configDir, ConfigFileName), filepath.Join(configDir, ConfigFileName+".bak")))
	}
	t.Cleanup(func() { convertRaceHookForTest = nil })

	cfg, err := LoadConfig()
	require.NoError(t, err)
	// The winner wrote aider, not our codex — we adopted its file.
	assert.Equal(t, "aider", cfg.DefaultProgram)
	assert.FileExists(t, filepath.Join(configDir, TomlConfigFileName))
	assert.NoFileExists(t, filepath.Join(configDir, ConfigFileName))
}

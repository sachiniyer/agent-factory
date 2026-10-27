package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file pins the legacy config.json half of the broken-symlink contract
// (#3660 review). refuseDanglingConfigLink was retrofitted only against the
// config.toml reads: a dangling ~/.agent-factory/config.json symlink reads as
// ENOENT, which the JSON branches treated as "no config yet" — so af started on
// defaults (LoadConfig) and `af config validate` / `af doctor` reported Missing
// (LoadConfigReadOnly), neither naming the link, diverging from the
// dangling-config.toml behavior the tomlPath guards already guaranteed. The
// guards added in config_load.go refuse the dangling config.json the same way;
// these tests pin that refusal and the non-regressions that keep the ordinary
// first-run and resolving-symlink paths ordinary.

// danglingJSONConfigHome stages an AF home whose config.json is a symlink to a
// file that does not exist anywhere — the dotfiles-repo shape with a moved or
// deleted target. It returns the link path (inside the home) and the missing
// target path (outside the home). It seeds NO config.toml, so the loader falls
// through to the legacy config.json branch.
func danglingJSONConfigHome(t *testing.T, state string) (link, missing string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)
	if state == "stateful" {
		// An initialized config dir (prior repos/) is the realistic upgrader:
		// the operator used af before, so they have a config.json legacy file
		// and state on disk. materializeDefaultConfig logs at ERROR for this
		// shape when the missing file is a regular absence; the guard must
		// refuse BEFORE that materialize path is reached.
		require.NoError(t, os.MkdirAll(filepath.Join(home, "repos"), 0o755))
	}
	dotfiles := t.TempDir()
	missing = filepath.Join(dotfiles, "gone.json")
	link = filepath.Join(home, ConfigFileName)
	require.NoError(t, os.Symlink(missing, link))
	return link, missing
}

// TestLoadConfig_RefusesDanglingLegacyJSONSymlink pins the startup leg: a
// dangling config.json symlink must NOT read as "no config yet" and materialize
// defaults. It must refuse, naming both ends, in both the stateless (first-home)
// and stateful (prior repos/, the realistic upgrader) shapes. Before the fix
// the stateful leg materialized config.toml of defaults and proceeded; the
// stateless leg proceeded silently.
func TestLoadConfig_RefusesDanglingLegacyJSONSymlink(t *testing.T) {
	for _, state := range []string{"stateless", "stateful"} {
		t.Run(state, func(t *testing.T) {
			link, missing := danglingJSONConfigHome(t, state)
			home := os.Getenv("AGENT_FACTORY_HOME")
			tomlPath := filepath.Join(home, TomlConfigFileName)

			cfg, err := LoadConfig()
			require.Error(t, err, "a broken config.json link must not read as 'no config yet'")
			require.Nil(t, cfg, "startup must not proceed on defaults over a broken link")
			assert.Contains(t, err.Error(), link, "the error names the link")
			assert.Contains(t, err.Error(), missing, "and the target it points at")

			// af stops and lets the operator decide: nothing is materialized at
			// either end, and the link the operator made is still a link.
			assert.NoFileExists(t, missing, "nothing was created at the far end")
			assert.NoFileExists(t, tomlPath, "af must not materialize config.toml over a broken config.json link")
			info, lerr := os.Lstat(link)
			require.NoError(t, lerr)
			assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
				"the broken link is left for the user to repair, not replaced")
		})
	}
}

// TestLoadConfigReadOnly_RefusesDanglingLegacyJSONSymlink pins the diagnostic
// leg — the cleanest half of the bug. LoadConfigReadOnly backs `af config
// validate` and `af doctor`; with a dangling config.json it returned
// Missing=true, err=nil, so validate exited 0 with "config OK: no config file
// yet" and doctor warned problem=false — while af itself refuses to start on
// that same link. The guard now refuses, so the diagnostic agrees with startup.
func TestLoadConfigReadOnly_RefusesDanglingLegacyJSONSymlink(t *testing.T) {
	for _, state := range []string{"stateless", "stateful"} {
		t.Run(state, func(t *testing.T) {
			link, missing := danglingJSONConfigHome(t, state)

			loaded, err := LoadConfigReadOnly()
			require.Error(t, err, "a broken config.json link must not read as 'no config file yet'")
			assert.False(t, loaded.Missing, "and must not be reported as Missing")
			assert.False(t, loaded.EmptyStub, "a broken link is not an empty stub")
			assert.Contains(t, err.Error(), link, "the error names the link")
			assert.Contains(t, err.Error(), missing, "and the target it points at")

			// Read-only: nothing is created at either end.
			assert.NoFileExists(t, missing)
			info, lerr := os.Lstat(link)
			require.NoError(t, lerr)
			assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink)
		})
	}
}

// --- Non-regressions: the guard must not turn ordinary paths into errors. ---

// TestLoadConfig_RegularMissingJSONStillMaterializesDefaults is the key
// non-regression. refuseDanglingConfigLink returns nil for a simply-absent
// file (an ordinary first run), so the guard added before os.ReadFile(configPath)
// must NOT turn "no config yet" into an error. Both files absent → first run →
// materialize config.toml of defaults, no error, exactly as before.
func TestLoadConfig_RegularMissingJSONStillMaterializesDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)

	cfg, err := LoadConfig()
	require.NoError(t, err, "a regular absent config.json is an ordinary first run, not a broken link")
	require.NotNil(t, cfg)
	assert.FileExists(t, filepath.Join(home, TomlConfigFileName),
		"first run still materializes config.toml of defaults")
}

// TestLoadConfigReadOnly_RegularMissingJSONStillReportsMissing is the
// diagnostic non-regression: a regular absent config.json (no symlink) must
// still report Missing=true, err=nil so `af config validate` / `af doctor`
// treat it as "no config file yet", not as a broken link.
func TestLoadConfigReadOnly_RegularMissingJSONStillReportsMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)

	loaded, err := LoadConfigReadOnly()
	require.NoError(t, err, "a regular absent config.json is not a broken link")
	assert.True(t, loaded.Missing, "a regular absence still reports Missing")
	assert.False(t, loaded.EmptyStub)
	_, err = os.Stat(filepath.Join(home, TomlConfigFileName))
	assert.True(t, os.IsNotExist(err), "read-only must not materialize config.toml")
}

// TestLoadConfig_ResolvingLegacyJSONSymlinkStillConverts pins that a
// resolving config.json symlink (a real dotfiles target) is NOT refused —
// refuseDanglingConfigLink returns nil for a link that resolves. The legacy
// config.json must still load and convert to config.toml as before.
func TestLoadConfig_ResolvingLegacyJSONSymlinkStillConverts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)
	dotfiles := t.TempDir()
	target := filepath.Join(dotfiles, "real-config.json")
	require.NoError(t, os.WriteFile(target, []byte(`{"default_program":"codex"}`), 0o644))
	link := filepath.Join(home, ConfigFileName)
	require.NoError(t, os.Symlink(target, link))

	cfg, err := LoadConfig()
	require.NoError(t, err, "a resolving config.json symlink must still convert, not be refused")
	require.NotNil(t, cfg)
	assert.Equal(t, "codex", cfg.DefaultProgram, "the symlinked config.json's settings are preserved through conversion")
	// Conversion wrote config.toml and moved the link target's file aside.
	assert.FileExists(t, filepath.Join(home, TomlConfigFileName))
}

// TestLoadConfigReadOnly_ResolvingLegacyJSONSymlinkReportsLegacyJSON is the
// diagnostic non-regression for a resolving config.json symlink: it must read
// as LegacyJSON (not Missing, not an error), exactly as a regular config.json
// would, so the guard did not regress the resolving-link path.
func TestLoadConfigReadOnly_ResolvingLegacyJSONSymlinkReportsLegacyJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)
	dotfiles := t.TempDir()
	target := filepath.Join(dotfiles, "real-config.json")
	require.NoError(t, os.WriteFile(target, []byte(`{"default_program":"codex"}`), 0o644))
	link := filepath.Join(home, ConfigFileName)
	require.NoError(t, os.Symlink(target, link))

	loaded, err := LoadConfigReadOnly()
	require.NoError(t, err, "a resolving config.json symlink must not be refused")
	assert.True(t, loaded.LegacyJSON, "it is read as a legacy config.json")
	assert.False(t, loaded.Missing)
	assert.False(t, loaded.EmptyStub)
	assert.Equal(t, link, loaded.Path)
	require.NotNil(t, loaded.Config)
	assert.Equal(t, "codex", loaded.Config.DefaultProgram)
	// Read-only: no conversion happened.
	assert.NoFileExists(t, filepath.Join(home, TomlConfigFileName))
}

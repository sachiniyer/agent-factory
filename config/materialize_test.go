package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aflog "github.com/sachiniyer/agent-factory/log"
)

// Regression tests for #837: the global config.json was silently replaced by
// materialized defaults. The materialize-on-missing branch must (a) stay
// silent on a genuine first run, (b) log loudly when the config dir visibly
// already carries state, and (c) never clobber a concurrently recreated
// config.json.

// fastShell keeps DefaultConfig's cached claude-alias probe off the
// interactive bash/zsh path so these tests do not pay for shell startup.
func fastShell(t *testing.T) {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")
}

func TestLoadConfig_MaterializeSilentOnFirstRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)
	errBuf := captureLog(t, &aflog.ErrorLog)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Empty(t, errBuf.String(), "first-run materialization must not log an error")
	assert.FileExists(t, filepath.Join(home, TomlConfigFileName), "first run must persist the defaults as config.toml")
	assert.NoFileExists(t, filepath.Join(home, ConfigFileName), "first run must not write config.json")
}

func TestLoadConfig_MaterializeLogsLoudlyOnInitializedDir(t *testing.T) {
	markers := []struct {
		name string
		seed func(t *testing.T, home string)
	}{
		{"instances dir", func(t *testing.T, home string) {
			require.NoError(t, os.MkdirAll(filepath.Join(home, "instances"), 0755))
		}},
		{"repos dir", func(t *testing.T, home string) {
			require.NoError(t, os.MkdirAll(filepath.Join(home, "repos"), 0755))
		}},
		{"daemon.pid", func(t *testing.T, home string) {
			require.NoError(t, os.WriteFile(filepath.Join(home, "daemon.pid"), []byte("12345"), 0600))
		}},
	}
	for _, m := range markers {
		t.Run(m.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			fastShell(t)
			require.NoError(t, os.MkdirAll(home, 0755))
			m.seed(t, home)
			errBuf := captureLog(t, &aflog.ErrorLog)

			cfg, err := LoadConfig()
			require.NoError(t, err)
			require.NotNil(t, cfg, "the app still needs a config — materialization proceeds")

			assert.Contains(t, errBuf.String(), "materializing defaults",
				"a missing config in an initialized dir must be a loud, diagnosable event")
			assert.Contains(t, errBuf.String(), "previous settings are lost")
			assert.FileExists(t, filepath.Join(home, TomlConfigFileName))
		})
	}
}

func TestLoadConfig_MaterializeLosesRaceToConcurrentWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)

	tomlPath := filepath.Join(home, TomlConfigFileName)
	concurrent := "default_program = 'codex'\ndaemon_poll_interval = 2500\n"
	materializeRaceHookForTest = func() {
		if err := os.WriteFile(tomlPath, []byte(concurrent), 0644); err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	}
	t.Cleanup(func() { materializeRaceHookForTest = nil })

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, "codex", cfg.DefaultProgram, "the concurrently written config must win")
	assert.Equal(t, 2500, cfg.DaemonPollInterval)

	data, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Equal(t, concurrent, string(data), "the concurrent file must not be clobbered by defaults")
}

func TestWriteConfigIfMissing_RemovesStubOnWriteFailure(t *testing.T) {
	// Regression for #864: when the O_EXCL create succeeds but the write fails,
	// writeConfigIfMissing must not leave a zero-byte config.json behind —
	// otherwise the next LoadConfig sees a present-but-empty file and hard-errors.
	home := t.TempDir()
	configPath := filepath.Join(home, ConfigFileName)

	writeConfigForceFailForTest = func() error {
		return assert.AnError
	}
	t.Cleanup(func() { writeConfigForceFailForTest = nil })

	created, err := writeConfigIfMissing(configPath, &Config{DefaultProgram: "claude"})
	require.Error(t, err, "a failed write must surface an error")
	assert.True(t, created, "the file was created (O_EXCL) before the write failed")
	assert.Contains(t, err.Error(), "failed to write config file")

	_, statErr := os.Stat(configPath)
	assert.True(t, os.IsNotExist(statErr), "the zero-byte stub must be removed so the next run can retry")
}

func TestLoadConfig_RecoversAfterFailedFirstRunWrite(t *testing.T) {
	// End-to-end #864: a failed first-run write leaves an empty config.toml;
	// the NEXT startup must recover, not wedge on the contentless-TOML hard
	// error. Since #4483 the recovery is in memory: a load can never tell a
	// crashed write apart from one still in flight, so the stub is left
	// untouched and defaults are returned — af starts, and the file heals on
	// the next config write.
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)
	tomlPath := filepath.Join(home, TomlConfigFileName)

	// Reproduce the bug state directly: O_EXCL created config.toml, then the
	// process died before its body landed, leaving a 0-byte stub.
	require.NoError(t, os.WriteFile(tomlPath, []byte(``), 0644))
	info, err := os.Stat(tomlPath)
	require.NoError(t, err)
	require.Equal(t, int64(0), info.Size(), "precondition: empty stub on disk")

	cfg, err := LoadConfig()
	require.NoError(t, err, "the empty stub must not wedge startup")
	require.NotNil(t, cfg)
	assert.Equal(t, defaultProgram, cfg.DefaultProgram)

	data, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Empty(t, data, "the load must not delete or rewrite the stub (#4483)")
}

func TestWriteConfigIfMissing_RefusesExistingFile(t *testing.T) {
	home := t.TempDir()
	tomlPath := filepath.Join(home, TomlConfigFileName)
	original := []byte("detach_keys = 'ctrl-]'\n")
	require.NoError(t, os.WriteFile(tomlPath, original, 0644))

	created, err := writeConfigIfMissing(tomlPath, &Config{DefaultProgram: "claude"})
	require.NoError(t, err)
	assert.False(t, created, "an existing config.toml must never be replaced")

	data, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Equal(t, original, data)
}

// TestLoadConfig_DoesNotUnlinkAnInFlightRewrite is the #4483 reproduction: a
// writer that rewrites config.toml IN PLACE (shell `>`, an in-place editor)
// truncates first, so a LoadConfig inside that window sees a contentless
// regular unshadowed file — a shape identical to a failed first-run stub.
// The load must not touch the file: removing it detaches the writer's
// descriptor, the writer's content lands in an unlinked inode, and the
// re-materialized defaults win — the user's entire config silently replaced.
// A read must never delete user data.
func TestLoadConfig_DoesNotUnlinkAnInFlightRewrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)
	tomlPath := filepath.Join(home, TomlConfigFileName)

	require.NoError(t, os.WriteFile(tomlPath,
		[]byte("schema_version = 1\ndefault_program = 'aider'\nauto_update = true\n"), 0o644))

	// The in-place writer: open + truncate, new content not yet written.
	writer, err := os.OpenFile(tomlPath, os.O_WRONLY|os.O_TRUNC, 0o644)
	require.NoError(t, err)

	// The load lands in the window. It may see the file empty, but it must
	// not delete, rewrite, or otherwise mutate it.
	cfg, err := LoadConfig()
	require.NoError(t, err, "a load during an in-place rewrite must not fail")
	require.NotNil(t, cfg)

	// The writer finishes through ITS descriptor.
	_, werr := writer.WriteString("schema_version = 1\ndefault_program = 'codex'\nauto_update = true\n")
	require.NoError(t, werr)
	require.NoError(t, writer.Close())

	// The path must hold the WRITER's content — not the defaults af would
	// have installed in place of the file it unlinked.
	data, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "codex",
		"the writer's content must land at config.toml — losing it is the #4483 data loss")
	assert.NotContains(t, string(data), "'claude'",
		"af's defaults must not replace the user's in-flight rewrite")
}

// The same in-place-rewrite window exists for a legacy config.json on a host
// that never materialized config.toml: a zero-byte config.json is the same
// failed-write fingerprint, and unlinking it detaches the writer's descriptor
// exactly the same way.
func TestLoadConfig_DoesNotUnlinkAnInFlightJSONRewrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)
	jsonPath := filepath.Join(home, ConfigFileName)

	require.NoError(t, os.WriteFile(jsonPath,
		[]byte(`{"schema_version":1,"default_program":"aider"}`), 0o644))

	writer, err := os.OpenFile(jsonPath, os.O_WRONLY|os.O_TRUNC, 0o644)
	require.NoError(t, err)

	cfg, err := LoadConfig()
	require.NoError(t, err, "a load during an in-place rewrite must not fail")
	require.NotNil(t, cfg)

	_, werr := writer.WriteString(`{"schema_version":1,"default_program":"codex"}`)
	require.NoError(t, werr)
	require.NoError(t, writer.Close())

	data, err := os.ReadFile(jsonPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"schema_version":1,"default_program":"codex"}`, string(data),
		"the writer's content must land at config.json — losing it is the #4483 data loss")
}

// A zero-byte config.json satisfies UnsetGlobalConfigValue's pre-lock
// LoadConfig with in-memory defaults while config.toml stays absent (#4483
// review). The locked body must answer "not set" from that empty document —
// not ENOENT, and not a defaults file materialized just to say so.
func TestUnsetGlobalConfigValue_EmptyJSONStubAnswersNotSet(t *testing.T) {
	fastShell(t)
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, ConfigFileName), nil, 0o644))
	t.Setenv("AGENT_FACTORY_HOME", home)

	result, err := UnsetGlobalConfigValue("ssh.host_key_verification")
	require.NoError(t, err, "unset on an accepted stub must not fail")
	require.NotNil(t, result)
	assert.False(t, result.Removed, "an empty document holds no key to remove")
	assert.Equal(t, filepath.Join(home, TomlConfigFileName), result.Path)
	_, statErr := os.Stat(filepath.Join(home, TomlConfigFileName))
	assert.True(t, os.IsNotExist(statErr),
		"a no-op unset must not materialize config.toml — a mid-flight rewrite may be holding the name open")
}

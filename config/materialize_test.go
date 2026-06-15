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

// fastShell keeps DefaultConfig's claude-alias probe off the interactive
// bash/zsh path so these tests don't pay seconds per LoadConfig call.
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

	// Don't assert an empty buffer: DefaultConfig() logs an unrelated ERROR
	// ("failed to get claude command") on machines without claude on PATH
	// (e.g. CI). First-run only has to skip the settings-loss warning.
	assert.NotContains(t, errBuf.String(), "config.json missing from an initialized config dir",
		"first-run materialization must not log the settings-loss error")
	assert.FileExists(t, filepath.Join(home, ConfigFileName), "first run must persist the defaults")
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
				"a missing config.json in an initialized dir must be a loud, diagnosable event")
			assert.Contains(t, errBuf.String(), "previous settings are lost")
			assert.FileExists(t, filepath.Join(home, ConfigFileName))
		})
	}
}

func TestLoadConfig_MaterializeLosesRaceToConcurrentWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)

	configPath := filepath.Join(home, ConfigFileName)
	concurrent := `{"default_program": "codex", "daemon_poll_interval": 2500}`
	materializeRaceHookForTest = func() {
		if err := os.WriteFile(configPath, []byte(concurrent), 0644); err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	}
	t.Cleanup(func() { materializeRaceHookForTest = nil })

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, "codex", cfg.DefaultProgram, "the concurrently written config must win")
	assert.Equal(t, 2500, cfg.DaemonPollInterval)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.JSONEq(t, concurrent, string(data), "the concurrent file must not be clobbered by defaults")
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
	// End-to-end #864: a failed first-run write leaves an empty config.json;
	// the NEXT startup must recover (re-materialize defaults), not wedge.
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)
	configPath := filepath.Join(home, ConfigFileName)

	// Reproduce the bug state directly: O_EXCL created the file, then the write
	// failed before defense-in-depth cleanup existed, leaving a 0-byte stub.
	require.NoError(t, os.WriteFile(configPath, []byte(``), 0644))
	info, err := os.Stat(configPath)
	require.NoError(t, err)
	require.Equal(t, int64(0), info.Size(), "precondition: empty stub on disk")

	cfg, err := LoadConfig()
	require.NoError(t, err, "the empty stub must not wedge startup")
	require.NotNil(t, cfg)
	assert.Equal(t, defaultProgram, cfg.DefaultProgram)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.NotEmpty(t, data, "defaults must be re-materialized to a non-empty file")
}

func TestWriteConfigIfMissing_RefusesExistingFile(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ConfigFileName)
	original := []byte(`{"detach_keys": "ctrl-]"}`)
	require.NoError(t, os.WriteFile(configPath, original, 0644))

	created, err := writeConfigIfMissing(configPath, &Config{DefaultProgram: "claude"})
	require.NoError(t, err)
	assert.False(t, created, "an existing config.json must never be replaced")

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, original, data)
}

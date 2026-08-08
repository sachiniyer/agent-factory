package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeDurationTOML(t *testing.T, content string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	configDir, err := GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(configDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, TomlConfigFileName), []byte(content), 0644))
}

func TestLoadDaemonPollIntervalTOMLForms(t *testing.T) {
	t.Run("keeps legacy integer milliseconds", func(t *testing.T) {
		writeDurationTOML(t, `daemon_poll_interval = 1500`+"\n")

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, 1500, cfg.DaemonPollInterval)
	})

	t.Run("accepts duration strings", func(t *testing.T) {
		cases := []struct {
			value        string
			milliseconds int
		}{
			{value: "1500ms", milliseconds: 1500},
			{value: "30m", milliseconds: 30 * 60 * 1000},
		}
		for _, tc := range cases {
			t.Run(tc.value, func(t *testing.T) {
				writeDurationTOML(t, fmt.Sprintf("daemon_poll_interval = %q\n", tc.value))

				cfg, err := LoadConfig()
				require.NoError(t, err)
				require.NotNil(t, cfg)
				assert.Equal(t, tc.milliseconds, cfg.DaemonPollInterval)
			})
		}
	})

	t.Run("rejects duration that loses millisecond precision", func(t *testing.T) {
		writeDurationTOML(t, `daemon_poll_interval = "500us"`+"\n")

		cfg, err := LoadConfig()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "whole milliseconds")
	})
}

func TestLoadDaemonPollIntervalDurationFromLegacyJSON(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	configDir, err := GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(configDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, ConfigFileName),
		[]byte(`{"daemon_poll_interval":"30m"}`), 0644))

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, 30*60*1000, cfg.DaemonPollInterval)
	tomlData, err := os.ReadFile(filepath.Join(configDir, TomlConfigFileName))
	require.NoError(t, err)
	assert.Contains(t, string(tomlData), "daemon_poll_interval = '1800000ms'")
}

func TestMaterializedConfigUsesDurationString(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, defaultDaemonPollInterval, cfg.DaemonPollInterval)
	data, err := os.ReadFile(filepath.Join(home, TomlConfigFileName))
	require.NoError(t, err)
	assert.Contains(t, string(data), "daemon_poll_interval = '1000ms'")
}

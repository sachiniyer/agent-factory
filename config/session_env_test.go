package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSessionEnvironmentConfig(t *testing.T, content string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	configDir, err := GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, TomlConfigFileName), []byte(content), 0o644))
}

func TestLoadConfigSessionEnvironmentPassthrough(t *testing.T) {
	t.Run("normalizes exact names", func(t *testing.T) {
		writeSessionEnvironmentConfig(t, `session_env_passthrough = ["CUSTOM_PROVIDER_TOKEN", "SECOND_TOKEN", "CUSTOM_PROVIDER_TOKEN"]`)
		cfg, err := LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, []string{"CUSTOM_PROVIDER_TOKEN", "SECOND_TOKEN"}, cfg.SessionEnvPassthrough)
	})

	t.Run("rejects patterns", func(t *testing.T) {
		writeSessionEnvironmentConfig(t, `session_env_passthrough = ["PROVIDER_*"]`)
		cfg, err := LoadConfig()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "session_env_passthrough")
		assert.Contains(t, err.Error(), "exact POSIX name")
	})
}

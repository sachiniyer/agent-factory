package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/require"
)

func TestRemovedAutoYesConfigLoadsForUpgradeAndWarns(t *testing.T) {
	tests := []struct {
		name  string
		parse func() (*Config, error)
	}{
		{
			name: "global TOML",
			parse: func() (*Config, error) {
				return parseConfigTOML([]byte("auto_yes = true\n"), "config.toml")
			},
		},
		{
			name: "global JSON",
			parse: func() (*Config, error) {
				return parseConfig([]byte(`{"auto_yes":true}`), "config.json")
			},
		},
		{
			name: "root-agent TOML",
			parse: func() (*Config, error) {
				return parseConfigTOML([]byte("[root_agents.\"/repo\"]\nauto_yes = false\n"), "config.toml")
			},
		},
		{
			name: "root-agent JSON",
			parse: func() (*Config, error) {
				return parseConfig([]byte(`{"root_agents":{"/repo":{"auto_yes":false}}}`), "config.json")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			warnings := captureLog(t, &aflog.WarningLog)
			cfg, err := tc.parse()
			require.NoError(t, err, "an existing config must not make an af upgrade fail")
			require.NotNil(t, cfg)
			require.Contains(t, warnings.String(), "auto_yes was removed")
			require.Contains(t, warnings.String(), "ignored")
			require.Contains(t, warnings.String(), "program_overrides")
			require.Equal(t, 1, strings.Count(warnings.String(), "auto_yes was removed"))
			require.NotContains(t, warnings.String(), "unknown key")
		})
	}
}

func TestRemovedAutoYesInRepoConfigLoadsForUpgradeAndWarns(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, TomlConfigFileName), []byte("auto_yes = true\n"), 0o600))

	warnings := captureLog(t, &aflog.WarningLog)
	cfg, _, err := LoadInRepoConfig(repo)
	require.NoError(t, err, "an existing in-repo config must not make an af upgrade fail")
	require.NotNil(t, cfg)
	require.Contains(t, warnings.String(), "auto_yes was removed")
	require.Contains(t, warnings.String(), "ignored")
	require.Contains(t, warnings.String(), "program_overrides")
	require.Equal(t, 1, strings.Count(warnings.String(), "auto_yes was removed"))
	require.NotContains(t, warnings.String(), "unknown key")
}

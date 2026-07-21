package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemovedAutoYesConfigGetsMigrationGuidance(t *testing.T) {
	tests := []struct {
		name  string
		parse func() error
	}{
		{
			name: "global TOML",
			parse: func() error {
				_, err := parseConfigTOML([]byte("auto_yes = true\n"), "config.toml")
				return err
			},
		},
		{
			name: "global JSON",
			parse: func() error {
				_, err := parseConfig([]byte(`{"auto_yes":true}`), "config.json")
				return err
			},
		},
		{
			name: "root-agent TOML",
			parse: func() error {
				_, err := parseConfigTOML([]byte("[root_agents.\"/repo\"]\nauto_yes = false\n"), "config.toml")
				return err
			},
		},
		{
			name: "root-agent JSON",
			parse: func() error {
				_, err := parseConfig([]byte(`{"root_agents":{"/repo":{"auto_yes":false}}}`), "config.json")
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.parse()
			require.Error(t, err)
			require.Contains(t, err.Error(), "auto_yes was removed")
			require.Contains(t, err.Error(), "program_overrides")
		})
	}
}

func TestRemovedAutoYesInRepoConfigGetsMigrationGuidance(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, TomlConfigFileName), []byte("auto_yes = true\n"), 0o600))

	_, _, err := LoadInRepoConfig(repo)
	require.Error(t, err)
	require.Contains(t, err.Error(), "auto_yes was removed")
	require.Contains(t, err.Error(), "program_overrides")
}

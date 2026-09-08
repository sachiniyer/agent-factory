package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInRepoGlobalOnlyRemedyCanonicalFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName), []byte("schema_version = 1\n"), 0644))
	repo := t.TempDir()
	writeInRepoTomlConfig(t, repo, "vscode_server_binary = '/opt/code-server'\n")
	_, _, err := LoadInRepoConfig(repo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Join(home, TomlConfigFileName))
	assert.NotContains(t, err.Error(), filepath.Join(home, ConfigFileName))
}

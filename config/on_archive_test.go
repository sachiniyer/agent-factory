package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveConfigOnArchiveCommandGlobalAndPersonal(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "on_archive_command = \"global-prune\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "global-prune", resolvedValueString(t, resolved, "on_archive_command"))

	writePersonalConfig(t, project.ID, "on_archive_command = \"project-prune\"\n")
	resolved, err = ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "project-prune", resolvedValueString(t, resolved, "on_archive_command"))

	value := requireResolvedValue(t, resolved, "on_archive_command")
	require.NotNil(t, value.Winner)
	assert.Equal(t, SourceProjectPersonal.String(), value.Winner.Layer)
}

func TestLoadInRepoConfigRejectsOnArchiveCommandAsOperatorOnly(t *testing.T) {
	_, repoRoot, _ := registeredTestProject(t)
	writeInRepoTomlConfig(t, repoRoot, "on_archive_command = \"touch /tmp/repo-code-ran\"\n")

	_, _, err := LoadInRepoConfig(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"on_archive_command"`)
	assert.Contains(t, err.Error(), "global setting")
	assert.Contains(t, err.Error(), "cannot be set per-repo")
}

func resolvedValueString(t *testing.T, resolved *ResolvedConfig, key string) string {
	t.Helper()
	value := requireResolvedValue(t, resolved, key)
	got, ok := value.Value.(string)
	require.True(t, ok, "resolved %s value has type %T, want string", key, value.Value)
	return got
}

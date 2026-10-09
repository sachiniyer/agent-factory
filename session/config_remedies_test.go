package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackendConfigErrorResolvedConfigFile(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	for _, kind := range []BackendKind{BackendDocker, BackendSSH, BackendHook} {
		t.Run(string(kind), func(t *testing.T) {
			for _, name := range []string{config.TomlConfigFileName, config.ConfigFileName} {
				t.Run(name, func(t *testing.T) {
					repo := t.TempDir()
					dir := filepath.Join(repo, config.InRepoConfigDirName)
					require.NoError(t, os.MkdirAll(dir, 0755))
					content := "backend = \"local\"\n"
					other := config.ConfigFileName
					if name == config.ConfigFileName {
						content = "{}"
						other = config.TomlConfigFileName
					}
					path := filepath.Join(dir, name)
					require.NoError(t, os.WriteFile(path, []byte(content), 0644))
					cfg, err := config.ResolveConfig(repo)
					require.NoError(t, err)
					err = BackendConfigError(kind, cfg)
					require.Error(t, err)
					assert.Contains(t, err.Error(), filepath.Join(config.InRepoConfigDirName, name))
					assert.NotContains(t, err.Error(), filepath.Join(config.InRepoConfigDirName, other))
					// The remedy must retain the loaded source even if the directory changes.
					require.NoError(t, os.Rename(path, filepath.Join(dir, other)))
					assert.EqualError(t, BackendConfigError(kind, cfg), err.Error())
				})
			}
		})
	}
}

func TestSandboxConfigErrorResolvedConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	err := BackendConfigError(BackendSandbox, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Join(dir, config.TomlConfigFileName))
	assert.NotContains(t, err.Error(), "~/.agent-factory")
}

func TestReservedTitleRefusalForResolvedRepo(t *testing.T) {
	const repo = "/tmp/B's repo"
	for _, title := range []string{"root", "ro ot"} {
		err := ReservedTitleRefusalFor(title, repo)
		require.Error(t, err)
		quoted := config.ShellQuotePath(repo)
		assert.Contains(t, err.Error(), "af projects add "+quoted)
		assert.Contains(t, err.Error(), "af config set --project "+quoted)
		assert.NotContains(t, err.Error(), "from this repo")
	}
	assert.NoError(t, ReservedTitleRefusalFor("ordinary", repo))
}

package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReservedTitleRefusalResolvedConfigFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range []string{filepath.Join(home, "relocated"), t.TempDir()} {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", dir)
			want := filepath.Join(dir, config.TomlConfigFileName)
			if dir == filepath.Join(home, "relocated") {
				want = "~/relocated/" + config.TomlConfigFileName
			}
			for _, title := range []string{"root", "ro ot"} {
				err := ReservedTitleRefusal(title)
				require.Error(t, err)
				assert.Contains(t, err.Error(), want)
				assert.Contains(t, err.Error(), "[root_agent]")
				assert.NotContains(t, err.Error(), "config.json")
				assert.NotContains(t, err.Error(), "root_agents")
			}
		})
	}
}

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

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepoConfigRemoteHooks(t *testing.T) {
	t.Run("save and load with remote hooks", func(t *testing.T) {
		tempHome := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", tempHome)

		repoID := "test-repo-id"
		cfg := &RepoConfig{
			RemoteHooks: &RemoteHooks{
				LaunchCmd: "/usr/local/bin/launch.sh",
				ListCmd:   "/usr/local/bin/list.sh",
				AttachCmd: "/usr/local/bin/attach.sh",
				DeleteCmd: "/usr/local/bin/delete.sh",
			},
		}

		err := SaveRepoConfig(repoID, cfg)
		require.NoError(t, err)

		loaded, err := LoadRepoConfig(repoID)
		require.NoError(t, err)
		require.NotNil(t, loaded.RemoteHooks)
		assert.Equal(t, "/usr/local/bin/launch.sh", loaded.RemoteHooks.LaunchCmd)
		assert.Equal(t, "/usr/local/bin/list.sh", loaded.RemoteHooks.ListCmd)
		assert.Equal(t, "/usr/local/bin/attach.sh", loaded.RemoteHooks.AttachCmd)
		assert.Equal(t, "/usr/local/bin/delete.sh", loaded.RemoteHooks.DeleteCmd)
	})

	t.Run("save and load without remote hooks", func(t *testing.T) {
		tempHome := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", tempHome)

		repoID := "test-repo-no-hooks"
		cfg := &RepoConfig{
			PostWorktreeCommands: []string{"npm install"},
		}

		err := SaveRepoConfig(repoID, cfg)
		require.NoError(t, err)

		loaded, err := LoadRepoConfig(repoID)
		require.NoError(t, err)
		assert.Nil(t, loaded.RemoteHooks)
		assert.Equal(t, []string{"npm install"}, loaded.PostWorktreeCommands)
	})

	t.Run("load nonexistent returns empty config", func(t *testing.T) {
		tempHome := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", tempHome)

		loaded, err := LoadRepoConfig("nonexistent")
		require.NoError(t, err)
		assert.Nil(t, loaded.RemoteHooks)
		assert.Nil(t, loaded.PostWorktreeCommands)
	})

	t.Run("load returns error when config dir cannot be resolved", func(t *testing.T) {
		// Use a ~ prefix with no HOME set so GetConfigDir fails.
		t.Setenv("AGENT_FACTORY_HOME", "~/broken")
		t.Setenv("HOME", "")

		loaded, err := LoadRepoConfig("any-repo")
		assert.Error(t, err)
		assert.Nil(t, loaded)
		assert.Contains(t, err.Error(), "failed to get config dir")
	})

	t.Run("both remote hooks and post worktree commands", func(t *testing.T) {
		tempHome := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", tempHome)

		repoID := "test-repo-both"
		cfg := &RepoConfig{
			PostWorktreeCommands: []string{"npm install", "make build"},
			RemoteHooks: &RemoteHooks{
				LaunchCmd: "/bin/launch",
				ListCmd:   "/bin/list",
				AttachCmd: "/bin/attach",
				DeleteCmd: "/bin/delete",
			},
		}

		err := SaveRepoConfig(repoID, cfg)
		require.NoError(t, err)

		loaded, err := LoadRepoConfig(repoID)
		require.NoError(t, err)
		require.NotNil(t, loaded.RemoteHooks)
		assert.Equal(t, "/bin/launch", loaded.RemoteHooks.LaunchCmd)
		assert.Equal(t, 2, len(loaded.PostWorktreeCommands))
	})
}

func TestRemoteHooksJSON(t *testing.T) {
	t.Run("marshals correctly", func(t *testing.T) {
		hooks := RemoteHooks{
			LaunchCmd: "/path/to/launch.sh",
			ListCmd:   "/path/to/list.sh",
			AttachCmd: "/path/to/attach.sh",
			DeleteCmd: "/path/to/delete.sh",
		}

		data, err := json.Marshal(hooks)
		require.NoError(t, err)

		var parsed map[string]string
		err = json.Unmarshal(data, &parsed)
		require.NoError(t, err)

		assert.Equal(t, "/path/to/launch.sh", parsed["launch_cmd"])
		assert.Equal(t, "/path/to/list.sh", parsed["list_cmd"])
		assert.Equal(t, "/path/to/attach.sh", parsed["attach_cmd"])
		assert.Equal(t, "/path/to/delete.sh", parsed["delete_cmd"])
	})

	t.Run("unmarshals correctly", func(t *testing.T) {
		jsonStr := `{"launch_cmd":"/a","list_cmd":"/b","attach_cmd":"/c","delete_cmd":"/d"}`
		var hooks RemoteHooks
		err := json.Unmarshal([]byte(jsonStr), &hooks)
		require.NoError(t, err)
		assert.Equal(t, "/a", hooks.LaunchCmd)
		assert.Equal(t, "/b", hooks.ListCmd)
		assert.Equal(t, "/c", hooks.AttachCmd)
		assert.Equal(t, "/d", hooks.DeleteCmd)
	})

	t.Run("omitted when nil in RepoConfig", func(t *testing.T) {
		cfg := RepoConfig{
			PostWorktreeCommands: []string{"test"},
		}
		data, err := json.Marshal(cfg)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "remote_hooks")
	})

	t.Run("config file round-trip", func(t *testing.T) {
		tempHome := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", tempHome)

		repoID := "json-roundtrip"
		cfg := &RepoConfig{
			RemoteHooks: &RemoteHooks{
				LaunchCmd: "/x/launch",
				ListCmd:   "/x/list",
				AttachCmd: "/x/attach",
				DeleteCmd: "/x/delete",
			},
		}

		err := SaveRepoConfig(repoID, cfg)
		require.NoError(t, err)

		// Read raw file to verify JSON structure
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		path := filepath.Join(configDir, "repos", repoID, RepoConfigFileName)
		raw, err := os.ReadFile(path)
		require.NoError(t, err)

		assert.Contains(t, string(raw), `"remote_hooks"`)
		assert.Contains(t, string(raw), `"launch_cmd"`)
		assert.Contains(t, string(raw), `"/x/launch"`)
	})
}

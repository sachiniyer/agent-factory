package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
				DeleteCmd: "/usr/local/bin/delete.sh",
			},
		}

		err := SaveRepoConfig(repoID, cfg)
		require.NoError(t, err)

		loaded, err := LoadRepoConfig(repoID)
		require.NoError(t, err)
		require.NotNil(t, loaded.RemoteHooks)
		assert.Equal(t, "/usr/local/bin/launch.sh", loaded.RemoteHooks.LaunchCmd)
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
		// Post-PR7 the only working keys are launch_cmd and delete_cmd.
		hooks := RemoteHooks{
			LaunchCmd: "/path/to/launch.sh",
			DeleteCmd: "/path/to/delete.sh",
		}

		data, err := json.Marshal(hooks)
		require.NoError(t, err)

		var parsed map[string]string
		err = json.Unmarshal(data, &parsed)
		require.NoError(t, err)

		assert.Equal(t, "/path/to/launch.sh", parsed["launch_cmd"])
		assert.Equal(t, "/path/to/delete.sh", parsed["delete_cmd"])
		// The removed tripwire keys are omitempty, so a clean config never
		// serializes them.
		assert.NotContains(t, string(data), "list_cmd")
		assert.NotContains(t, string(data), "attach_cmd")
		assert.NotContains(t, string(data), "terminal_cmd")
	})

	t.Run("unmarshals correctly", func(t *testing.T) {
		jsonStr := `{"launch_cmd":"/a","delete_cmd":"/d"}`
		var hooks RemoteHooks
		err := json.Unmarshal([]byte(jsonStr), &hooks)
		require.NoError(t, err)
		assert.Equal(t, "/a", hooks.LaunchCmd)
		assert.Equal(t, "/d", hooks.DeleteCmd)
	})

	t.Run("stale pre-PR7 keys unmarshal into tripwire fields", func(t *testing.T) {
		// A config written before the provision-and-expose migration still
		// carries list_cmd/attach_cmd/terminal_cmd; they decode into the
		// Removed* tripwire fields so Validate can reject them with a
		// migration message rather than silently dropping them.
		jsonStr := `{"launch_cmd":"/a","delete_cmd":"/d","list_cmd":"/b","attach_cmd":"/c","terminal_cmd":"/e"}`
		var hooks RemoteHooks
		err := json.Unmarshal([]byte(jsonStr), &hooks)
		require.NoError(t, err)
		assert.Equal(t, "/b", hooks.RemovedListCmd)
		assert.Equal(t, "/c", hooks.RemovedAttachCmd)
		assert.Equal(t, "/e", hooks.RemovedTerminalCmd)
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

// TestSaveRepoConfigAtomicWrite verifies SaveRepoConfig uses AtomicWriteFile:
// a failed write must leave the prior on-disk content intact (crash-mid-write
// safety), and a successful write must not leave temp-file droppings behind.
func TestSaveRepoConfigAtomicWrite(t *testing.T) {
	t.Run("failed write preserves prior content", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("chmod-based write barrier is bypassed when running as root")
		}
		tempHome := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", tempHome)

		repoID := "preserve-on-failure"
		initial := &RepoConfig{PostWorktreeCommands: []string{"echo initial"}}
		require.NoError(t, SaveRepoConfig(repoID, initial))

		dir, path, err := repoConfigPath(repoID)
		require.NoError(t, err)
		priorBytes, err := os.ReadFile(path)
		require.NoError(t, err)

		// Strip write permission from the repo dir so AtomicWriteFile cannot
		// create its temp file. A non-atomic implementation that truncated
		// the destination before writing would clobber the prior content;
		// AtomicWriteFile must leave it untouched.
		require.NoError(t, os.Chmod(dir, 0o555))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

		err = SaveRepoConfig(repoID, &RepoConfig{
			PostWorktreeCommands: []string{"echo replacement"},
		})
		require.Error(t, err, "save into read-only dir must fail")

		after, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, priorBytes, after, "prior content must survive failed write")
	})

	t.Run("successful write leaves no tmp files", func(t *testing.T) {
		tempHome := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", tempHome)

		repoID := "no-tmp-droppings"
		cfg := &RepoConfig{PostWorktreeCommands: []string{"echo hi"}}
		require.NoError(t, SaveRepoConfig(repoID, cfg))
		require.NoError(t, SaveRepoConfig(repoID, cfg))

		dir, _, err := repoConfigPath(repoID)
		require.NoError(t, err)
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			assert.False(t, strings.Contains(e.Name(), ".tmp."),
				"leftover tmp file in repo config dir: %s", e.Name())
		}
	})
}

// TestRemoteHooksValidate covers the post-PR7 provision-and-expose contract:
// launch_cmd and delete_cmd are both required (the #738 fail-fast guard against
// exec.Command's cryptic "exec: no command"), and any of the removed pre-PR7
// keys (list_cmd/attach_cmd/terminal_cmd) must be rejected with an actionable
// migration error rather than silently ignored.
func TestRemoteHooksValidate(t *testing.T) {
	full := func() RemoteHooks {
		return RemoteHooks{
			LaunchCmd: "/bin/launch",
			DeleteCmd: "/bin/delete",
		}
	}

	t.Run("launch_cmd + delete_cmd is valid", func(t *testing.T) {
		assert.NoError(t, full().Validate())
	})

	cases := []struct {
		name    string
		mutate  func(*RemoteHooks)
		wantMsg string
	}{
		{"empty launch_cmd", func(h *RemoteHooks) { h.LaunchCmd = "" }, "remote_hooks.launch_cmd is required"},
		{"whitespace launch_cmd", func(h *RemoteHooks) { h.LaunchCmd = "   " }, "remote_hooks.launch_cmd is required"},
		{"empty delete_cmd", func(h *RemoteHooks) { h.DeleteCmd = "" }, "remote_hooks.delete_cmd is required"},
		{"whitespace delete_cmd", func(h *RemoteHooks) { h.DeleteCmd = "   " }, "remote_hooks.delete_cmd is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := full()
			tc.mutate(&h)
			err := h.Validate()
			require.Error(t, err)
			assert.EqualError(t, err, tc.wantMsg)
		})
	}
}

// TestRemoteHooksValidateMigration verifies the tripwire guard: a stale config
// still carrying any pre-PR7 key (list_cmd/attach_cmd/terminal_cmd) fails
// Validate with the provision-and-expose migration message naming the offending
// key, even when launch_cmd/delete_cmd are otherwise valid.
func TestRemoteHooksValidateMigration(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*RemoteHooks)
		wantKey string
	}{
		{"list_cmd set", func(h *RemoteHooks) { h.RemovedListCmd = "/bin/list" }, "remote_hooks.list_cmd"},
		{"attach_cmd set", func(h *RemoteHooks) { h.RemovedAttachCmd = "/bin/attach" }, "remote_hooks.attach_cmd"},
		{"terminal_cmd set", func(h *RemoteHooks) { h.RemovedTerminalCmd = "/bin/terminal" }, "remote_hooks.terminal_cmd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := RemoteHooks{LaunchCmd: "/bin/launch", DeleteCmd: "/bin/delete"}
			tc.mutate(&h)
			err := h.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantKey)
			assert.Contains(t, err.Error(), "was removed in the provision-and-expose migration")
		})
	}
}

func TestResolveHookCommandPath(t *testing.T) {
	const root = "/srv/repos/detail"
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"absolute unchanged", "/usr/local/bin/launch.sh", "/usr/local/bin/launch.sh"},
		{"dot-slash relative resolved", "./.agent-factory/hooks/coder-launch.sh", root + "/.agent-factory/hooks/coder-launch.sh"},
		{"bare relative resolved", "infra/launch.sh", root + "/infra/launch.sh"},
		{"parent-relative resolved and cleaned", "../shared/launch.sh", "/srv/repos/shared/launch.sh"},
		{"bare name keeps $PATH lookup", "coder-launch.sh", "coder-launch.sh"},
		{"plain command keeps $PATH lookup", "bash", "bash"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveHookCommandPath(root, tc.cmd))
		})
	}
}

// TestResolveHookCommandPathWhitespace covers leading/trailing whitespace
// around the command token: it is trimmed before the IsAbs/separator decision
// and the join, so absolute paths stay absolute, relative paths still resolve
// under repoRoot, and bare names keep their $PATH lookup (#933).
func TestResolveHookCommandPathWhitespace(t *testing.T) {
	const root = "/srv/repos/detail"
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"leading whitespace on absolute path", "   /usr/local/bin/launch.sh", "/usr/local/bin/launch.sh"},
		{"trailing whitespace on absolute path", "/usr/local/bin/launch.sh   ", "/usr/local/bin/launch.sh"},
		{"leading whitespace on relative path", "   ./hooks/launch.sh", root + "/hooks/launch.sh"},
		{"trailing whitespace on relative path", "./hooks/launch.sh   ", root + "/hooks/launch.sh"},
		{"leading whitespace on bare name", "   bash", "bash"},
		{"trailing whitespace on bare name", "bash   ", "bash"},
		{"whitespace-only stays empty", "   ", ""},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveHookCommandPath(root, tc.cmd))
		})
	}
}

// TestRemoteHooksResolveCommandPaths verifies the copy semantics: the
// returned hooks carry resolved paths while the receiver is untouched, so
// ResolveConfig can never write rewritten values back through a loaded
// config struct.
func TestRemoteHooksResolveCommandPaths(t *testing.T) {
	orig := RemoteHooks{
		LaunchCmd: "./hooks/launch.sh",
		DeleteCmd: "hooks/delete.sh",
	}
	resolved := orig.resolveCommandPaths("/repo")

	assert.Equal(t, "/repo/hooks/launch.sh", resolved.LaunchCmd)
	assert.Equal(t, "/repo/hooks/delete.sh", resolved.DeleteCmd)

	assert.Equal(t, "./hooks/launch.sh", orig.LaunchCmd, "receiver must not be mutated")
	assert.Equal(t, "hooks/delete.sh", orig.DeleteCmd, "receiver must not be mutated")
}

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
			LaunchCmd:   "/path/to/launch.sh",
			ListCmd:     "/path/to/list.sh",
			AttachCmd:   "/path/to/attach.sh",
			DeleteCmd:   "/path/to/delete.sh",
			TerminalCmd: "/path/to/terminal.sh",
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
		assert.Equal(t, "/path/to/terminal.sh", parsed["terminal_cmd"])
	})

	t.Run("empty terminal_cmd is omitted from JSON", func(t *testing.T) {
		// terminal_cmd is optional, so configs that never set it round-trip
		// byte-identically to the pre-#843 format.
		data, err := json.Marshal(RemoteHooks{LaunchCmd: "/a"})
		require.NoError(t, err)
		assert.NotContains(t, string(data), "terminal_cmd")
	})

	t.Run("unmarshals correctly", func(t *testing.T) {
		jsonStr := `{"launch_cmd":"/a","list_cmd":"/b","attach_cmd":"/c","delete_cmd":"/d","terminal_cmd":"/e"}`
		var hooks RemoteHooks
		err := json.Unmarshal([]byte(jsonStr), &hooks)
		require.NoError(t, err)
		assert.Equal(t, "/a", hooks.LaunchCmd)
		assert.Equal(t, "/b", hooks.ListCmd)
		assert.Equal(t, "/c", hooks.AttachCmd)
		assert.Equal(t, "/d", hooks.DeleteCmd)
		assert.Equal(t, "/e", hooks.TerminalCmd)
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

// TestRemoteHooksValidate covers the fail-fast guard added for #738: empty
// (or whitespace-only) command strings for launch_cmd, attach_cmd, or
// delete_cmd must produce an actionable error naming the offending field
// rather than deferring to exec.Command's cryptic "exec: no command" at
// operation time. list_cmd is intentionally optional (import/sync treat an
// empty list_cmd as "no remote sessions to enumerate").
func TestRemoteHooksValidate(t *testing.T) {
	full := func() RemoteHooks {
		return RemoteHooks{
			LaunchCmd: "/bin/launch",
			ListCmd:   "/bin/list",
			AttachCmd: "/bin/attach",
			DeleteCmd: "/bin/delete",
		}
	}

	t.Run("fully populated is valid", func(t *testing.T) {
		assert.NoError(t, full().Validate())
	})

	t.Run("empty list_cmd is allowed", func(t *testing.T) {
		h := full()
		h.ListCmd = ""
		assert.NoError(t, h.Validate())
	})

	t.Run("empty terminal_cmd is allowed", func(t *testing.T) {
		// terminal_cmd is optional (#843): empty just disables the Terminal
		// tab for remote sessions, it is never a validation error. full()
		// leaves it empty, and setting it must validate too.
		assert.NoError(t, full().Validate())
		h := full()
		h.TerminalCmd = "/bin/terminal"
		assert.NoError(t, h.Validate())
	})

	cases := []struct {
		name    string
		mutate  func(*RemoteHooks)
		wantMsg string
	}{
		{"empty launch_cmd", func(h *RemoteHooks) { h.LaunchCmd = "" }, "remote_hooks.launch_cmd is required"},
		{"whitespace launch_cmd", func(h *RemoteHooks) { h.LaunchCmd = "   " }, "remote_hooks.launch_cmd is required"},
		{"empty attach_cmd", func(h *RemoteHooks) { h.AttachCmd = "" }, "remote_hooks.attach_cmd is required"},
		{"empty delete_cmd", func(h *RemoteHooks) { h.DeleteCmd = "" }, "remote_hooks.delete_cmd is required"},
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
		LaunchCmd:   "./hooks/launch.sh",
		ListCmd:     "/abs/list.sh",
		AttachCmd:   "ssh-attach",
		DeleteCmd:   "hooks/delete.sh",
		TerminalCmd: "./hooks/terminal.sh",
	}
	resolved := orig.resolveCommandPaths("/repo")

	assert.Equal(t, "/repo/hooks/launch.sh", resolved.LaunchCmd)
	assert.Equal(t, "/abs/list.sh", resolved.ListCmd)
	assert.Equal(t, "ssh-attach", resolved.AttachCmd)
	assert.Equal(t, "/repo/hooks/delete.sh", resolved.DeleteCmd)
	assert.Equal(t, "/repo/hooks/terminal.sh", resolved.TerminalCmd)

	assert.Equal(t, "./hooks/launch.sh", orig.LaunchCmd, "receiver must not be mutated")
	assert.Equal(t, "hooks/delete.sh", orig.DeleteCmd, "receiver must not be mutated")
	assert.Equal(t, "./hooks/terminal.sh", orig.TerminalCmd, "receiver must not be mutated")
}

package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertRemoteTabs asserts that inst carries the uniform remote tab model
// (#930 PR 6): an Agent tab always, plus a Shell (terminal) tab iff wantTerminal.
// Remote tabs are hook-driven, so neither tab carries a tmux session.
func assertRemoteTabs(t *testing.T, inst *Instance, wantTerminal bool) {
	t.Helper()
	tabs := inst.GetTabs()
	require.NotEmpty(t, tabs, "remote instance must have at least the agent tab")
	assert.Equal(t, TabKindAgent, tabs[0].Kind, "Tabs[0] must be the agent tab")
	assert.Nil(t, tabs[0].tmux, "remote agent tab carries no local tmux session")
	if wantTerminal {
		require.Len(t, tabs, 2, "terminal_cmd configured: expected agent + terminal tabs")
		assert.Equal(t, TabKindShell, tabs[1].Kind, "Tabs[1] must be the terminal (shell) tab")
		assert.Nil(t, tabs[1].tmux, "remote terminal tab carries no local tmux session")
	} else {
		require.Len(t, tabs, 1, "no terminal_cmd: expected only the agent tab")
	}
}

// hooksWithTerminal returns a HookBackend built from makeHooks, optionally with a
// terminal_cmd script wired in so the remote instance gains a terminal tab.
func hooksWithTerminal(t *testing.T, withTerminal bool) *HookBackend {
	t.Helper()
	b := makeHooks(t)
	if withTerminal {
		dir := t.TempDir()
		b.Hooks.TerminalCmd = writeScript(t, dir, "terminal.sh", `echo "terminal $1"; sleep 0.1`)
	}
	return b
}

// TestRemoteStartPopulatesTabs: a freshly launched remote instance gets an agent
// tab always, and a terminal tab only when terminal_cmd is configured.
func TestRemoteStartPopulatesTabs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		withTerminal bool
	}{
		{"with terminal_cmd", true},
		{"without terminal_cmd", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := hooksWithTerminal(t, tc.withTerminal)
			i := &Instance{Title: "test-session", Path: t.TempDir(), backend: b}

			require.NoError(t, b.Start(i, true))
			defer b.closePTY(i.Title)

			assertRemoteTabs(t, i, tc.withTerminal)
		})
	}
}

// TestRemoteRestorePopulatesTabs: the restore Start path (firstTimeSetup=false)
// reconstructs the same tab model from the live hook config.
func TestRemoteRestorePopulatesTabs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		withTerminal bool
	}{
		{"with terminal_cmd", true},
		{"without terminal_cmd", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := hooksWithTerminal(t, tc.withTerminal)
			i := &Instance{Title: "test-session", Path: t.TempDir(), backend: b}

			require.NoError(t, b.Start(i, false))
			defer b.closePTY(i.Title)

			assertRemoteTabs(t, i, tc.withTerminal)
		})
	}
}

// TestRemoteTabsReconstructedFromConfigOnRestore proves the terminal tab is
// derived from the live terminal_cmd config on restore, NOT from whatever tab
// list was serialized — so a terminal_cmd added or removed while af was down is
// honored. It drives both directions across a restart.
func TestRemoteTabsReconstructedFromConfigOnRestore(t *testing.T) {
	t.Run("terminal_cmd removed while down drops the terminal tab", func(t *testing.T) {
		// Save: terminal_cmd present -> agent + terminal tabs serialized.
		saved := hooksWithTerminal(t, true)
		i := &Instance{Title: "test-session", Path: t.TempDir(), backend: saved}
		require.NoError(t, saved.Start(i, true))
		defer saved.closePTY(i.Title)
		assertRemoteTabs(t, i, true)
		data := i.ToInstanceData()
		require.Len(t, data.Tabs, 2, "both tabs must serialize")

		// Restore against a config WITHOUT terminal_cmd: only the agent tab.
		restoredBackend := hooksWithTerminal(t, false)
		rebuilt := &Instance{Title: "test-session", Path: t.TempDir(), backend: restoredBackend, remoteMeta: data.RemoteMeta}
		restoreLocalTabsForRemoteTest(rebuilt, data)
		require.NoError(t, restoredBackend.Start(rebuilt, false))
		defer restoredBackend.closePTY(rebuilt.Title)
		assertRemoteTabs(t, rebuilt, false)
	})

	t.Run("terminal_cmd added while down gains the terminal tab", func(t *testing.T) {
		saved := hooksWithTerminal(t, false)
		i := &Instance{Title: "test-session", Path: t.TempDir(), backend: saved}
		require.NoError(t, saved.Start(i, true))
		defer saved.closePTY(i.Title)
		assertRemoteTabs(t, i, false)
		data := i.ToInstanceData()
		require.Len(t, data.Tabs, 1, "only the agent tab serializes")

		restoredBackend := hooksWithTerminal(t, true)
		rebuilt := &Instance{Title: "test-session", Path: t.TempDir(), backend: restoredBackend, remoteMeta: data.RemoteMeta}
		restoreLocalTabsForRemoteTest(rebuilt, data)
		require.NoError(t, restoredBackend.Start(rebuilt, false))
		defer restoredBackend.closePTY(rebuilt.Title)
		assertRemoteTabs(t, rebuilt, true)
	})
}

// restoreLocalTabsForRemoteTest mimics what FromInstanceData's local branch does
// to data.Tabs, seeding the rebuilt instance's Tabs from the persisted list. For
// remote instances HookBackend.Start then overwrites this from the live config —
// which is exactly the property the test asserts. Seeding a non-empty list first
// makes the "Start re-derives, ignoring persisted tabs" behavior observable.
func restoreLocalTabsForRemoteTest(inst *Instance, data InstanceData) {
	for _, td := range data.Tabs {
		inst.Tabs = append(inst.Tabs, &Tab{Name: td.Name, Kind: tabKindForData(td.Kind), Command: td.Command})
	}
}

// TestRemoteTabsPersistRoundTrip is the headline persistence test: a remote
// instance's tabs survive a full ToInstanceData -> FromInstanceData restart,
// reconstructed from the repo's terminal_cmd config (not a local tmux name).
func TestRemoteTabsPersistRoundTrip(t *testing.T) {
	repoDir := setupRemoteTabRepo(t, "remote-persist", true)

	inst, err := NewInstance(InstanceOptions{Title: "remote-persist", Path: repoDir, Program: "claude", ForceRemote: true})
	require.NoError(t, err)
	require.True(t, inst.IsRemote())
	require.NoError(t, inst.Start(true))
	if hb, ok := inst.GetBackend().(*HookBackend); ok {
		defer hb.closePTY(inst.Title)
	}
	assertRemoteTabs(t, inst, true)

	data := inst.ToInstanceData()
	require.Equal(t, "remote", data.BackendType)
	require.Len(t, data.Tabs, 2, "agent + terminal tabs must persist")

	rebuilt, err := FromInstanceData(data)
	require.NoError(t, err)
	if hb, ok := rebuilt.GetBackend().(*HookBackend); ok {
		defer hb.closePTY(rebuilt.Title)
	}
	require.True(t, rebuilt.IsRemote())
	assertRemoteTabs(t, rebuilt, true)
}

// setupRemoteTabRepo creates a git repo whose config declares remote_hooks with
// static (python-free) scripts. list_cmd always reports the given slug as
// running so the restore liveness check passes. terminal_cmd is wired iff
// withTerminal.
func setupRemoteTabRepo(t *testing.T, slug string, withTerminal bool) string {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "--local", "user.email", "test@af.com")
	runGit(t, repoDir, "config", "--local", "user.name", "AF Test")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("x"), 0644))
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "init")

	dir := t.TempDir()
	hooks := &config.RemoteHooks{
		LaunchCmd: writeScript(t, dir, "launch.sh", `echo '{"name": "'"$2"'", "status": "running"}'`),
		ListCmd:   writeScript(t, dir, "list.sh", `echo '[{"name": "`+slug+`", "status": "running"}]'`),
		AttachCmd: writeScript(t, dir, "attach.sh", `echo "attached $1"; sleep 0.1`),
		DeleteCmd: writeScript(t, dir, "delete.sh", `echo '{"name": "'"$2"'", "deleted": true}'`),
	}
	if withTerminal {
		hooks.TerminalCmd = writeScript(t, dir, "terminal.sh", `echo "terminal $1"; sleep 0.1`)
	}

	repo, err := config.RepoFromPath(repoDir)
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoConfig(repo.ID, &config.RepoConfig{RemoteHooks: hooks}))
	return repoDir
}

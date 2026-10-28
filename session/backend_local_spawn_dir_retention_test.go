package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/require"
)

// TestLoad_AccountScopedSiblingSpawnDirRefusalKeepsTheLiveAgent is the
// tmux.ErrSpawnDirMissing analogue of
// TestLoad_AccountUnknownSiblingProbeKeepsTheLiveAgent: a persisted
// account-scoped Running row whose agent tmux session is LIVE (reattached on
// load) while a sibling shell tab is dead (absent) and whose worktree directory
// was deleted out from under the live agent (rm -rf / git worktree remove
// --force while it ran, then a daemon restart with the tmux server still alive).
//
// The dead sibling's RestoreWithResult takes the respawn branch and the missing
// worktree makes the #5172 spawn-dir seam refuse it with tmux.ErrSpawnDirMissing.
// launch's setupTabs-failure branch correctly preserves the agent's tmux handle
// (preserveAgentHandle = retainsInertInstance(err) || errors.Is(err,
// tmux.ErrSpawnDirMissing)) because the binding — live or never-spawned — is what
// Recover reattaches or re-spawns. finishLaunchTabFailure must draw the same
// line: closing the reattached agent here would run `tmux kill-session` against
// the pane the preserve decision just kept, leaving a preserved handle pointing
// at a killed session — the exact contradiction retainsInertInstance's own
// comment calls out. Before the fix this killed the live agent and flipped the
// Running row to Lost behind the user's back.
func TestLoad_AccountScopedSiblingSpawnDirRefusalKeepsTheLiveAgent(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_spawn_dir_keeps_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	// The agent session is live (the reattach path, which never stats the
	// worktree); the shell session is GONE, so its RestoreWithResult takes the
	// respawn branch. With the worktree deleted below, that respawn is refused
	// up front by the #5172 spawn-dir seam — the sibling spawn-dir refusal that
	// reaches finishLaunchTabFailure.
	inner := nameKeyedExec(map[string]bool{agentName: true})
	var commands []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			commands = append(commands, cmd.String())
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	previousRestore := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = previousRestore })

	// The worktree path must be a real directory when GitWorktreeFromStorage
	// accepts it (existingDir creates it), then it is removed before load so the
	// sibling's respawn stats it and is refused — exactly the #5150 outage
	// shape a later daemon restart encounters after rm -rf / git worktree remove.
	worktreePath := existingDir(t, filepath.Join(t.TempDir(), "wt"))
	data := InstanceData{
		ID:       "spawn-dir-keeps-agent-id",
		Title:    "spawn-dir-keeps-agent",
		Path:     "/tmp/spawn-dir-keeps-agent-repo",
		Program:  tmux.ProgramCodex,
		Account:  "work",
		Status:   Running,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			// AccountScope matches the session's Account, so the sibling's scope
			// provenance is known and the pre-scope probe is skipped: the refusal
			// comes from the sibling's own RestoreWithResult, not from a probe.
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName, AccountScope: "work"},
		},
		Worktree: GitWorktreeData{
			RepoPath:     "/tmp/spawn-dir-keeps-agent-repo",
			WorktreePath: worktreePath,
			SessionName:  "spawn-dir-keeps-agent",
			BranchName:   "af/spawn-dir-keeps-agent",
		},
	}
	require.NoError(t, os.RemoveAll(worktreePath),
		"the worktree must be gone before load so the sibling's respawn is refused by the spawn-dir seam")

	instance, err := FromInstanceData(data)
	require.NoError(t, err,
		"a spawn-dir refusal routes the row Lost, not drops it — a dropped row is invisible to the lost-restore loop")
	require.NotNil(t, instance)

	// The primary guarantee: the live, just-reattached agent pane must NOT be
	// killed by the sibling's spawn-dir refusal. finishLaunchTabFailure must
	// honor the same preservation decision launch just made — mirroring the two
	// preserveAgentHandle sites in backend_local.go — or a preserved handle
	// points at a killed session and the Running row is silently flipped to Lost.
	require.False(t, commandIncludesSession(commands, "kill-session", agentName),
		"a SIBLING spawn-dir refusal must not tear down the separately confirmed live agent — "+
			"finishLaunchTabFailure must mirror launch's preserveAgentHandle decision")
	require.True(t, instance.TabAlive(0),
		"the live agent pane must survive the sibling's spawn-dir refusal — its process and scrollback are the whole point of keeping the handle")

	// The agent's cleanup handle must survive along with the pane it names:
	// nilling it would orphan the pane finishLaunchTabFailure just spared, and
	// Recover respawns the exact persisted tmux name.
	require.NotNil(t, instance.tmuxLocked(),
		"the agent's persisted tmux binding must survive — Recover respawns that exact name")
	stored := instance.ToInstanceData()
	require.Equal(t, agentName, stored.TmuxName,
		"the retained row must keep the cleanup handle for the agent it kept alive")
	require.Equal(t, shellName, stored.Tabs[1].TmuxName,
		"the refusing sibling's cleanup handle must survive alongside it")

	// The refusal routes the row Lost (the #5172 path in instance_data.go), so
	// the daemon's lost-restore loop can rebuild the worktree and re-spawn the
	// agent — and it can only reach a row that materialized.
	require.Equal(t, Lost, instance.GetStatus(),
		"a spawn-dir refusal loads Lost so the lost-restore loop can rebuild and respawn")
	require.True(t, instance.Started(),
		"the row must stay started so SaveInstances keeps it on disk and lostSessionWantsRestore admits it")
}

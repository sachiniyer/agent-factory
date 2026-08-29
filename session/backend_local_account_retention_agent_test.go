package session

import (
	"os/exec"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/require"
)

// An inconclusive sibling probe is a statement about the SIBLING. The loader
// answers it by retaining the row inert and killable so every exact tmux
// cleanup handle survives — which is only worth anything if the processes those
// handles name are still alive.
//
// The agent here is a separately confirmed, REATTACHED pane: this load did not
// spawn it, and nothing about it was in doubt. Tearing it down on a transient
// sibling probe timeout destroys a running agent and its scrollback to report a
// problem with a different tab, and leaves the retained row pointing at a
// session that no longer exists. finishRecoverTabFailure already draws this
// line correctly — it closes only what it respawned.
func TestLoad_AccountUnknownSiblingProbeKeepsTheLiveAgent(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_account_probe_keeps_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName
	inner := nameKeyedExec(map[string]bool{agentName: true, shellName: true})
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
	previousProbe := probeRestoredTabSession
	// Inconclusive, NOT absent: the sibling may well still be running.
	probeRestoredTabSession = func(session *tmux.TmuxSession) (bool, bool) {
		if session.SanitizedName() == shellName {
			return false, false
		}
		return session.ProbeSession()
	}
	t.Cleanup(func() { probeRestoredTabSession = previousProbe })

	instance, err := FromInstanceData(InstanceData{
		ID:       "account-probe-keeps-agent-id",
		Title:    "account-probe-keeps-agent",
		Path:     "/tmp/account-probe-keeps-agent-repo",
		Program:  tmux.ProgramCodex,
		Account:  "work",
		Status:   Running,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:     "/tmp/account-probe-keeps-agent-repo",
			WorktreePath: t.TempDir(),
			SessionName:  "account-probe-keeps-agent",
			BranchName:   "af/account-probe-keeps-agent",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, instance)
	require.True(t, instance.StartupStateUnknown(), "the retained row must stay inert until explicit teardown")

	require.False(t, commandIncludesSession(commands, "kill-session", agentName),
		"an inconclusive SIBLING probe must not tear down the separately confirmed live agent")

	// Surviving the teardown is only half of it: the retained row has to keep the
	// handle that NAMES the surviving agent, or the pane it just spared becomes an
	// orphan that no later kill or restore can reach.
	stored := instance.ToInstanceData()
	require.Equal(t, agentName, stored.TmuxName,
		"the retained row must keep the cleanup handle for the agent it kept alive")
	require.Equal(t, shellName, stored.Tabs[1].TmuxName,
		"the unknown sibling cleanup handle must survive alongside it")
}

package daemon

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestHandoffAccountPartialStopRecordsLostRecovery(t *testing.T) {
	m, repo, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.ClearLimitReached()
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	const agentName = "af_partial_account"
	inner := tabNameKeyedExec(map[string]bool{agentName: true})
	sibling := ""
	executor := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if sibling != "" && strings.Contains(cmd.String(), "kill-session") && strings.Contains(cmd.String(), sibling) {
				return errors.New("sibling teardown refused")
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	inst.SetBackend(&session.LocalBackend{})
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", tabPtyFactory{t: t, cmdExec: executor}, executor))
	_, err = inst.AddProcessTab("cat", "worker")
	require.NoError(t, err)
	sibling = inst.ToInstanceData().Tabs[1].TmuxName
	require.True(t, inst.TabAlive(0))
	require.True(t, inst.TabAlive(1))
	_, err = m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.ErrorContains(t, err, "sibling teardown refused")
	require.False(t, inst.TabAlive(0), "the agent stopped before the sibling refused teardown")
	require.True(t, inst.TabAlive(1))
	require.Equal(t, session.LiveLost, inst.GetLiveness())
	require.Equal(t, session.LiveLost, persistedInstanceByTitle(t, repo, inst.Title).Liveness)
	require.Nil(t, inst.ToInstanceData().PendingAccountSwap)
	require.Empty(t, inst.Account)
}

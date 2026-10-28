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
	require.False(t, isMutationCommitted(err))
	require.False(t, inst.TabAlive(0), "the agent stopped before the sibling refused teardown")
	require.True(t, inst.TabAlive(1))
	require.Equal(t, session.LiveLost, inst.GetLiveness())
	require.Equal(t, session.LiveLost, persistedInstanceByTitle(t, repo, inst.Title).Liveness)
	require.Nil(t, inst.ToInstanceData().PendingAccountSwap)
	require.Empty(t, inst.Account)
	require.False(t, inst.StartupStateUnknown())
	recovery := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst.SetBackend(recovery)
	m.RestoreLostSessions()
	require.Equal(t, 1, recovery.recoverCalls(), "confirmed agent stop still permits ordinary recovery")
}

func TestHandoffAccountSiblingBlindStopRecordsStartupUnknown(t *testing.T) {
	m, repo, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.ClearLimitReached()
	prepareHandoffTargetPreflight(t, inst)
	const agentName = "af_blind_sibling_agent"
	const siblingName = "af_blind_sibling_tab"
	var missing *exec.ExitError
	require.ErrorAs(t, exec.Command("sh", "-c", "exit 1").Run(), &missing)
	missing.Stderr = []byte("can't find session: " + siblingName)
	vanished := false
	started := false
	inner := tabNameKeyedExec(map[string]bool{agentName: true, siblingName: true})
	executor := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if vanished && strings.Contains(cmd.String(), "has-session") && strings.Contains(cmd.String(), siblingName) {
				return errors.New("session does not exist")
			}
			if started {
				require.NotContains(t, cmd.String(), "new-session")
			}
			return inner.Run(cmd)
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "display-message") && strings.Contains(cmd.String(), siblingName) && strings.Contains(cmd.String(), "pane_pid") {
				vanished = true
				return nil, nil
			}
			if vanished && strings.Contains(cmd.String(), "list-panes") && strings.Contains(cmd.String(), siblingName) {
				return nil, missing
			}
			return inner.Output(cmd)
		},
	}
	inst.SetBackend(&session.LocalBackend{})
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", tabPtyFactory{t: t, cmdExec: executor}, executor))
	_, err := inst.AddProcessTab("cat", siblingName)
	require.NoError(t, err)
	started = true
	_, err = m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.True(t, vanished)
	require.ErrorContains(t, err, "detached child")
	require.ErrorIs(t, err, session.ErrAccountSwapAgentTeardownBlind)
	require.True(t, inst.StartupStateUnknown())
	require.NotEqual(t, session.LiveLost, inst.GetLiveness())
	recovery := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst.SetBackend(recovery)
	m.RestoreLostSessions()
	require.Zero(t, recovery.recoverCalls())
}

func TestHandoffAccountSiblingUnknownStopRecordsStartupUnknown(t *testing.T) {
	m, repo, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.ClearLimitReached()
	prepareHandoffTargetPreflight(t, inst)
	const agentName = "af_unknown_sibling_agent"
	const siblingName = "af_unknown_sibling_tab"
	inner := tabNameKeyedExec(map[string]bool{agentName: true, siblingName: true})
	executor := cmd_test.MockCmdExec{
		RunFunc: inner.Run,
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "display-message") && strings.Contains(cmd.String(), siblingName) {
				return nil, tmux.ErrTmuxTimeout
			}
			return inner.Output(cmd)
		},
	}
	inst.SetBackend(&session.LocalBackend{})
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", tabPtyFactory{t: t, cmdExec: executor}, executor))
	_, err := inst.AddProcessTab("cat", siblingName)
	require.NoError(t, err)

	_, err = m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.ErrorIs(t, err, tmux.ErrTmuxTimeout)
	require.ErrorIs(t, err, session.ErrAccountSwapAgentTeardownBlind)
	require.True(t, inst.StartupStateUnknown())
	require.NotEqual(t, session.LiveLost, inst.GetLiveness())
	recovery := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst.SetBackend(recovery)
	m.RestoreLostSessions()
	require.Zero(t, recovery.recoverCalls())
}

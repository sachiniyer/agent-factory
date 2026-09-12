package session

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestAccountSwapBlindTeardownIdentifiesOnlyAgent(t *testing.T) {
	for _, agentBlind := range []bool{true, false} {
		name := "sibling"
		if agentBlind {
			name = "agent"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			names := []string{"af_blind_agent", "af_unrelated_shell"}
			blindName := names[1]
			if agentBlind {
				blindName = names[0]
			}
			var missing *exec.ExitError
			require.ErrorAs(t, exec.Command("sh", "-c", "exit 1").Run(), &missing)
			missing.Stderr = []byte("can't find session: " + blindName)
			executor := cmd_test.MockCmdExec{
				RunFunc: func(c *exec.Cmd) error {
					if strings.Contains(c.String(), "has-session") {
						return errors.New("session does not exist")
					}
					return nil
				},
				OutputFunc: func(c *exec.Cmd) ([]byte, error) {
					if strings.Contains(c.String(), "list-panes") && strings.Contains(c.String(), blindName) {
						return nil, missing
					}
					return nil, nil
				},
			}
			inst := accountSwapTestInstance("claude")
			repo := initTempGitRepo(t)
			gw, err := git.NewGitWorktreeFromStorage(repo, repo, inst.Title, "main", "", false, true)
			require.NoError(t, err)
			inst.gitWorktree = gw
			inst.Tabs = []*Tab{
				newAgentTab(tmux.NewTmuxSessionFromSanitizedNameWithDeps(names[0], "claude", nil, executor)),
				{ID: "shell", Name: "shell", Kind: TabKindProcess, Command: "cat", tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps(names[1], "cat", nil, executor)},
			}
			err = inst.StopForAccountSwap()
			require.ErrorContains(t, err, "detached child")
			require.True(t, errors.Is(fmt.Errorf("wrapped teardown: %w", err), ErrAccountSwapAgentTeardownBlind), "every blind credential-bearing tab must classify the teardown as unsafe")
		})
	}
}

func TestAccountSwapAbsentProbeStillChecksAgentPane(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const name = "af_absent_before_account_swap"
	var missing *exec.ExitError
	require.ErrorAs(t, exec.Command("sh", "-c", "exit 1").Run(), &missing)
	missing.Stderr = []byte("can't find session: " + name)
	executor := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return missing },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(c.String(), "list-panes") {
				return nil, missing
			}
			return nil, nil
		},
	}
	inst := accountSwapTestInstance("claude")
	repo := initTempGitRepo(t)
	gw, err := git.NewGitWorktreeFromStorage(repo, repo, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.gitWorktree = gw
	inst.liveness = LiveRunning
	inst.Account = "work"
	inst.Tabs = []*Tab{newAgentTab(tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, "claude", nil, executor))}

	err = inst.StopRemainingPanesForAccountSwap()
	require.ErrorIs(t, err, ErrAccountSwapAgentTeardownBlind)
	require.ErrorContains(t, err, "detached child")
	require.Equal(t, LiveRunning, inst.GetLiveness())
	require.Equal(t, "work", inst.Account)
	require.Nil(t, inst.ToInstanceData().PendingAccountSwap,
		"an absent probe must not let the manual handoff commit its replacement identity")
}

func TestAccountSwapSkipsProvenPanelessSibling(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const agentName = "af_swap_live_agent"
	inner := nameKeyedExec(map[string]bool{agentName: true})
	inst := accountSwapTestInstance("claude")
	repo := initTempGitRepo(t)
	gw, err := git.NewGitWorktreeFromStorage(repo, repo, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.gitWorktree = gw
	paneless := neverSpawnedSession(t, "af_swap_paneless_sibling")
	require.True(t, paneless.ProvenNoPane())
	inst.Tabs = []*Tab{
		newAgentTab(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", nil, inner)),
		{ID: "shell", Name: "shell", Kind: TabKindProcess, Command: "cat", tmux: paneless},
	}

	require.NoError(t, inst.StopForAccountSwap(),
		"positive no-pane provenance must keep exempting a sibling from teardown")
}

func TestAccountSwapUnknownSiblingTeardownIsUnsafe(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	names := []string{"af_unknown_agent", "af_unknown_shell"}
	inner := nameKeyedExec(map[string]bool{names[0]: true, names[1]: true})
	executor := cmd_test.MockCmdExec{
		RunFunc: inner.Run,
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(c.String(), "display-message") && strings.Contains(c.String(), names[1]) {
				return nil, tmux.ErrTmuxTimeout
			}
			return inner.Output(c)
		},
	}
	inst := accountSwapTestInstance("claude")
	repo := initTempGitRepo(t)
	gw, err := git.NewGitWorktreeFromStorage(repo, repo, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.gitWorktree = gw
	inst.Tabs = []*Tab{
		newAgentTab(tmux.NewTmuxSessionFromSanitizedNameWithDeps(names[0], "claude", nil, executor)),
		{ID: "shell", Name: "shell", Kind: TabKindProcess, Command: "cat", tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps(names[1], "cat", nil, executor)},
	}

	err = inst.StopForAccountSwap()
	require.ErrorContains(t, err, "cannot confirm credential-bearing tab")
	require.ErrorIs(t, err, tmux.ErrTmuxTimeout)
	require.ErrorIs(t, err, ErrAccountSwapAgentTeardownBlind,
		"every unconfirmed credential-bearing teardown must block automatic recovery")
}

// exitedPanePID returns a PID whose process has exited and been reaped, so
// tmux.TmuxSession's pre-kill identity check (proctree.Snapshot then
// syscall.Kill(pid, 0)) resolves the pane leader as already gone - the
// production outcome for a just-started session whose pane process dies on
// kill-session's SIGHUP. Mirrors exitedProcess in session/tmux/close_wait_test.go.
func exitedPanePID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.NoError(t, cmd.Process.Kill())
	_, _ = cmd.Process.Wait()
	return pid
}

// TestRespawnFreshRedundantStopSkipsAfterConclusiveInnerClose is the #703b4a70
// fix's end-to-end mechanism test, driving the two real closes
// respawnFresh performs on the setupTabs-failure path against one shared tmux
// session:
//
//  1. finishRecoverTabFailure's inner CloseAndWaitForPaneExit on the live
//     replacement pane, which is conclusive and observed (PaneStateKnown,
//     blind=false, nil) on a responsive server.
//  2. respawnFresh's outer stopForAccountSwap on the now-dead session, which the
//     fix's ProvenNoPane latch skips - instead of re-closing the dead session,
//     re-classifying it blind (pane PID empty + session vanished), and wrapping
//     ErrAccountSwapAgentTeardownBlind onto the setupTabs error.
//
// Before the fix, step 2 re-closed the dead session and tripped the
// idx==0 && blind guard at account_swap.go:416-419, so this test fails RED
// (StopForAccountSwap returns the phantom blind sentinel) without the latch.
func TestRespawnFreshRedundantStopSkipsAfterConclusiveInnerClose(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const agentName = "af_swap_conclusive_inner"
	// alive tracks the shared session's liveness across the two closes: live for
	// the inner close, dead for the outer re-close - exactly what real tmux
	// answers for two consecutive closes on a session the first one killed.
	alive := true
	var missing *exec.ExitError
	require.ErrorAs(t, exec.Command("sh", "-c", "exit 1").Run(), &missing)
	missing.Stderr = []byte("can't find session: " + agentName)
	livePID := exitedPanePID(t)
	executor := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "has-session") {
				if alive {
					return nil
				}
				return errors.New("session does not exist")
			}
			if strings.Contains(c.String(), "kill-session") {
				alive = false
				return nil // idempotent: succeeds on the live session, no-ops on the dead one
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			s := c.String()
			if strings.Contains(s, "display-message") {
				if alive {
					return []byte(fmt.Sprintf("%d\n", livePID)), nil
				}
				// A dead session answers display-message with exit 0 and empty
				// output, so panePID yields errPaneQueryFoundNoPane.
				return nil, nil
			}
			if strings.Contains(s, "list-panes") {
				if alive {
					// The live pane had no descendants, so captureSessionProcessTrees
					// returns (nil, nil) and the close is conclusive and non-blind.
					return []byte(""), nil
				}
				// The dead session is gone: list-panes answers exit 1 with the exact
				// diagnostic, which captureSessionProcessTrees maps to
				// ErrSessionVanishedBeforeCapture.
				return nil, missing
			}
			return nil, nil
		},
	}
	inst := accountSwapTestInstance("claude")
	repo := initTempGitRepo(t)
	gw, err := git.NewGitWorktreeFromStorage(repo, repo, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.gitWorktree = gw
	inst.Tabs = []*Tab{newAgentTab(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", nil, executor))}

	// Phase 1: finishRecoverTabFailure's inner close on the live replacement pane.
	state, blind, closeErr := inst.Tabs[0].tmux.CloseAndWaitForPaneExitReportingBlindness()
	require.Equal(t, tmux.PaneStateKnown, state, "the inner close on the live replacement pane is conclusive")
	require.False(t, blind, "the inner close observed the pane's process set, so it is not blind")
	require.NoError(t, closeErr, "the inner close returns no error on a responsive tmux server")
	require.True(t, inst.Tabs[0].tmux.ProvenNoPane(),
		"a conclusive non-blind teardown latches the pane-gone proof so the redundant outer stop is skipped")

	// Phase 2: respawnFresh's outer stopForAccountSwap on the now-dead session.
	stopErr := inst.StopForAccountSwap()
	require.NoError(t, stopErr,
		"the redundant outer stop after a conclusive inner close is skipped, not re-classified blind")
	require.False(t, errors.Is(stopErr, ErrAccountSwapAgentTeardownBlind),
		"no phantom blind sentinel may be wrapped when the teardown was conclusively observed")
}

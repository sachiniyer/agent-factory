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

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

package session

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// socketGoneExec answers every has-session the way tmux does when the socket
// path is gone (exit 1, "error connecting to <socket> (No such file or
// directory)") and counts any new-session, which a respawn would issue.
func socketGoneExec(t *testing.T, newSessions *int) cmd_test.MockCmdExec {
	t.Helper()
	const diagnostic = "error connecting to /tmp/tmux-1000/default (No such file or directory)"
	failure := exec.Command("sh", "-c", `printf '%s\n' "$1" >&2; exit 1`, "sh", diagnostic)
	_, failErr := failure.Output()
	require.Error(t, failErr)
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			s := c.String()
			switch {
			case strings.Contains(s, "new-session"):
				*newSessions++
				return nil
			case strings.Contains(s, "has-session"):
				if c.Stderr != nil {
					_, _ = c.Stderr.Write([]byte(diagnostic + "\n"))
				}
				return failErr
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, failErr },
	}
}

// #4135: a persisted row whose tmux socket is missing under a possibly-live
// server must be RETAINED as it was when its restore is refused — listed,
// started, its tmux binding and workspace untouched — never dropped, marked
// dead, or respawned beside the still-running agent. The restore is deferred.
func TestFromInstanceData_UnverifiedSocketRetainsTheRowAndDefersRestore(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Cleanup(tmux.PinServerProbeForTest(os.Getpid()))

	const agentName = "af_socket_unverified_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	var newSessions int
	exec := socketGoneExec(t, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	data := deadInstanceData(t, Running, agentName, shellName)
	worktree := data.Worktree.WorktreePath

	restored, err := FromInstanceData(data)
	require.NoError(t, err, "a deferred restore must keep the row, not drop it")
	require.NotNil(t, restored)
	assert.Equal(t, Lost, restored.GetStatus(), "surfaced as Lost so the restore loop retries it later")
	assert.True(t, restored.Started(), "started, so storage keeps the record and the loop admits it")
	require.NotNil(t, restored.tmuxLocked(), "the persisted tmux binding must survive for the later restore")
	assert.Equal(t, 0, newSessions, "no second agent may be spawned beside a possibly-live one")
	_, statErr := os.Stat(worktree)
	assert.NoError(t, statErr, "the workspace must be untouched")
}

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/stretchr/testify/require"
)

// deadPID returns a PID that is guaranteed to have exited and been reaped:
// we spawn a trivial process and wait for it ourselves.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	return cmd.Process.Pid
}

func TestWaitForPIDExit_ExitedProcess(t *testing.T) {
	start := time.Now()
	require.True(t, waitForPIDExit(deadPID(t), 2*time.Second),
		"an already-exited PID must report exited")
	require.Less(t, time.Since(start), time.Second,
		"a dead PID must be detected without burning the timeout")
}

func TestWaitForPIDExit_AliveProcessTimesOut(t *testing.T) {
	// Our own PID is alive for the duration of the test.
	require.False(t, waitForPIDExit(os.Getpid(), 120*time.Millisecond),
		"a live PID must report not-exited once the timeout elapses")
}

// TestCloseAndWaitForPaneExit_QueriesPaneBeforeKill verifies the #802
// ordering contract: the pane PID is captured via display-message BEFORE
// kill-session runs (afterwards there is nothing left to query), and the
// wait happens on that PID after the session is gone.
func TestCloseAndWaitForPaneExit_QueriesPaneBeforeKill(t *testing.T) {
	pid := deadPID(t)
	var calls []string

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				calls = append(calls, "kill-session")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "display-message") {
				calls = append(calls, "display-message")
				return []byte(fmt.Sprintf("%d\n", pid)), nil
			}
			return []byte(""), nil
		},
	}

	session := newTmuxSession(toTmuxName("close-wait", ""), "claude", NewMockPtyFactory(t), cmdExec)

	start := time.Now()
	require.NoError(t, session.CloseAndWaitForPaneExit())
	require.Less(t, time.Since(start), time.Second,
		"wait on an already-dead agent process must return promptly")

	require.Equal(t, []string{"display-message", "kill-session"}, calls,
		"pane PID must be captured before kill-session destroys the session")
}

// TestCloseAndWaitForPaneExit_SessionGone: when the pane PID cannot be
// queried (session already dead), the method must skip the wait and still
// perform the Close teardown.
func TestCloseAndWaitForPaneExit_SessionGone(t *testing.T) {
	killed := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				killed = true
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("can't find session")
		},
	}

	session := newTmuxSession(toTmuxName("close-wait-gone", ""), "claude", NewMockPtyFactory(t), cmdExec)

	start := time.Now()
	require.NoError(t, session.CloseAndWaitForPaneExit())
	require.Less(t, time.Since(start), time.Second)
	require.True(t, killed, "kill-session must still run when the pane PID is unqueryable")
}

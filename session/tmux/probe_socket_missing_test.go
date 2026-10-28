package tmux

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// has-session fails the way tmux does when the socket path is gone: exit 1 and
// "error connecting to <socket> (No such file or directory)" on stderr. The
// fake executor writes that diagnostic to the command's Stderr the way a real
// exec.Cmd would, then reports the exit-1 failure.
func socketMissingExecutor(t *testing.T) cmd_test.MockCmdExec {
	t.Helper()
	failure := tmuxExitOneError(t, "error connecting to /tmp/tmux-1000/default (No such file or directory)")
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if c.Stderr != nil {
				_, _ = c.Stderr.Write([]byte("error connecting to /tmp/tmux-1000/default (No such file or directory)\n"))
			}
			return failure
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, failure },
	}
}

// #4135: a socket file a /tmp cleaner unlinked while its server (and the agent
// pane) keeps running answers ENOENT to every client. The liveness probe used to
// read that as "tmux answered: session gone", the daemon marked the live session
// Lost, and its local restore respawned a SECOND agent into the same worktree.
// With a live server that may own the socket the answer must be unknown.
func TestProbeSessionSocketMissingWithALiveServerIsUnknown(t *testing.T) {
	// A pid that is alive but not a positively identified tmux server: the probe
	// can neither signal it nor disprove that it owns the unlinked socket.
	t.Cleanup(PinServerProbeForTest(os.Getpid()))

	exists, known := probeSession(socketMissingExecutor(t), "af_live_agent")
	require.False(t, exists)
	require.False(t, known, "an unlinked socket backed by a live server must not be read as absence")
}

// With no tmux server alive for this uid the same diagnostic is a determinate
// absence, exactly as before.
func TestProbeSessionSocketMissingWithNoServerIsAbsent(t *testing.T) {
	t.Cleanup(PinServerProbeForTest())

	exists, known := probeSession(socketMissingExecutor(t), "af_gone_agent")
	require.False(t, exists)
	require.True(t, known, "no server anywhere: the session is determinately absent")
}

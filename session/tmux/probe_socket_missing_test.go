package tmux

import (
	"context"
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

func pinSocketClaimProof(t *testing.T, available bool) {
	t.Helper()
	prev := socketClaimProofAvailable
	socketClaimProofAvailable = func() bool { return available }
	t.Cleanup(func() { socketClaimProofAvailable = prev })
}

func probeLiveness(t *testing.T, name string) (exists, known bool) {
	t.Helper()
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	return probeSessionLivenessContext(ctx, socketMissingExecutor(t), name)
}

// #4135: a socket file a /tmp cleaner unlinked while its server (and the agent
// pane) keeps running answers ENOENT to every client. The LIVENESS probe of an
// existing session must read that as unknown, not gone: the daemon would
// otherwise respawn a SECOND agent into the same worktree.
func TestLivenessProbeSocketMissingWithALiveServerIsUnknown(t *testing.T) {
	pinSocketClaimProof(t, true)
	// A pid that is alive but not a positively identified tmux server: the probe
	// can neither signal it nor disprove that it owns the unlinked socket.
	t.Cleanup(PinServerProbeForTest(os.Getpid()))

	exists, known := probeLiveness(t, "af_live_agent")
	require.False(t, exists)
	require.False(t, known, "an unlinked socket backed by a live server must not be read as absence")
}

// With no tmux server alive for this uid the same diagnostic is a determinate
// absence for the liveness probe too.
func TestLivenessProbeSocketMissingWithNoServerIsAbsent(t *testing.T) {
	pinSocketClaimProof(t, true)
	t.Cleanup(PinServerProbeForTest())

	exists, known := probeLiveness(t, "af_gone_agent")
	require.False(t, exists)
	require.True(t, known)
}

// The darwin shape: no /proc, so the live-server check is inconclusive (a
// process table that cannot be read reports a server present). The liveness
// probe falls back to the pre-#4135 answer — absent — rather than staying
// unknown forever on a host whose server is really gone.
func TestLivenessProbeFallsBackToAbsentWhereTheServerCheckIsInconclusive(t *testing.T) {
	pinSocketClaimProof(t, false)
	t.Cleanup(PinServerProbeForTest(0))

	exists, known := probeLiveness(t, "af_agent")
	require.False(t, exists)
	require.True(t, known, "without a conclusive live-server check the liveness probe keeps the legacy answer")
}

// The PRE-SPAWN existence probe (Start's has-session gate) asks whether a name
// is taken. A new server binds a fresh socket file, so a socket-ENOENT is plain
// absence there whatever the live-server check says — including the darwin
// shape where it is inconclusive. Answering unknown made session creation fail
// with "did not answer the pre-spawn has-session probe" on a fresh host.
func TestPreSpawnProbeTreatsSocketMissingAsAbsentRegardlessOfServerCheck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		proof     bool
		serverPID []int
	}{
		{"linux with a live server", true, []int{os.Getpid()}},
		{"linux with no server", true, nil},
		{"darwin shape, server check inconclusive", false, []int{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinSocketClaimProof(t, tc.proof)
			t.Cleanup(PinServerProbeForTest(tc.serverPID...))

			exists, known := probeSession(socketMissingExecutor(t), "af_new_agent")
			require.False(t, exists)
			require.True(t, known, "socket-ENOENT must be a determinate absence for the pre-spawn probe")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exists, known = probeSessionContext(ctx, socketMissingExecutor(t), "af_new_agent")
			require.False(t, exists)
			require.True(t, known)
		})
	}
}

package daemon

import (
	"fmt"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// fakeShutdownTarget starts a fake af daemon for this home running script,
// records it in daemon.pid, and returns it as a Shutdown's target with a
// channel that reports its exit.
func fakeShutdownTarget(t *testing.T, home, script string) (ShutdownTarget, <-chan *os.ProcessState) {
	t.Helper()
	cmd := spawnFakeDaemonHomeProc(t, "af", script, "--daemon", "af-test")
	pid := cmd.Process.Pid
	waitForReady(t, fmt.Sprintf("pid=%d cmdline exposes --daemon", pid), func() bool { return isAgentFactoryDaemon(pid) })
	exited := make(chan *os.ProcessState, 1)
	go func(cmd *exec.Cmd) {
		state, _ := cmd.Process.Wait()
		exited <- state
	}(cmd)
	require.NoError(t, os.WriteFile(filepath.Join(home, "daemon.pid"), []byte(fmt.Sprintf("%d", pid)), 0o600))
	return ShutdownTarget{PID: pid, StartToken: processStartTokenFn(pid)}, exited
}

// TestFinishShutdownWithinGraceSignalsADaemonStillRunning: a daemon the
// Shutdown RPC stopped that has not exited within StopDaemon's grace is
// stopped through StopDaemon's own verified path with no second grace: SIGTERM,
// then SIGKILL at once. So a stopper that asked through the RPC (`af daemon
// adopt`) displaces it within StopDaemon's own bound, rather than waiting out
// shutdownCompleteGrace and giving up (#7480). A daemon that exits within its
// grace is left alone.
func TestFinishShutdownWithinGraceSignalsADaemonStillRunning(t *testing.T) {
	origGrace, origPoll := stopDaemonGrace, stopDaemonPoll
	stopDaemonGrace, stopDaemonPoll = time.Second, 25*time.Millisecond
	t.Cleanup(func() { stopDaemonGrace, stopDaemonPoll = origGrace, origPoll })

	t.Run("still running at its grace", func(t *testing.T) {
		home := testguard.SocketTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", home)
		readyFile := filepath.Join(home, "trap-ready")
		// It ignores SIGTERM, as a daemon whose stop never ends does in effect.
		target, exited := fakeShutdownTarget(t, home, fmt.Sprintf(`trap "" TERM; : > %s; %s`, readyFile,
			testguard.BoundedSpin(time.Second, 5*time.Minute)))
		waitForReady(t, "trap-ready sentinel", func() bool { _, err := os.Stat(readyFile); return err == nil })

		start := time.Now()
		escalated, err := FinishShutdownWithinGrace(target)
		elapsed := time.Since(start)
		require.NoError(t, err)
		require.True(t, escalated, "a daemon still running at its grace must be stopped by signal")
		require.GreaterOrEqual(t, elapsed, stopDaemonGrace, "the daemon must get its grace first")
		require.Less(t, elapsed, 2*stopDaemonGrace,
			"a second grace between SIGTERM and SIGKILL would take the displacement past StopDaemon's bound")
		select {
		case state := <-exited:
			ws, ok := state.Sys().(syscall.WaitStatus)
			require.True(t, ok)
			require.True(t, ws.Signaled() && ws.Signal() == syscall.SIGKILL, "want SIGKILL, got %v", state)
		case <-time.After(5 * time.Second):
			t.Fatal("the fake daemon did not exit")
		}
	})
	t.Run("exits within its grace", func(t *testing.T) {
		home := testguard.SocketTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", home)
		// Two commands, so the shell cannot exec sleep and lose its af argv.
		target, exited := fakeShutdownTarget(t, home, "sleep 0.5; :")

		escalated, err := FinishShutdownWithinGrace(target)
		require.NoError(t, err)
		require.False(t, escalated, "a daemon that exits within its grace is never signalled")
		state := <-exited
		require.True(t, state.Exited(), "it exited on its own, unsignalled: %v", state)
	})
}

// unansweringShutdownControl is a daemon that accepted the connection but does
// not answer its Shutdown, and, if pingStalls, not its Ping either, until
// release closes.
type unansweringShutdownControl struct {
	release    <-chan struct{}
	pingStalls bool
}

func (c unansweringShutdownControl) Ping(_ PingRequest, _ *PingResponse) error {
	if c.pingStalls {
		<-c.release
	}
	return nil
}

func (c unansweringShutdownControl) Shutdown(_ ShutdownRequest, resp *ShutdownResponse) error {
	<-c.release
	resp.OK = true
	return nil
}

// TestRequestRestartShutdownWithinAbandonsAnUnansweredShutdown: a bounded
// restart Shutdown to a daemon that never answers it gives up at its bound with
// ShutdownError, rather than holding its caller, so `af daemon adopt` reaches
// its signal fallback (#7481). That holds on both paths: Shutdown on the pinged
// connection, and on a fresh one after a Ping that missed its own bound.
func TestRequestRestartShutdownWithinAbandonsAnUnansweredShutdown(t *testing.T) {
	for _, pingStalls := range []bool{false, true} {
		t.Run(map[bool]string{false: "after a Ping", true: "after a stalled Ping"}[pingStalls], func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			release := make(chan struct{})
			srv := rpc.NewServer()
			require.NoError(t, srv.RegisterName(controlServiceName, unansweringShutdownControl{release: release, pingStalls: pingStalls}))
			_, cleanup := startFakeControlListener(t, srv)
			t.Cleanup(cleanup)
			t.Cleanup(func() { close(release) })

			type answer struct {
				result ShutdownResult
				err    error
			}
			answered := make(chan answer, 1)
			start := time.Now()
			go func() {
				result, _, err := RequestRestartShutdownWithin(200 * time.Millisecond)
				answered <- answer{result, err}
			}()
			select {
			case got := <-answered:
				require.Equal(t, ShutdownError, got.result)
				require.ErrorIs(t, got.err, errShutdownUnanswered)
				require.Less(t, time.Since(start), 2*time.Second)
			case <-time.After(5 * time.Second):
				t.Fatal("an unanswered Shutdown held its caller past its bound")
			}
		})
	}
}

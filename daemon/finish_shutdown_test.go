package daemon

import (
	"fmt"
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

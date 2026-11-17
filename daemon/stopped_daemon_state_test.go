package daemon

import (
	"errors"
	"net/rpc"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"
)

// latchStoppedDaemonForTest makes this test's returning daemon latch its
// process's gates closed, as production does, and restores the inert
// defaults, and TestMain's setting, when the test ends.
func latchStoppedDaemonForTest(t *testing.T) {
	t.Helper()
	daemonReturnRestoresProcessDefaults = false
	t.Cleanup(func() {
		daemonReturnRestoresProcessDefaults = true
		resetInProcessDaemonForTest()
	})
}

// installInProcessLifecycleForTest stands lifecycle in for the daemon running
// in this process, until the test ends.
func installInProcessLifecycleForTest(t *testing.T, lifecycle *daemonLifecycle) {
	t.Helper()
	inProcessDaemon.Store(lifecycle)
	t.Cleanup(resetInProcessDaemonForTest)
}

func resetInProcessDaemonForTest() {
	daemonDrainingFn = func() bool { return false }
	taskDeliveries.Store(nil)
	inProcessDaemon.Store(nil)
	inProcessDaemonStopped.Store(false)
}

// TestRunDaemonReturnedLeavesItsDeliveryGatesClosed: once the daemon has
// returned, its process never admits a delivery untracked, and never reads a
// late delivery's transport error as a task failure. A goroutine starved
// through the whole teardown is refused before it writes and keeps its marker
// for the next daemon, and the client in that process ensures no daemon.
func TestRunDaemonReturnedLeavesItsDeliveryGatesClosed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubContainerSweep(t)
	latchStoppedDaemonForTest(t)

	runExited := startInProcessDaemon(t, nil)
	waitForDaemonPhase(t, DaemonPhaseReady)
	requestShutdown(t, runExited)

	_, admitted := beginTaskDelivery()
	require.False(t, admitted, "a returned daemon's process admitted a delivery")
	require.True(t, daemonDrainingFn(), "a returned daemon's process reads a late transport error as a task failure")
	require.True(t, IsDaemonStoppingErr(inProcessDaemonEnsureRefusal()), "a returned daemon's process may ensure a daemon")

	repo := setupTaskRepo(t)
	seedCronTask(t, "ab7474a1", repo)
	_, _, err := ensureTaskFireMarker("ab7474a1")
	require.NoError(t, err)
	require.True(t, IsDaemonStoppingErr(RunTask("ab7474a1", task.ProjectExpectation{})))
	require.True(t, markerExists(t, "ab7474a1"), "the late delivery must leave its marker for the next daemon")
}

// TestAStoppingDaemonNeverEnsuresADaemon: in a daemon that is stopping or
// restarting, an unanswered control socket is its own stop, not a daemon to
// repair. A delivery's client never launches one, and a fire's dead socket is
// a shutdown abort that keeps its marker. An upgrade hand-off is unchanged.
func TestAStoppingDaemonNeverEnsuresADaemon(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "a final stop", true: "a restart"}[restart], func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			withAutostartTestEnv(t, runtime.GOOS)
			lifecycle := stoppingLifecycle(t, restart)
			installInProcessLifecycleForTest(t, lifecycle)
			pinDaemonDraining(t, true)
			var launches atomic.Int32
			previous := launchDaemonProcessFn
			launchDaemonProcessFn = func() error {
				launches.Add(1)
				return errors.New("a stopping daemon launched a daemon")
			}
			t.Cleanup(func() { launchDaemonProcessFn = previous })

			require.True(t, IsDaemonStoppingErr(EnsureDaemonUntil(time.Now().Add(2*time.Second))))

			repo := setupTaskRepo(t)
			seedCronTask(t, "ab7474b1", repo)
			err := RunTask("ab7474b1", task.ProjectExpectation{})
			require.True(t, IsDaemonStoppingErr(err), "got %v", err)
			require.True(t, markerExists(t, "ab7474b1"), "a fire cut short by its own daemon's stop is replayed")
			require.Zero(t, launches.Load(), "a stopping daemon launched a daemon")
		})
	}
	t.Run("an upgrade hand-off", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
		manager.lifecycle.markQuiescing()
		installInProcessLifecycleForTest(t, manager.lifecycle)
		require.NoError(t, inProcessDaemonEnsureRefusal())
	})
}

// alwaysRefusingControl is a daemon whose socket still answers, and refuses
// the mutation with a refusal a client would otherwise wait out.
type alwaysRefusingControl struct{}

func (alwaysRefusingControl) Ping(_ PingRequest, _ *PingResponse) error { return nil }
func (alwaysRefusingControl) QuiescingMutation(_ struct{}, _ *struct{}) error {
	return errDaemonStarting()
}

// TestARestartingDaemonsOwnCallIsNotRetried: a delivery inside a restarting
// daemon, refused with any refusal a client would retry, ends at once as a
// shutdown abort. It is not waited out, nor retried into the successor from
// inside the daemon the successor replaces, where it would hold the stop for
// the whole retry window.
func TestARestartingDaemonsOwnCallIsNotRetried(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	srv := rpc.NewServer()
	require.NoError(t, srv.RegisterName(controlServiceName, alwaysRefusingControl{}))
	_, cleanup := startFakeControlListener(t, srv)
	t.Cleanup(cleanup)
	installInProcessLifecycleForTest(t, stoppingLifecycle(t, true))

	start := time.Now()
	err := callDaemon("QuiescingMutation", struct{}{}, &struct{}{})
	require.True(t, IsDaemonStoppingErr(err), "got %v", err)
	require.Less(t, time.Since(start), time.Second, "the restarting daemon waited out its own refusal")
}

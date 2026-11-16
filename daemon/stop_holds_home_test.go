package daemon

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/throwawaysandbox"
)

// #7439: a stop that abandoned anything exits while it still holds the AF
// home, so nothing it abandoned can touch the home after the release; the
// final save is bounded like every other stop step; and a stop signal's
// deadline starts when the signal arrives, through RunDaemon itself.

// recordExitHoldingHome is the package-wide exitHoldingHome TestMain installs:
// an in-process daemon must never end the test binary. It records nothing a
// test reads; tests that assert on the exit install captureExitHoldingHome.
func recordExitHoldingHome(holdingHomeExit) {}

// captureExitHoldingHome replaces exitHoldingHome for one test. atExit runs
// inside the exit, before it is reported on the returned channel, so a test can
// look at what the process still holds at that instant.
func captureExitHoldingHome(t *testing.T, atExit func()) <-chan holdingHomeExit {
	t.Helper()
	exits := make(chan holdingHomeExit, 4)
	previous := exitHoldingHome
	exitHoldingHome = func(exit holdingHomeExit) {
		if atExit != nil {
			atExit()
		}
		exits <- exit
	}
	// Registered before the test starts its daemon, so it is restored only
	// after that daemon has been joined (cleanups run LIFO).
	t.Cleanup(func() { exitHoldingHome = previous })
	return exits
}

// stopOrder records, in order, the exit and the two releases it must precede.
type stopOrder struct {
	mu     sync.Mutex
	events []string
}

func (o *stopOrder) note(event string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func (o *stopOrder) seen() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

// watchHomeReleases notes the latch and flock releases in order.
func watchHomeReleases(t *testing.T, order *stopOrder) {
	t.Helper()
	previousLatch, previousLock := testHookDaemonBeforeHomeLatchRelease, testHookDaemonBeforeHomeLockRelease
	testHookDaemonBeforeHomeLatchRelease = func() { order.note("latch released") }
	testHookDaemonBeforeHomeLockRelease = func() { order.note("flock released") }
	t.Cleanup(func() {
		testHookDaemonBeforeHomeLatchRelease, testHookDaemonBeforeHomeLockRelease = previousLatch, previousLock
	})
}

// startInProcessDaemon runs RunDaemon on its own goroutine and joins it at
// cleanup, stopping it first when a failure left it running.
func startInProcessDaemon(t *testing.T, cleanup func()) <-chan struct{} {
	t.Helper()
	runExited := make(chan struct{})
	go func() {
		defer close(runExited)
		_ = RunDaemon(tcpDisabledConfig())
	}()
	t.Cleanup(func() {
		if cleanup != nil {
			cleanup()
		}
		select {
		case <-runExited:
		default:
			var resp ShutdownResponse
			_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
		}
		select {
		case <-runExited:
		case <-time.After(10 * time.Second):
			t.Error("in-process daemon did not exit during cleanup")
		}
	})
	return runExited
}

// captureManager hands the test the daemon's manager through the restore
// seam, leaving the restore itself unchanged.
func captureManager(t *testing.T) <-chan *Manager {
	t.Helper()
	managers := make(chan *Manager, 1)
	previous := restoreManagerForStartup
	restoreManagerForStartup = func(m *Manager) error {
		managers <- m
		return m.restoreInstances()
	}
	t.Cleanup(func() { restoreManagerForStartup = previous })
	return managers
}

// TestRunDaemonStopThatAbandonedSomethingExitsHoldingTheHome: a stop that
// abandoned a teardown step ends the process while it still holds the home's
// flock and before it releases the latch, so the abandoned step can never
// write to a home a successor owns. A stop that abandoned nothing releases
// both and removes daemon.pid, as before.
func TestRunDaemonStopThatAbandonedSomethingExitsHoldingTheHome(t *testing.T) {
	t.Run("a teardown step never returns", func(t *testing.T) {
		home := testguard.SocketTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", home)
		warn := captureWarnings(t)
		stubContainerSweep(t)
		shortenTeardown(t, time.Second)
		order := &stopOrder{}
		watchHomeReleases(t, order)
		var flockAtExit error
		exits := captureExitHoldingHome(t, func() {
			// A free flock would be taken here; give it straight back.
			var lock *homeLock
			if lock, flockAtExit = acquireHomeLockAt(home); flockAtExit == nil {
				lock.release()
			}
			order.note("exit")
		})

		release := make(chan struct{})
		previousStop := stopSandboxTunnels
		stopSandboxTunnels = func() { <-release }
		t.Cleanup(func() { stopSandboxTunnels = previousStop })

		runExited := startInProcessDaemon(t, func() { close(release) })
		waitForDaemonPhase(t, DaemonPhaseReady)
		requestShutdown(t, runExited)

		select {
		case exit := <-exits:
			require.Equal(t, []string{"teardown: sandbox tunnels"}, exit.abandoned)
			require.Zero(t, exit.code, "a requested stop that abandoned a step still exits 0")
		default:
			t.Fatalf("the stop abandoned a step but never exited holding the home; it unwound through %v", order.seen())
		}
		require.ErrorIs(t, flockAtExit, errLockHeld, "the exit must come while this daemon still holds the home's flock")
		require.Equal(t, []string{"exit", "latch released", "flock released"}, order.seen(),
			"the exit must come before the latch and the flock are released")
		require.Contains(t, warn.String(), "exiting while the daemon still holds its AF home's lock and latch")
	})

	t.Run("nothing was abandoned", func(t *testing.T) {
		home := testguard.SocketTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", home)
		stubContainerSweep(t)
		exits := captureExitHoldingHome(t, nil)

		runExited := startInProcessDaemon(t, nil)
		waitForDaemonPhase(t, DaemonPhaseReady)
		requestShutdown(t, runExited)

		select {
		case exit := <-exits:
			t.Fatalf("a stop that abandoned nothing exited holding the home: %v", exit.abandoned)
		default:
		}
		_, err := os.Stat(filepath.Join(home, "daemon.pid"))
		require.ErrorIs(t, err, os.ErrNotExist, "a clean stop removes daemon.pid")
	})
}

// TestRunDaemonWarmupExitEntersTheRunningRestoreInTheLedger: a Shutdown during
// the instance restore leaves the restore goroutine running; nothing joins it.
// The stop enters it in its ledger, so the process exits holding the home
// instead of letting the restore write there after the release.
func TestRunDaemonWarmupExitEntersTheRunningRestoreInTheLedger(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubContainerSweep(t)

	managers := make(chan *Manager, 1)
	holdRestore := make(chan struct{})
	previousRestore := restoreManagerForStartup
	restoreManagerForStartup = func(m *Manager) error {
		managers <- m
		<-holdRestore
		return nil
	}
	cfg := config.DefaultConfig()
	cfg.ListenAddr = ""
	runExited := make(chan struct{})
	t.Cleanup(func() {
		close(holdRestore)
		select {
		case <-runExited:
		default:
			var resp ShutdownResponse
			_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
		}
		select {
		case <-runExited:
		case <-time.After(10 * time.Second):
			t.Error("in-process daemon did not exit during cleanup")
		}
		restoreManagerForStartup = previousRestore
	})
	go func() {
		defer close(runExited)
		_ = RunDaemon(cfg)
	}()
	var manager *Manager
	select {
	case manager = <-managers:
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon never reached its instance restore")
	}
	waitForDaemonPhase(t, DaemonPhaseWarming)

	requestShutdown(t, runExited)
	require.Contains(t, manager.lifecycle.stopClock().abandonments(), "the instance restore",
		"a warm-up exit must enter the restore it leaves running in the stop's ledger")
}

// TestRunDaemonFinalSaveEndsAtItsDeadline: a final save that never returns no
// longer holds the teardown. The teardown starts at the save's deadline — the
// drain's worst case plus the save's reserve after the stop began — and the
// save is named and entered in the stop's ledger.
func TestRunDaemonFinalSaveEndsAtItsDeadline(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	warn := captureWarnings(t)
	stubContainerSweep(t)
	shortenDrainTotal(t, 300*time.Millisecond)
	shortenTeardown(t, 3*time.Second)
	managers := captureManager(t)
	// The save's deadline under the shortened drain: 300ms, a floor for each
	// later drain step, and the save's 1s reserve.
	saveDue := saveBy()

	saving := make(chan struct{}, 1)
	releaseSave := make(chan struct{})
	previousSave := finalSave
	finalSave = func(*Manager) error {
		saving <- struct{}{}
		<-releaseSave
		return nil
	}
	t.Cleanup(func() { finalSave = previousSave })

	teardownStarted := make(chan time.Time, 1)
	previousStop := stopSandboxTunnels
	stopSandboxTunnels = func() { teardownStarted <- time.Now() }
	t.Cleanup(func() { stopSandboxTunnels = previousStop })

	runExited := startInProcessDaemon(t, func() { close(releaseSave) })
	var manager *Manager
	select {
	case manager = <-managers:
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon never reached its instance restore")
	}
	waitForDaemonPhase(t, DaemonPhaseReady)

	asked := time.Now()
	requestShutdown(t, runExited)
	select {
	case <-saving:
	default:
		t.Fatal("the stop never reached the final save")
	}
	started := <-teardownStarted
	require.Less(t, started.Sub(asked), saveDue+boundMargin,
		"the teardown must start by the save's deadline, whatever the save is doing")
	require.GreaterOrEqual(t, started.Sub(asked), saveDue-100*time.Millisecond,
		"the save must be given until its deadline")
	require.Contains(t, manager.lifecycle.stopClock().abandonments(), "the final save")
	require.Contains(t, warn.String(), "the final save did not finish by")
}

// TestRunDaemonStopSignalDeadlineStartsAtArrival: a SIGTERM delivered to the
// daemon while its main flow is busy in a startup step sits buffered until the
// main select reads it. The stop's deadline still starts when the signal
// arrived, so a teardown step that never returns is abandoned at arrival plus
// stopTeardownBy, not at the main select's read plus stopTeardownBy.
func TestRunDaemonStopSignalDeadlineStartsAtArrival(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubContainerSweep(t)
	const teardownBy = 2 * time.Second
	// How long the main flow is held after the signal: a clock that started at
	// the read instead of the arrival ends the stop this much late. The rest of
	// the startup tail runs after it, before the teardown, so the anchored stop
	// ends at teardownBy as long as that tail stays under teardownBy - busyFor.
	const busyFor = time.Second
	shortenTeardown(t, teardownBy)

	releaseStep := make(chan struct{})
	stepHeld := make(chan struct{}, 1)
	previousHook := testHookStartupStep
	testHookStartupStep = func(name string) {
		// The first step after the restore: the main flow, past the startup
		// signal watch, so the signal is buffered rather than re-raised.
		if name == "legacy task unit sweep" {
			stepHeld <- struct{}{}
			<-releaseStep
		}
	}
	t.Cleanup(func() { testHookStartupStep = previousHook })

	releaseTunnels := make(chan struct{})
	previousStop := stopSandboxTunnels
	stopSandboxTunnels = func() { <-releaseTunnels }
	t.Cleanup(func() { stopSandboxTunnels = previousStop })

	var releaseStepOnce sync.Once
	runExited := startInProcessDaemon(t, func() {
		releaseStepOnce.Do(func() { close(releaseStep) })
		close(releaseTunnels)
	})
	select {
	case <-stepHeld:
	case <-time.After(20 * time.Second):
		t.Fatal("the daemon never reached its post-restore startup steps")
	}

	// The daemon is this process; its stop signals are delivered to it.
	sent := time.Now()
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	time.Sleep(busyFor)
	releaseStepOnce.Do(func() { close(releaseStep) })

	select {
	case <-runExited:
	case <-time.After(teardownBy + busyFor + 3*time.Second):
		t.Fatal("RunDaemon never finished stopping after SIGTERM")
	}
	took := time.Since(sent)
	require.Less(t, took, teardownBy+boundMargin,
		"the stop's deadline must start when its signal arrived, not when the main flow read it")
}

// TestRunDaemonExpiredThrowawayIsTornDownBeforeTheExitHoldingTheHome: an
// expired throwaway sandbox whose stop abandoned something is still torn down,
// before the process exits holding the home. Only this daemon and a later
// marked start reap the sandbox's processes and private tmux servers, and a
// throwaway home's daemon may never start again (#7439). The teardown runs
// once.
func TestRunDaemonExpiredThrowawayIsTornDownBeforeTheExitHoldingTheHome(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	stubContainerSweep(t)
	shortenTeardown(t, time.Second)

	// The test's home is AGENT_FACTORY_HOME, which the marker check would read
	// as the default install; point that check and the per-user registry at
	// directories of the test's own.
	notDefault, registry := t.TempDir(), t.TempDir()
	previousDefault, previousRegistry, previousTeardown := throwawayDefaultHomeFn, throwawayRegistryDirFn, throwawayTeardownFn
	throwawayDefaultHomeFn = func() (string, error) { return notDefault, nil }
	throwawayRegistryDirFn = func() string { return registry }
	order := &stopOrder{}
	throwawayTeardownFn = func(string, *throwawaysandbox.Marker) { order.note("throwaway teardown") }
	previousContainerReap := throwawayHomeContainerReapFn
	throwawayHomeContainerReapFn = func(string) bool { order.note("container reap"); return true }
	t.Cleanup(func() {
		throwawayDefaultHomeFn, throwawayRegistryDirFn, throwawayTeardownFn = previousDefault, previousRegistry, previousTeardown
		throwawayHomeContainerReapFn = previousContainerReap
	})
	// Expired before the daemon starts, so its watchdog ends it at once.
	require.NoError(t, throwawaysandbox.WriteMarker(home, &throwawaysandbox.Marker{
		Version: 1, Kind: "test-7439", CreatedUnix: time.Now().Add(-time.Hour).Unix(), MaxLifetimeSeconds: 1,
	}))
	captureExitHoldingHome(t, func() { order.note("exit") })

	release := make(chan struct{})
	previousStop := stopSandboxTunnels
	stopSandboxTunnels = func() { <-release }
	t.Cleanup(func() { stopSandboxTunnels = previousStop })

	runExited := startInProcessDaemon(t, func() { close(release) })
	select {
	case <-runExited:
	case <-time.After(stopWithin + 10*time.Second):
		t.Fatal("the expired throwaway's daemon never stopped")
	}
	// Whether the stop exits holding the home is the first test's property;
	// this one is that such an exit never skips the sandbox's teardown.
	seen := order.seen()
	require.NotEmpty(t, seen, "the expired sandbox was never torn down")
	require.Equal(t, "throwaway teardown", seen[0],
		"the expired sandbox must be torn down before the exit that skips the rest of the unwind: %v", seen)
	teardowns := 0
	for _, event := range seen {
		if event == "throwaway teardown" {
			teardowns++
		}
	}
	require.Equal(t, 1, teardowns, "the sandbox is torn down once: %v", seen)
	// Its containers go with it, before the exit too (#7447).
	require.GreaterOrEqual(t, len(seen), 2, "the expired sandbox's containers were never reaped: %v", seen)
	require.Equal(t, "container reap", seen[1],
		"the expired sandbox's containers must be reaped right after its teardown, before the exit: %v", seen)
}

// TestRunDaemonPanicDuringAnAbandonedStopExitsTwoWithItsStack: a panic on the
// main flow during a stop that abandoned something must not end the process
// with success. The exit holding the home reports the panic's value and the
// stack it panicked on, and exits 2, so systemd's Restart=on-failure fires
// (#7447). Before, its os.Exit(0) swallowed the panic without a trace.
func TestRunDaemonPanicDuringAnAbandonedStopExitsTwoWithItsStack(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubContainerSweep(t)
	shortenTeardown(t, time.Second)
	exits := captureExitHoldingHome(t, nil)

	release := make(chan struct{})
	previousStop := stopSandboxTunnels
	stopSandboxTunnels = func() { <-release }
	t.Cleanup(func() { stopSandboxTunnels = previousStop })

	const panicValue = "boom-7447: a post-restore startup step panicked"
	previousHook := testHookStartupStep
	testHookStartupStep = func(name string) {
		// On the main flow, after the exit-holding defer is registered.
		if name == "legacy task unit sweep" {
			panic(panicValue)
		}
	}
	t.Cleanup(func() { testHookStartupStep = previousHook })

	// A panic that escapes RunDaemon is caught here, so a regression fails this
	// test instead of ending the test binary.
	runExited := make(chan struct{})
	escaped := make(chan any, 1)
	go func() {
		defer close(runExited)
		defer func() { escaped <- recover() }()
		_ = RunDaemon(tcpDisabledConfig())
	}()
	t.Cleanup(func() {
		close(release)
		select {
		case <-runExited:
		case <-time.After(10 * time.Second):
			t.Error("in-process daemon did not exit during cleanup")
		}
	})

	var exit holdingHomeExit
	select {
	case exit = <-exits:
	case <-runExited:
		// The exit is recorded before RunDaemon returns, so both can be ready
		// at once; look for it before calling this a skipped exit (Codex on
		// #7448).
		select {
		case exit = <-exits:
		default:
			t.Fatalf("the panicking stop never exited holding the home; the panic escaped RunDaemon: %v", <-escaped)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the panicking daemon never stopped")
	}
	require.Contains(t, exit.abandoned, "teardown: sandbox tunnels")
	require.Equal(t, 2, exit.code, "a panicking runDaemon must never exit holding the home with success")
	require.Contains(t, exit.panicReport, panicValue, "the exit must report the panic's value")
	require.Contains(t, exit.panicReport, "startupStep(", "the exit must report the stack the daemon panicked on")
}

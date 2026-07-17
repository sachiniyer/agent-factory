package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// Tests for WaitForShutdownCompletion and the #854 upgrade-respawn race: the
// Shutdown RPC acks before the daemon tears down, so a respawn that runs
// immediately can ping the still-alive dying daemon, skip the spawn, and
// leave nothing running. Every test points AGENT_FACTORY_HOME at a temp dir
// so the control socket under test is private — the host's real supervised
// daemon is never pinged, signaled, or spawned.

// TestWaitForShutdownCompletionNoDaemon: with no socket at all (the SIGTERM
// fallback path, or a daemon that already finished tearing down), the wait
// must return nil on its first ping rather than burning the grace.
func TestWaitForShutdownCompletionNoDaemon(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	start := time.Now()
	if err := WaitForShutdownCompletion(); err != nil {
		t.Fatalf("WaitForShutdownCompletion with no daemon: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("wait with no daemon took %s, want an immediate return", elapsed)
	}
}

// TestUpgradeRespawnWaitsForDelayedTeardown reproduces the #854 race shape
// end to end: a fake daemon acks the Shutdown RPC but holds its control
// socket open well past the ack (shutdownAckGrace plus a stretched teardown
// tail). The shutdown-then-respawn sequence — RequestShutdown, wait, then
// EnsureDaemon — must end with exactly one spawn. Pre-fix, EnsureDaemon ran
// without the wait, pinged the still-alive socket, and returned without
// spawning anything.
func TestUpgradeRespawnWaitsForDelayedTeardown(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	shutdownCh := make(chan struct{})
	closeFn, err := startControlServer(nil, nil, nil, shutdownCh)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	// The dying daemon's teardown tail: after the Shutdown handler closes
	// shutdownCh (post shutdownAckGrace), keep answering on the socket a
	// while longer before closing the listener.
	teardownDone := make(chan struct{})
	go func() {
		<-shutdownCh
		time.Sleep(300 * time.Millisecond)
		_ = closeFn()
		close(teardownDone)
	}()
	t.Cleanup(func() {
		select {
		case <-teardownDone:
		case <-time.After(5 * time.Second):
			t.Errorf("fake daemon teardown goroutine never finished")
		}
	})

	spawns := 0
	var newDaemonClose func() error
	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error {
		spawns++
		// The "new daemon": bind a fresh control server so EnsureDaemon's
		// readiness poll sees it come up.
		var bindErr error
		newDaemonClose, bindErr = startControlServer(nil, nil, nil, nil)
		return bindErr
	}
	t.Cleanup(func() {
		launchDaemonProcessFn = prevLaunch
		if newDaemonClose != nil {
			_ = newDaemonClose()
		}
	})

	result, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown: %v", err)
	}
	if result != ShutdownViaRPC {
		t.Fatalf("shutdown result = %v, want ShutdownViaRPC", result)
	}

	if err := WaitForShutdownCompletion(); err != nil {
		t.Fatalf("WaitForShutdownCompletion: %v", err)
	}
	if pingDaemon() == nil {
		t.Fatalf("old daemon still answering after WaitForShutdownCompletion returned")
	}

	if err := EnsureDaemon(); err != nil {
		t.Fatalf("EnsureDaemon after the wait: %v", err)
	}
	if spawns != 1 {
		t.Fatalf("daemon spawns = %d, want 1 — the respawn must spawn a new daemon after the old socket dies, not skip against the dying one (#854)", spawns)
	}
}

// TestWaitForShutdownCompletionTimesOut: a daemon that never stops answering
// (wedged teardown) must produce an error at the grace deadline so the caller
// can warn — not hang forever or silently report success.
func TestWaitForShutdownCompletionTimesOut(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	closeFn, err := startControlServer(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })

	prevGrace := shutdownCompleteGrace
	shutdownCompleteGrace = 250 * time.Millisecond
	t.Cleanup(func() { shutdownCompleteGrace = prevGrace })

	if err := WaitForShutdownCompletion(); err == nil {
		t.Fatalf("expected a timeout error while the daemon socket keeps answering")
	}
}

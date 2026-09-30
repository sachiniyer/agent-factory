package daemon

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// Tests for WaitForShutdownCompletion and the #854 upgrade-respawn race: the
// Shutdown RPC acks before the daemon tears down, so a respawn that runs
// immediately can ping the still-alive dying daemon, skip the spawn, and
// leave nothing running. Every test points AGENT_FACTORY_HOME at a temp dir
// so the control socket and home lock under test are private — the host's real
// supervised daemon is never pinged, signaled, or spawned.

// TestWaitForShutdownCompletion_Exited_NoDaemonReturnsAtOnce: with no daemon at all — no socket and
// no lock file, as in a home that never ran one — the PID-less wait must
// return nil on its first probe rather than burning the grace.
func TestWaitForShutdownCompletion_Exited_NoDaemonReturnsAtOnce(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	start := time.Now()
	if err := WaitForShutdownCompletion(ShutdownPID{}); err != nil {
		t.Fatalf("WaitForShutdownCompletion with no daemon: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("wait with no daemon took %s, want an immediate return", elapsed)
	}
}

// TestRespawn_Draining_WaitsForLockReleaseThenSpawns reproduces the #854 race shape
// end to end, with #5007's tail: a fake daemon holds the home lock, acks the
// Shutdown RPC, keeps its control socket open past the ack, then CLOSES the
// socket and keeps holding the lock a while longer — drainDaemon's durable-join
// tail. The shutdown-then-respawn sequence — RequestShutdown, wait, then
// EnsureDaemon — must not proceed until the lock is released, and must end
// with exactly one spawn. Pre-#854 EnsureDaemon pinged the still-alive socket
// and skipped the spawn; pre-#5007 the wait returned on the quiet socket while
// the lock was still held, so the replacement would lose it.
func TestRespawn_Draining_WaitsForLockReleaseThenSpawns(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	oldLock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock for the old daemon: %v", err)
	}
	var lockReleased atomic.Bool
	shutdownCh := make(chan struct{})
	closeFn, err := startControlServer(nil, nil, nil, shutdownCh)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	// The dying daemon's teardown tail: after the Shutdown handler closes
	// shutdownCh (post shutdownAckGrace), keep answering on the socket a while,
	// close the listener, then hold the home lock through the durable joins.
	teardownDone := make(chan struct{})
	go func() {
		<-shutdownCh
		time.Sleep(200 * time.Millisecond)
		_ = closeFn()
		time.Sleep(300 * time.Millisecond)
		lockReleased.Store(true)
		oldLock.release()
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

	result, pid, err := RequestShutdown()
	if err != nil {
		t.Fatalf("RequestShutdown: %v", err)
	}
	if result != ShutdownViaRPC {
		t.Fatalf("shutdown result = %v, want ShutdownViaRPC", result)
	}

	// The in-process fake reports this test's own PID, which the wait refuses
	// to watch, so this exercises the PID-less home-lock path end to end.
	if err := WaitForShutdownCompletion(pid); err != nil {
		t.Fatalf("WaitForShutdownCompletion: %v", err)
	}
	if !lockReleased.Load() {
		t.Fatalf("WaitForShutdownCompletion returned while the old daemon still held the home lock (#5007): a quiet socket is not an exit")
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

// TestWaitForShutdownCompletion_Draining_BoundIsShutdownIncomplete: a daemon that never releases its home
// lock (a long drain, or a wedged teardown) must produce ErrShutdownIncomplete
// at the grace deadline — not hang forever or silently report success. No
// control socket exists at all here: a quiet socket must not read as exit.
func TestWaitForShutdownCompletion_Draining_BoundIsShutdownIncomplete(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	lock, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock: %v", err)
	}
	t.Cleanup(lock.release)

	prevGrace := shutdownCompleteGrace
	shutdownCompleteGrace = 250 * time.Millisecond
	t.Cleanup(func() { shutdownCompleteGrace = prevGrace })

	if err := WaitForShutdownCompletion(ShutdownPID{}); !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("WaitForShutdownCompletion(ShutdownPID{}) = %v, want ErrShutdownIncomplete while the home lock is held", err)
	}
}

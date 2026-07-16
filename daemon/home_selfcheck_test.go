package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The tests in this file cover the #1093 abandoned-daemon self-check: a daemon
// whose own AF home directory has been deleted (an abandoned temp/test home,
// or an rm -rf'd install) is unreachable via the control plane yet used to
// keep firing cron schedules forever. RunDaemon now watches its home and shuts
// down cleanly once the directory has been missing on two consecutive checks.

// TestRunDaemon_ExitsWhenHomeDeleted drives the fix end to end in-process:
// start a daemon against a temp home, verify it stays up while the home
// exists, delete the home, and assert RunDaemon returns within a bounded time
// without recreating the deleted directory.
func TestRunDaemon_ExitsWhenHomeDeleted(t *testing.T) {
	// The home lives one level below a temp dir so deleting it cannot break that
	// dir's own cleanup.
	//
	// SocketTempDir, not t.TempDir: this daemon binds daemon.sock inside this home,
	// and t.TempDir's test-name-bearing path put the socket at 125 bytes — 22 over
	// sun_path's ceiling — so the daemon never bound and never reported ready. The
	// failure surfaced 20s later as "daemon ready" never holding, nowhere near the
	// home-deletion this test is actually about (#1931).
	home := filepath.Join(testguard.SocketTempDir(t), "af-home")
	if err := os.MkdirAll(home, 0755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("AGENT_FACTORY_HOME", home)
	// This test runs RunDaemon to full readiness, which sweeps legacy task
	// units — point the sweep at empty temp dirs so it can never touch the
	// host's real systemd/launchd directories.
	stubLegacyUnitSweep(t)

	prevInterval := homeCheckInterval
	homeCheckInterval = 25 * time.Millisecond
	t.Cleanup(func() { homeCheckInterval = prevInterval })

	runDone := make(chan error, 1)
	runExited := make(chan struct{})
	go func() {
		runDone <- RunDaemon(config.DefaultConfig())
		close(runExited)
	}()

	// Failsafe teardown: if the test fails before the home deletion below,
	// the in-process daemon is still serving its socket — end it via the
	// Shutdown RPC so it cannot bleed into later tests in the package.
	// runExited (closed, so readable forever) rather than runDone (its one
	// buffered value is consumed by the assertions below) tells us whether
	// RunDaemon already returned.
	t.Cleanup(func() {
		select {
		case <-runExited:
			return
		default:
		}
		var resp ShutdownResponse
		_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
		select {
		case <-runExited:
		case <-time.After(5 * time.Second):
			t.Errorf("in-process daemon did not exit during cleanup")
		}
	})

	// Wait past warm-up: Snapshot is gated on the instance restore (#829), so
	// a success means RunDaemon reached its main loop and armed the watcher.
	waitForReady(t, "daemon ready (Snapshot RPC passes the warm-up gate)", func() bool {
		var resp SnapshotResponse
		return callDaemonNoEnsure("Snapshot", SnapshotRequest{}, &resp) == nil
	})

	// While the home exists the daemon must ride out many check intervals —
	// a false-positive self-shutdown here is the failure mode the two-strike
	// rule exists to prevent.
	select {
	case err := <-runDone:
		t.Fatalf("daemon exited while its home still existed: %v", err)
	case <-time.After(10 * homeCheckInterval):
	}

	if err := os.RemoveAll(home); err != nil {
		t.Fatalf("delete home: %v", err)
	}

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("RunDaemon returned an error on the home-deleted self-shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("daemon did not shut down within 10s of its home directory being deleted (#1093)")
	}

	// The exit path must not resurrect the deleted home (the normal shutdown
	// SaveInstances would recreate a skeleton of it).
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("daemon recreated its deleted home on shutdown, stat err = %v", err)
	}
}

// TestApplyHomeCheck_RequiresTwoConsecutiveMisses unit-tests the two-strike
// rule deterministically, one observation at a time: a home that disappears
// for a single check and is back for the next (a transient stat blip, a
// racing restore) must reset the counter and never reach the threshold, while
// two consecutive misses must.
func TestApplyHomeCheck_RequiresTwoConsecutiveMisses(t *testing.T) {
	home := filepath.Join(t.TempDir(), "af-home")
	mkHome := func() {
		t.Helper()
		if err := os.MkdirAll(home, 0755); err != nil {
			t.Fatalf("mkdir home: %v", err)
		}
	}
	rmHome := func() {
		t.Helper()
		if err := os.RemoveAll(home); err != nil {
			t.Fatalf("delete home: %v", err)
		}
	}

	// Present home: counter stays at zero.
	mkHome()
	misses, exit := applyHomeCheck(home, 0)
	if misses != 0 || exit {
		t.Fatalf("present home: got (misses=%d, exit=%v), want (0, false)", misses, exit)
	}

	// First miss: counted, but one strike must not fire.
	rmHome()
	misses, exit = applyHomeCheck(home, misses)
	if misses != 1 || exit {
		t.Fatalf("first miss: got (misses=%d, exit=%v), want (1, false)", misses, exit)
	}

	// Transient blip: the home is back on the next check, so the counter
	// resets instead of accumulating toward the threshold.
	mkHome()
	misses, exit = applyHomeCheck(home, misses)
	if misses != 0 || exit {
		t.Fatalf("home restored after one miss: got (misses=%d, exit=%v), want (0, false)", misses, exit)
	}

	// A real deletion: two consecutive misses reach the threshold.
	rmHome()
	misses, exit = applyHomeCheck(home, misses)
	if misses != 1 || exit {
		t.Fatalf("first miss of real deletion: got (misses=%d, exit=%v), want (1, false)", misses, exit)
	}
	if _, exit = applyHomeCheck(home, misses); !exit {
		t.Fatalf("second consecutive miss did not trigger the self-shutdown")
	}
}

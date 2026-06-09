package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// startTestControlServer binds a control server on the temp-home socket path
// and registers its teardown. It stands in for a freshly-started "new" daemon
// in the stop/start race tests below.
func startTestControlServer(t *testing.T) {
	t.Helper()
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	closeServer, err := startControlServer(manager, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })
}

// TestStopDaemon_PreservesNewDaemonSocket is the regression test for #767.
// Timeline it reproduces:
//
//  1. Old daemon A is running; its PID is in daemon.pid.
//  2. StopDaemon SIGTERMs A and polls for its exit.
//  3. During (or right after) that window a NEW daemon B starts and binds the
//     control socket (autostart unit racing `af daemon install`, or an
//     upgrade respawn).
//  4. BUG (pre-fix): cleanupDaemonRuntimeFiles unconditionally removed the
//     socket path, unlinking B's live socket — B kept serving an unreachable
//     inode and the next EnsureDaemon spawned a third daemon while B leaked.
//
// Post-fix, cleanup pings the socket first and leaves the runtime files alone
// when a live daemon answers.
func TestStopDaemon_PreservesNewDaemonSocket(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	tmpHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)

	// Daemon B: a live control server already bound on the socket path.
	startTestControlServer(t)
	if err := pingDaemon(); err != nil {
		t.Fatalf("control server not answering before StopDaemon: %v", err)
	}

	// Daemon A: a fake old daemon that exits on SIGTERM and exposes
	// "--daemon" as a discrete cmdline token (same recipe as
	// TestStopDaemon_SIGTERMFirst).
	cmd := exec.Command("bash", "-c", "exec -a 'sleep --daemon af-test' sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if isAgentFactoryDaemon(pid) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !isAgentFactoryDaemon(pid) {
		t.Fatalf("fake daemon pid=%d not recognized as agent-factory daemon", pid)
	}

	// Reap A in a goroutine so /proc/<pid>/cmdline clears once it exits and
	// the StopDaemon poll observes the death promptly.
	go func() { _, _ = cmd.Process.Wait() }()

	pidFile := filepath.Join(tmpHome, "daemon.pid")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", pid)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	if err := StopDaemon(); err != nil {
		t.Fatalf("StopDaemon: %v", err)
	}

	socketPath, err := DaemonSocketPath()
	if err != nil {
		t.Fatalf("DaemonSocketPath: %v", err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("StopDaemon removed the new daemon's socket file (#767): %v", err)
	}
	if err := pingDaemon(); err != nil {
		t.Fatalf("new daemon no longer answers after StopDaemon (#767): %v", err)
	}
	// The whole runtime-file set is preserved when a live daemon answers —
	// the PID file on disk may already be the new daemon's.
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("StopDaemon removed the PID file despite a live daemon answering: %v", err)
	}
}

// TestCleanupDaemonRuntimeFiles_SkipsLiveDaemonFiles unit-tests the #767 guard
// directly: with a live daemon answering on the socket, cleanup must leave
// both runtime files in place.
func TestCleanupDaemonRuntimeFiles_SkipsLiveDaemonFiles(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)

	startTestControlServer(t)

	pidFile := filepath.Join(tmpHome, "daemon.pid")
	if err := os.WriteFile(pidFile, []byte("12345"), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	cleanupDaemonRuntimeFiles(pidFile)

	socketPath, err := DaemonSocketPath()
	if err != nil {
		t.Fatalf("DaemonSocketPath: %v", err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("cleanup removed a live daemon's socket: %v", err)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("cleanup removed the PID file despite a live daemon answering: %v", err)
	}
	if err := pingDaemon(); err != nil {
		t.Fatalf("live daemon stopped answering after cleanup: %v", err)
	}
}

// TestCleanupDaemonRuntimeFiles_RemovesDeadFiles is the companion negative
// case: when nothing answers on the socket path (a stale file from a killed
// daemon), cleanup must still remove both runtime files — the #767 guard must
// not suppress legitimate cleanup.
func TestCleanupDaemonRuntimeFiles_RemovesDeadFiles(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)

	socketPath, err := DaemonSocketPath()
	if err != nil {
		t.Fatalf("DaemonSocketPath: %v", err)
	}
	// A plain file at the socket path stands in for a SIGKILLed daemon's
	// leftover socket: dialing it fails, so nothing answers the ping.
	if err := os.WriteFile(socketPath, nil, 0600); err != nil {
		t.Fatalf("write stale socket file: %v", err)
	}
	pidFile := filepath.Join(tmpHome, "daemon.pid")
	if err := os.WriteFile(pidFile, []byte("12345"), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	cleanupDaemonRuntimeFiles(pidFile)

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale socket file to be removed, stat err = %v", err)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("expected stale PID file to be removed, stat err = %v", err)
	}
}

// TestBindControlServerExclusive_ExactlyOneBinds closes out the residual of
// #718 left after #791's RunDaemon ping-guard: two daemons could both pass
// the ping (neither bound yet) and then both bind, with the second unlinking
// and stealing the first's socket. The spawn flock makes ping→bind atomic, so
// exactly one spawner binds and the other observes the winner and reports
// alreadyRunning with no error (RunDaemon then exits 0 — no orphan, no steal).
//
// The flock is taken on separate file descriptors, so two goroutines in one
// process contend on it exactly like two processes would.
func TestBindControlServerExclusive_ExactlyOneBinds(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)

	// Hold the winner inside the ping→bind window so the loser provably
	// arrives while the window is open. Without the lock the loser would
	// pass its own ping during this sleep and steal the socket.
	prevHook := testHookSpawnPingPassed
	testHookSpawnPingPassed = func() { time.Sleep(150 * time.Millisecond) }
	t.Cleanup(func() { testHookSpawnPingPassed = prevHook })

	type result struct {
		closeFn        func() error
		alreadyRunning bool
		err            error
	}
	results := make([]result, 2)

	// Build the managers up front: NewManager can take the better part of a
	// second, and constructing it inside the goroutines would skew the two
	// spawners apart by more than the race window, letting the loser's ping
	// trivially observe the winner even without the lock. The race under test
	// is ping→bind, so only that part runs concurrently, released by a
	// barrier.
	managers := make([]*Manager, len(results))
	for i := range managers {
		manager, err := NewManager(config.DefaultConfig())
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		managers[i] = manager
	}

	startBarrier := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-startBarrier
			closeFn, alreadyRunning, err := bindControlServerExclusive(managers[i], nil, nil)
			results[i] = result{closeFn: closeFn, alreadyRunning: alreadyRunning, err: err}
		}(i)
	}
	close(startBarrier)
	wg.Wait()

	var winners, losers int
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("spawner %d returned error: %v", i, r.err)
		}
		switch {
		case r.closeFn != nil && !r.alreadyRunning:
			winners++
			t.Cleanup(func() { _ = r.closeFn() })
		case r.closeFn == nil && r.alreadyRunning:
			losers++
		default:
			t.Fatalf("spawner %d in inconsistent state: closeFn=%v alreadyRunning=%v", i, r.closeFn != nil, r.alreadyRunning)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("expected exactly one spawner to bind and one to defer; got %d winners, %d losers", winners, losers)
	}

	// The surviving socket must belong to the winner and still answer — a
	// steal would leave an unlinked listener and a path nothing serves.
	if err := pingDaemon(); err != nil {
		t.Fatalf("control socket does not answer after concurrent spawn: %v", err)
	}
}

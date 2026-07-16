package daemon

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestAcquireHomeLock_SecondFailsFastAndFreesOnRelease pins the core singleton
// invariant: only one holder at a time. A second acquire on the same home fails
// fast with the "already running" *daemonLockHeldError (never blocks), and the
// lock frees the instant the first holder releases.
func TestAcquireHomeLock_SecondFailsFastAndFreesOnRelease(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	first, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("first acquireHomeLock: %v", err)
	}

	// A second acquire while the first is held must fail fast (LOCK_NB) with the
	// typed held error. flock is per open-file-description, so two acquires in one
	// process contend exactly like two processes (see #718 bind test).
	start := time.Now()
	second, err := acquireHomeLock()
	if err == nil {
		second.release()
		t.Fatal("second acquireHomeLock succeeded while the first was held; expected it to fail")
	}
	if !isDaemonLockHeldErr(err) {
		t.Fatalf("second acquireHomeLock returned %v; want a *daemonLockHeldError", err)
	}
	if !strings.Contains(err.Error(), "already running for this home") {
		t.Fatalf("held error message %q missing the clear 'already running for this home' text", err.Error())
	}
	if time.Since(start) > time.Second {
		t.Fatalf("second acquireHomeLock blocked for %s; must be non-blocking", time.Since(start))
	}

	// Release the first holder; the lock must now be free.
	first.release()
	third, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("acquireHomeLock after release: %v; lock did not free", err)
	}
	third.release()
}

// TestRunDaemon_SecondStartFailsFastWithoutTouchingRuntimeFiles simulates a
// live daemon A (holds the lock) and proves a second RunDaemon on the same home
// fails fast with the clear error and does NOT clobber A's socket or PID file —
// the split-brain fix (#960). Holding the lock in-process stands in for daemon
// A: flock contends across open-file-descriptions the same in one process as
// across two.
func TestRunDaemon_SecondStartFailsFastWithoutTouchingRuntimeFiles(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// Daemon A holds the lock and owns its runtime files.
	lockA, err := acquireHomeLock()
	if err != nil {
		t.Fatalf("daemon A acquireHomeLock: %v", err)
	}
	defer lockA.release()

	socketPath, err := DaemonSocketPath()
	if err != nil {
		t.Fatalf("DaemonSocketPath: %v", err)
	}
	// Sentinels standing in for A's live runtime files. The socket is a plain
	// file (not a listener) so the pre-lock ping in RunDaemon fails and it
	// proceeds to the lock guard, exactly as a second start would when A's real
	// listener momentarily does not answer.
	const socketSentinel = "A-socket"
	if err := os.WriteFile(socketPath, []byte(socketSentinel), 0600); err != nil {
		t.Fatalf("write sentinel socket: %v", err)
	}
	pidPath := filepath.Join(home, "daemon.pid")
	if err := os.WriteFile(pidPath, []byte("424242"), 0600); err != nil {
		t.Fatalf("write sentinel PID file: %v", err)
	}

	// Second start.
	runErr := RunDaemon(config.DefaultConfig())
	if runErr == nil {
		t.Fatal("second RunDaemon returned nil; expected it to refuse to start")
	}
	if !isDaemonLockHeldErr(runErr) {
		t.Fatalf("second RunDaemon returned %v; want a *daemonLockHeldError", runErr)
	}
	if !strings.Contains(runErr.Error(), "424242") {
		t.Fatalf("held error %q should name the holder pid from the PID file", runErr.Error())
	}

	// A's runtime files must be untouched.
	gotSocket, err := os.ReadFile(socketPath)
	if err != nil {
		t.Fatalf("read socket after refused start: %v", err)
	}
	if string(gotSocket) != socketSentinel {
		t.Fatalf("second start clobbered the socket file: got %q want %q", gotSocket, socketSentinel)
	}
	gotPID, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read PID file after refused start: %v", err)
	}
	if string(gotPID) != "424242" {
		t.Fatalf("second start rewrote the PID file: got %q want %q", gotPID, "424242")
	}
}

// TestHomeLock_FreesAfterHolderKilledDashNine proves the auto-release guarantee
// across a real process boundary: a child process acquires the lock, is SIGKILLed
// (kill -9, no chance to clean up), and the lock is then free for a new start —
// no stale-lock pid guessing needed. This is the crash-recovery path.
func TestHomeLock_FreesAfterHolderKilledDashNine(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// Spawn a child (this test binary re-invoked) that acquires the lock, writes
	// its PID, prints READY, and blocks until killed. The child's own TestMain
	// re-sandboxes AGENT_FACTORY_HOME, so we hand it this home explicitly via
	// AF_HELPER_HOME and it re-points at it before acquiring.
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperHoldsHomeLock")
	cmd.Env = append(os.Environ(), "AF_HELPER_HOLD_LOCK=1", "AF_HELPER_HOME="+home)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Wait for the child to signal it holds the lock.
	scanner := bufio.NewScanner(stdout)
	ready := make(chan struct{})
	go func() {
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == "READY" {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("helper never signaled it holds the lock")
	}

	// While the child holds it, our acquire must fail with the held error.
	if _, err := acquireHomeLock(); !isDaemonLockHeldErr(err) {
		t.Fatalf("acquireHomeLock while child holds it = %v; want *daemonLockHeldError", err)
	}

	// kill -9: the child gets no chance to release the lock explicitly.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL helper: %v", err)
	}
	_, _ = cmd.Process.Wait()

	// The kernel releases the flock on process death. A new acquire must succeed
	// within a short bound (teardown is not perfectly instantaneous).
	deadline := time.Now().Add(3 * time.Second)
	for {
		lock, err := acquireHomeLock()
		if err == nil {
			lock.release()
			return
		}
		if !isDaemonLockHeldErr(err) {
			t.Fatalf("acquireHomeLock after kill -9: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("lock never freed after the holder was SIGKILLed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// helperLockHold keeps the child process's lock reachable for its whole life so
// the GC never finalizes the *os.File and closes the fd (which would release the
// flock early). A package var is always reachable.
var helperLockHold *homeLock

// TestHelperHoldsHomeLock is not a real test: it is the child process spawned by
// TestHomeLock_FreesAfterHolderKilledDashNine. Guarded by an env var so it is a
// no-op under a normal `go test` run.
func TestHelperHoldsHomeLock(t *testing.T) {
	if os.Getenv("AF_HELPER_HOLD_LOCK") != "1" {
		return
	}
	// The package TestMain sandboxed AGENT_FACTORY_HOME to a throwaway dir;
	// re-point at the home the parent handed us so we lock the same file.
	if h := os.Getenv("AF_HELPER_HOME"); h != "" {
		_ = os.Setenv("AGENT_FACTORY_HOME", h)
	}
	lock, err := acquireHomeLock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper acquireHomeLock: %v\n", err)
		os.Exit(1)
	}
	helperLockHold = lock
	// Write our PID so the parent's held-error message can name us, mirroring a
	// real daemon writing its PID file after acquiring the lock.
	dir, err := config.GetConfigDir()
	if err != nil {
		os.Exit(1)
	}
	_ = os.WriteFile(filepath.Join(dir, "daemon.pid"), []byte(strconv.Itoa(os.Getpid())), 0600)
	fmt.Println("READY")
	// Block until the parent kills us.
	select {}
}

// TestEnsureDaemon_NoSpawnWhenLiveDaemonServes covers the common case: a live
// daemon is already serving the socket, so EnsureDaemon returns immediately
// without launching a second one.
func TestEnsureDaemon_NoSpawnWhenLiveDaemonServes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	startTestControlServer(t)
	if err := pingDaemon(); err != nil {
		t.Fatalf("control server not answering: %v", err)
	}

	launched := false
	if err := ensureDaemonWithLauncher(func() error {
		launched = true
		return nil
	}); err != nil {
		t.Fatalf("ensureDaemonWithLauncher against a live daemon: %v", err)
	}
	if launched {
		t.Fatal("EnsureDaemon launched a second daemon while one was already serving")
	}
}

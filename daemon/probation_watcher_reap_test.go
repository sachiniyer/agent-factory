package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

// TestRunDaemonHandoffPendingExitReapsWatchers is the regression guard for the
// upgrade-handoff watcher subprocess leak. A released upgrade candidate
// transitions to DaemonPhaseHandoffPending while still parked in runDaemon's
// probation select; in that window it admits task-mutating RPCs, and an
// admitted ReloadTasks arms a real watcher subprocess via
// watchers.reconcile -> go w.run() -> cmd.Start() ($SHELL -c watch_cmd,
// Setpgid). The select exits via `return nil` on Shutdown, so the deferred
// watchers.Stop() must run on that exit path too — otherwise the reliable
// SIGTERM/SIGKILL group teardown that reaps those subprocesses is skipped and
// they leak, undiscoverable by any startup sweep. Drives the whole path
// through the real control socket like TestRunDaemonUpgradeProbationEndToEnd.
func TestRunDaemonHandoffPendingExitReapsWatchers(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// Seed tasks.json with one enabled watch task whose command records its
	// shell PID (== process-group leader, because the watcher launches it with
	// Setpgid) and then blocks without writing to stdout. Empty TargetSession
	// and ProjectPath so persistedTasksForArming admits it as safe (no target
	// validation runs) and so no watch event is ever delivered — the watcher
	// subprocess simply persists until watchers.Stop() reaps it.
	pidFile := filepath.Join(home, "watcher.pid")
	watchCmd := "echo $$ > " + pidFile + "; sleep 300"
	require.NoError(t, task.AddTask(task.Task{
		ID:       "leak-handoff-001",
		Name:     "leak-handoff-001",
		WatchCmd: watchCmd,
		Enabled:  true,
	}))

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "" // AF-owned Unix sockets only, like TestRunDaemonUpgradeProbationEndToEnd.
	const transactionID = "txn-handoff-leak"
	runDone := make(chan error, 1)
	go func() { runDone <- runDaemon(cfg, transactionID) }()
	stopped := false
	defer func() {
		if stopped {
			return
		}
		var resp ShutdownResponse
		_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Errorf("probationary daemon did not stop during cleanup")
		}
	}()

	// Park in the probation select.
	probation := waitForDaemonPhase(t, DaemonPhaseUpgradeProbation)
	require.Equal(t, transactionID, probation.TransactionID)

	// Release -> HandoffPending: mutation admission opens while the daemon
	// stays parked in the same select (release mutates fields under a mutex; it
	// does not signal the select).
	var release ReleaseUpgradeProbationResponse
	require.NoError(t, callDaemonNoEnsure("ReleaseUpgradeProbation",
		ReleaseUpgradeProbationRequest{TransactionID: transactionID}, &release))
	handoff := waitForDaemonPhase(t, DaemonPhaseHandoffPending)
	require.Equal(t, transactionID, handoff.TransactionID)

	// An admitted ReloadTasks re-arms every enabled watch task, spawning the
	// watcher subprocess for the seeded task via watchers.reconcile.
	var reload ReloadTasksResponse
	require.NoError(t, callDaemonNoEnsure("ReloadTasks", ReloadTasksRequest{}, &reload))
	require.True(t, reload.OK)

	// Wait for the watcher subprocess to record its PID (echo runs first). This
	// is the would-be leak: a real $SHELL -c watch_cmd in its own process group.
	pid := waitForWatcherPID(t, pidFile)
	require.NotZero(t, pid, "watcher subprocess never spawned")
	require.True(t, processAlive(pid), "watcher subprocess should be alive while the daemon is parked")
	t.Cleanup(func() {
		// CI hygiene: if the test fails before the daemon reaps it, kill the
		// group. Guarded by liveness so a reaped/reused PID is never targeted —
		// a process group's ID cannot be reused while its leader (the shell,
		// whose PID we captured) is still alive.
		if processAlive(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})

	// Shutdown signals shutdownCh; the probation select returns nil. The fix
	// ensures the hoisted defer watchers.Stop() runs on this path, SIGTERM/SIGKILL
	// the watcher's process group, and reaps it before closeControl closes the
	// socket (LIFO) and the daemon exits.
	var shutdown ShutdownResponse
	require.NoError(t, callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &shutdown))
	select {
	case err := <-runDone:
		require.NoError(t, err)
		stopped = true
	case <-time.After(10 * time.Second):
		t.Fatal("probationary daemon did not stop after Shutdown")
	}

	// The watcher subprocess must be reaped. Before the fix this stayed alive
	// because defer watchers.Stop() was registered AFTER the probation select
	// and was skipped on the return-nil exit, leaving the subprocess orphaned.
	require.False(t, processAlive(pid), "watcher subprocess leaked: watchers.Stop() did not run on probation-exit path")
}

// waitForWatcherPID polls pidFile for the watcher shell's PID until it appears
// or the deadline elapses, returning 0 on timeout. Mirrors the pid-file poll
// idiom used elsewhere in the package.
func waitForWatcherPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && p > 0 {
				return p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0
}

// TestRunDaemonHandoffPendingSignalExitReapsWatchers is the symmetric partner to
// TestRunDaemonHandoffPendingExitReapsWatchers: it exercises the other probation-
// select exit arm, `case sig := <-sigChan`, by sending SIGTERM directly to the
// parked daemon's PID (read from daemon.pid, which bindControlServerExclusive
// publishes before the probation select). The hoisted defer watchers.Stop() must
// run on this return-nil path too, reaping the armed watcher subprocess. This
// directly proves behavioral guarantee #2 (signal-path reap).
func TestRunDaemonHandoffPendingSignalExitReapsWatchers(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	pidFile := filepath.Join(home, "watcher.pid")
	watchCmd := "echo $$ > " + pidFile + "; sleep 300"
	require.NoError(t, task.AddTask(task.Task{
		ID:       "leak-signal-001",
		Name:     "leak-signal-001",
		WatchCmd: watchCmd,
		Enabled:  true,
	}))

	cfg := config.DefaultConfig()
	cfg.ListenAddr = ""
	const transactionID = "txn-handoff-signal"
	runDone := make(chan error, 1)
	go func() { runDone <- runDaemon(cfg, transactionID) }()
	stopped := false
	defer func() {
		if stopped {
			return
		}
		// If the test bails before the signal is sent, shut the daemon via RPC
		// so it does not leak. SIGTERM-the-PID would also work, but the RPC path
		// is the one already regression-tested above.
		var resp ShutdownResponse
		_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Errorf("probationary daemon did not stop during cleanup")
		}
	}()

	probation := waitForDaemonPhase(t, DaemonPhaseUpgradeProbation)
	require.Equal(t, transactionID, probation.TransactionID)

	// daemon.pid is published inside bindControlServerExclusive, before the
	// probation select, so it is already present here.
	daemonPID := waitForDaemonPID(t, home)
	require.NotZero(t, daemonPID, "daemon.pid was not published before the probation select")

	var release ReleaseUpgradeProbationResponse
	require.NoError(t, callDaemonNoEnsure("ReleaseUpgradeProbation",
		ReleaseUpgradeProbationRequest{TransactionID: transactionID}, &release))
	handoff := waitForDaemonPhase(t, DaemonPhaseHandoffPending)
	require.Equal(t, transactionID, handoff.TransactionID)

	var reload ReloadTasksResponse
	require.NoError(t, callDaemonNoEnsure("ReloadTasks", ReloadTasksRequest{}, &reload))
	require.True(t, reload.OK)

	watcherPID := waitForWatcherPID(t, pidFile)
	require.NotZero(t, watcherPID, "watcher subprocess never spawned")
	require.True(t, processAlive(watcherPID), "watcher subprocess should be alive while the daemon is parked")
	t.Cleanup(func() {
		if processAlive(watcherPID) {
			_ = syscall.Kill(-watcherPID, syscall.SIGKILL)
		}
	})

	// SIGTERM the parked daemon directly so the probation select takes the
	// `case sig := <-sigChan` arm → return nil. The fix ensures the hoisted
	// defer watchers.Stop() runs on this path too.
	require.NoError(t, syscall.Kill(daemonPID, syscall.SIGTERM))
	select {
	case err := <-runDone:
		require.NoError(t, err)
		stopped = true
	case <-time.After(10 * time.Second):
		t.Fatal("probationary daemon did not stop after SIGTERM")
	}

	require.False(t, processAlive(watcherPID), "watcher subprocess leaked: watchers.Stop() did not run on signal probation-exit path")
}

// waitForDaemonPID polls <home>/daemon.pid for the daemon's PID until it
// appears or the deadline elapses, returning 0 on timeout.
func waitForDaemonPID(t *testing.T, home string) int {
	t.Helper()
	daemonPIDFile := filepath.Join(home, "daemon.pid")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(daemonPIDFile); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && p > 0 {
				return p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0
}

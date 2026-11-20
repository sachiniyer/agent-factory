package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
)

// This file guards the SIGKILL escalation branch of stopDaemonUntil (process.go)
// and the recovery stop that turns its outcome into StopConfirmed.
//
// SIGKILL cannot be trapped, so a process that receives it either dies or — if a
// thread is pinned in an uninterruptible syscall (hung NFS/FUSE I/O) — survives
// until the wedge clears, all the while holding the per-home flock. Before the
// fix, StopDaemon sent SIGKILL and fell straight through to "stopped successfully"
// without re-observing the exit: a wedged daemon read as stopped, and the
// recovery actor's WaitForShutdownCompletion(ShutdownTarget{}) (PID==0)
// confirmed it by socket-quiet, fabricating StopConfirmed while the lock was
// still held. The supervisor then spawned a candidate that failed
// acquireHomeLock and rolled back — sometimes all the way to the durable
// rollback_failed circuit breaker — from a wedge that lasted only tens of
// seconds.
//
// The "death blocked after SIGKILL" shape cannot be simulated with a real
// process (SIGKILL is untrappable), so both tests stub proctree.Kill to a no-op:
// proctree.Signal still revalidates the instance and returns nil (the signal
// "succeeded"), but the process never receives it. The SIGTERM is a no-op too,
// but the fake daemon traps SIGTERM anyway, so the observable behavior matches.
// This is the same seam sigterm_fallback_signalfailure_test.go uses; restored on
// cleanup, and the daemon package's tests do not call t.Parallel, so the global
// override is safe.

// stopDaemonTestGraces shortens the SIGTERM grace, the SIGKILL exit grace, and
// the poll cadence for the duration of a test and restores them on cleanup, so
// a wedge-driven test returns in ~300ms instead of ~10s.
func stopDaemonTestGraces(t *testing.T) {
	t.Helper()
	origGrace := stopDaemonGrace
	origKillGrace := stopDaemonKillGrace
	origPoll := stopDaemonPoll
	stopDaemonGrace = 150 * time.Millisecond
	stopDaemonKillGrace = 150 * time.Millisecond
	stopDaemonPoll = 25 * time.Millisecond
	t.Cleanup(func() {
		stopDaemonGrace = origGrace
		stopDaemonKillGrace = origKillGrace
		stopDaemonPoll = origPoll
	})
}

// injectNoOpKill stubs proctree.Kill to a no-op that returns nil for the
// duration of the test: proctree.Signal revalidates the instance and returns
// success, but the signal never reaches the process — modeling a SIGKILL that
// does not take effect (a thread pinned in uninterruptible I/O). Restored on
// cleanup so the global var never leaks to neighboring tests.
func injectNoOpKill(t *testing.T) {
	t.Helper()
	orig := proctree.Kill
	t.Cleanup(func() { proctree.Kill = orig })
	proctree.Kill = func(int, syscall.Signal) error { return nil }
}

// spawnWedgedFakeDaemon spawns a fake agent-factory daemon (argv[0]=="af" with a
// discrete "--daemon" token, AGENT_FACTORY_HOME bound to tmpHome) that ignores
// SIGTERM and spins, returning its PID once it is provably THIS home's daemon
// under classifyDaemonHome — the same readiness contract
// TestStopDaemon_EscalatesToSIGKILL uses. The spinner can only be removed by the
// SIGKILL escalation, which the caller arranges to have no effect via
// injectNoOpKill. testguard's group cleanup (real syscall.Kill, not the stubbed
// proctree.Kill) reaps it at test end.
func spawnWedgedFakeDaemon(t *testing.T, tmpHome string) int {
	t.Helper()
	readyFile := filepath.Join(tmpHome, "trap-ready")
	script := fmt.Sprintf(`trap "" TERM; : > %s; %s`, readyFile,
		testguard.BoundedSpin(time.Second, 5*time.Minute))
	cmd := spawnFakeDaemonHomeProc(t, "af", script, "--daemon")
	pid := cmd.Process.Pid
	waitForReady(t, fmt.Sprintf("trap-ready sentinel + pid=%d cmdline exposes --daemon", pid), func() bool {
		if _, err := os.Stat(readyFile); err != nil {
			return false
		}
		return isAgentFactoryDaemon(pid)
	})
	return pid
}

// TestStopDaemon_SigkillReobservesExit_FailsClosedWhenWedged verifies the SIGKILL
// escalation branch re-observes the daemon's actual exit before reporting
// success, the way the SIGTERM grace poll already does. With proctree.Kill
// stubbed to a no-op, the SIGKILL is "sent" but never takes effect: a wedged
// daemon stays alive and keeps holding the per-home flock. StopDaemon must
// detect that within stopDaemonKillGrace and return an error instead of
// claiming success, NOT run cleanupDaemonRuntimeFiles (the live daemon's PID
// file must remain), and leave the process untouched. Companion to
// TestStopDaemon_EscalatesToSIGKILL, which covers the normal case where SIGKILL
// does take effect and StopDaemon returns (true, nil).
func TestStopDaemon_SigkillReobservesExit_FailsClosedWhenWedged(t *testing.T) {
	tmpHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)
	stopDaemonTestGraces(t)

	pid := spawnWedgedFakeDaemon(t, tmpHome)
	injectNoOpKill(t)

	pidFile := filepath.Join(tmpHome, "daemon.pid")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", pid)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	stopped, err := StopDaemon()

	// The process is still alive, so StopDaemon must NOT report success: the
	// bool+nil pair is "a daemon was signaled AND the stop was confirmed", and
	// here it was not. The error propagates as StopUnknown to the recovery
	// actor (fail-closed), never a fabricated StopConfirmed.
	if err == nil {
		t.Fatalf("StopDaemon returned nil error after SIGKILL did not take effect; expected a failure rather than a fabricated success while the daemon still holds the home lock")
	}
	if stopped {
		t.Fatalf("StopDaemon reported stopped=true after SIGKILL did not take effect; the SIGKILL branch must not claim a successful stop without re-observing the exit")
	}
	if !strings.Contains(err.Error(), "SIGKILL") {
		t.Fatalf("StopDaemon error %q does not mention SIGKILL; expected the failure to identify the escalation that did not complete", err.Error())
	}

	// The wedged daemon is still alive and still answers signal 0. Confirming
	// this is the load-bearing assertion: had StopDaemon fallen through to
	// "stopped successfully", the recovery actor would fabricate StopConfirmed
	// over this very process.
	if !pidLooksAlive(pid) {
		t.Fatalf("daemon pid %d is not alive after a no-op SIGKILL; the test's wedge simulation broke and the assertion that StopDaemon did not kill it is no longer load-bearing", pid)
	}

	// A stop that could not prove the exit must not unlink a live daemon's
	// runtime files: a successful stop would run cleanupDaemonRuntimeFiles, but
	// the error path returns before it, so the PID file must still name this
	// process. (cleanupDaemonRuntimeFiles would in any case keep a daemonOurs
	// PID file via removePIDFileIfStillNames, but reaching cleanup at all on an
	// unconfirmed stop is the contract violation this test guards.)
	data, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatalf("PID file was removed after a failed stop; a stop that did not prove the exit must not unlink a live daemon's runtime files: %v", rerr)
	}
	if got := strings.TrimSpace(string(data)); got != fmt.Sprintf("%d", pid) {
		t.Fatalf("PID file no longer names the live daemon (got %q, want %d); cleanup ran despite the unconfirmed stop", got, pid)
	}
}

// TestStopDaemonForRecovery_WedgedDaemonFailsClosedNotConfirmed is the
// end-to-end regression the bug report identifies as missing: it drives the
// REAL recovery stop (stopDaemonForRecovery with the real StopDaemon and
// WaitForShutdownCompletion seams) against a wedged daemon holding this home's
// PID file, and asserts it returns StopUnknown — NOT StopConfirmed. Pre-fix,
// StopDaemon reported success over the wedged process and
// WaitForShutdownCompletion(ShutdownTarget{}) confirmed it by socket-quiet
// (no control socket exists for the fake), fabricating StopConfirmed; the
// supervisor would then spawn a candidate that fails acquireHomeLock. Post-fix,
// StopDaemon's SIGKILL branch re-observes the exit and returns an error, so
// stopDaemonForRecovery short-circuits to StopUnknown before the socket-quiet
// wait runs — the destructive StopConfirmed is no longer fabricated over a
// daemon that still holds the home lock.
func TestStopDaemonForRecovery_WedgedDaemonFailsClosedNotConfirmed(t *testing.T) {
	tmpHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)
	stopDaemonTestGraces(t)

	pid := spawnWedgedFakeDaemon(t, tmpHome)
	injectNoOpKill(t)

	pidFile := filepath.Join(tmpHome, "daemon.pid")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", pid)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	// Restore the recovery-stop seams to their production defaults for the
	// duration of this test so it drives the REAL StopDaemon +
	// WaitForShutdownCompletion — the path the bug report says was never
	// exercised against a live (wedged) daemon. The daemon package's tests do
	// not run in parallel, and the save/restore makes the test robust to
	// ordering against the stubbing tests.
	origStop := stopDaemonFn
	origWait := waitForShutdownFn
	t.Cleanup(func() {
		stopDaemonFn = origStop
		waitForShutdownFn = origWait
	})
	stopDaemonFn = StopDaemon
	waitForShutdownFn = WaitForShutdownCompletion

	journal := upgradetxn.Journal{HomeDir: tmpHome, FromVersion: "1.0.100"}

	outcome, err := stopDaemonForRecovery(journal, "previous")
	if outcome != upgradetxn.StopUnknown {
		t.Fatalf("a wedged daemon still holding the home lock must fail closed as StopUnknown, not fabricate %v; err=%v", outcome, err)
	}
	if err == nil {
		t.Fatalf("stopDaemonForRecovery returned StopUnknown with nil error; the unconfirmed stop must surface an error so the supervisor does not advance")
	}

	// The wedged daemon is still alive — exactly the state under which the
	// pre-fix code fabricated StopConfirmed. The destructive hand-off must not
	// proceed over it.
	if !pidLooksAlive(pid) {
		t.Fatalf("daemon pid %d is not alive after the recovery stop; the wedge simulation broke and the StopUnknown assertion is no longer load-bearing", pid)
	}
}

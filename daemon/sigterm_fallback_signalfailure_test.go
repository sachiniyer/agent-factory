package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// This file isolates the signal-FAILURE branches of the SIGTERM fallback:
// the cases where locateDaemonPID returned a proven-ours PID but
// signalClassifiedDaemon then returned a non-identity error (e.g. EPERM from
// a MAC policy). The recovery hint sigtermFallback surfaces in that window is
// governed by the error-text-broadness invariant: it must not recommend a
// host-wide `pkill -f -- '--daemon'` when foreign-home daemons may be alive on
// the host, because that command has no home or PID constraint and would kill
// exactly the daemons the home binding is designed to protect. These tests
// live apart from sigterm_fallback_test.go to keep that file under the
// file-length lint (#1145).

// injectSignalFailure stubs the proctree kill syscall for the duration of the
// test so every Signal call returns EPERM — modeling a MAC policy (SELinux,
// AppArmor, or the macOS sandbox) that permits the /proc reads the home binding
// performs but denies a same-uid kill. It is restored on cleanup. This is the
// seam TestSignalPropagatesNonESRCHErrors (internal/proctree/proctree_test.go)
// uses; it is exported so the daemon package's SIGTERM-fallback tests can
// reach it without duplicating an os-specific syscall override.
func injectSignalFailure(t *testing.T) {
	t.Helper()
	orig := proctree.Kill
	t.Cleanup(func() { proctree.Kill = orig })
	proctree.Kill = func(pid int, sig syscall.Signal) error { return syscall.EPERM }
}

// TestSigtermFallback_PIDFileOwnHomeSignalFailureDoesNotRecommendBlanketPkill
// is the regression for the error-text-broadness invariant on the PID-file
// fast path. The fast path (locateDaemonPID's daemonOurs arm) returns scanned
// = 0: it proved the PID-file PID serves THIS home and short-circuited before
// the pgrep scan ran, so it has no knowledge of whether other `--daemon`
// processes — including daemons serving foreign homes on a shared host — are
// alive. When signalling that proven-ours PID then fails with a non-identity
// error (here EPERM injected via the proctree.Kill seam), pre-fix
// sigtermFallback saw scanned == 0, fell past the `scanned > 1` guard, and
// recommended the blanket `pkill -f -- '--daemon'` — a command with no home
// or PID constraint, so following it would kill exactly the foreign-home
// daemons the home binding never had a chance to leave untouched. The fix
// adds a `scanned == 0` arm that carries the same scoped recovery the other
// branches use: stop the daemon serving THIS home by its PID, never a
// host-wide pattern.
func TestSigtermFallback_PIDFileOwnHomeSignalFailureDoesNotRecommendBlanketPkill(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	ours := spawnFakeDaemonWithHome(t, home)
	if !pidBelongsToThisHome(ours) {
		t.Fatalf("setup invariant: this home's fake daemon pid=%d is not bound to this home", ours)
	}
	if err := os.WriteFile(filepath.Join(home, "daemon.pid"),
		[]byte(strconv.Itoa(ours)), 0600); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	// Stub the host-wide scan to nothing: the fast path must short-circuit
	// before the scan runs (its scanned = 0 is the whole point of this test),
	// and an unstubbed scan could SIGTERM the host's real daemon (#793).
	stubDaemonScan(t, nil, nil)

	injectSignalFailure(t)

	result, _, err := sigtermFallback()
	if result != ShutdownFailed {
		t.Fatalf("sigtermFallback returned %v, want ShutdownFailed (signal failed with EPERM)", result)
	}
	if err == nil {
		t.Fatalf("sigtermFallback returned nil error; expected a recovery hint")
	}
	if strings.Contains(err.Error(), "pkill -f -- '--daemon'") {
		t.Errorf("sigtermFallback error %q recommends a blanket `pkill -f -- '--daemon'` after a "+
			"fast-path signal failure (scanned=0: the fast path never scanned, so it has no idea "+
			"whether foreign-home daemons exist on the host); following it would kill the very "+
			"foreign daemons the home binding is designed to protect", err.Error())
	}
	// EPERM means the signal never landed, so the fake daemon must still be
	// alive — the scoped error did not kill anything, and neither should
	// sigtermFallback.
	if !pidLooksAlive(ours) {
		t.Fatalf("this home's daemon pid=%d was killed despite the signal failing with EPERM; "+
			"the scoped recovery must not terminate the target", ours)
	}
}

// TestSigtermFallback_ScanSingleOwnHomeSignalFailureStillRecommendsPkill guards
// the genuinely-safe half the fix deliberately leaves alone: when the slow path
// finds EXACTLY one `--daemon` candidate (proven ours) and no rejected PID-file
// candidate inflated the count, scanned == 1, and a host-wide
// `pkill -f -- '--daemon'` can only match that one daemon — so recommending it
// on a signal failure is safe. The new `scanned == 0` arm must NOT broaden to
// swallow this case; if it did, the only-ours-daemon host would lose its
// actionable recovery hint while gaining nothing (no foreign daemon exists to
// protect). This test pins scanned == 1 -> blanket pkill so the fix stays
// scoped to the fast path.
func TestSigtermFallback_ScanSingleOwnHomeSignalFailureStillRecommendsPkill(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("scoping by AF home needs /proc")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	ours := spawnFakeDaemonWithHome(t, home)
	// No PID file: the fast path does not run, and the slow path's case 1
	// arm finds the one proven-ours daemon with scanned = 1 (no rejected
	// PID-file candidate to inflate the count).
	if _, err := os.Stat(filepath.Join(home, "daemon.pid")); !os.IsNotExist(err) {
		t.Fatalf("PID file pre-exists; test setup must start without one")
	}
	stubDaemonScan(t, []int{ours}, nil)
	injectSignalFailure(t)

	result, _, err := sigtermFallback()
	if result != ShutdownFailed {
		t.Fatalf("sigtermFallback returned %v, want ShutdownFailed (signal failed with EPERM)", result)
	}
	if err == nil {
		t.Fatalf("sigtermFallback returned nil error; expected the recovery hint")
	}
	// With exactly one --daemon process on the host (ours) and no rejected
	// PID-file candidate, scanned == 1, so a host-wide pkill can ONLY match
	// this daemon. Recommending it here is genuinely safe and MUST survive
	// the fast-path fix: silencing the pkill hint in this case would leave
	// the operator with no remedy on the only-ours-daemon host while
	// protecting nothing.
	if !strings.Contains(err.Error(), "pkill -f -- '--daemon'") {
		t.Errorf("sigtermFallback error %q drops the blanket pkill hint on the slow-path "+
			"scanned==1 case (exactly one proven-ours --daemon and no rejected PID-file "+
			"candidate); on an only-ours-daemon host a host-wide pkill is genuinely safe, "+
			"so the fix's scanned==0 arm must not over-broaden to swallow it", err.Error())
	}
}

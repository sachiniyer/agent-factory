package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
)

// stubEntrypointGate replaces the gate probe with a fixed outcome and restores
// it on cleanup. It lets the fail-open classification be tested against every
// EntrypointGate.Check result without building each on-disk journal state.
func stubEntrypointGate(t *testing.T, fn func(context.Context, string, bool) error) {
	t.Helper()
	prev := runEntrypointGate
	t.Cleanup(func() { runEntrypointGate = prev })
	runEntrypointGate = fn
}

// The classification is the whole safety contract (#2212 R1): only a provably
// live upgrade stops a launch; every other outcome proceeds, because the gate
// fronts every af invocation and a hang there is unrecoverable.

func TestUpgradeGate_NoTransaction_Proceeds(t *testing.T) {
	stubEntrypointGate(t, func(context.Context, string, bool) error { return nil })
	decision, err := checkUpgradeGate("/home/af", false)
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("no transaction must proceed with no error; got decision=%v err=%v", decision, err)
	}
}

func TestUpgradeGate_LiveForwardUpgrade_ReportsInProgress(t *testing.T) {
	inProgress := &upgradetxn.UpgradeInProgressError{
		TransactionID: "txn-1", ToVersion: "1.0.210", Phase: upgradetxn.PhaseCandidateValidating,
		Deadline: time.Now().Add(time.Minute),
	}
	stubEntrypointGate(t, func(context.Context, string, bool) error { return inProgress })

	decision, err := checkUpgradeGate("/home/af", false)
	if decision != upgradeGateInProgress {
		t.Fatalf("a live forward upgrade must stop the launch; got decision=%v", decision)
	}
	var typed *upgradetxn.UpgradeInProgressError
	if !errors.As(err, &typed) {
		t.Fatalf("in-progress must return the typed retryable error; got %v", err)
	}
}

// The P1 regression: a live actor restoring the previous daemon during a
// rollback must classify as restoring-previous, so the daemon-BIND path binds
// rather than deferring to the actor that is waiting for it. Every
// rollback-restore phase must map this way.
func TestUpgradeGate_LiveRollbackRestore_ReportsRestoringPrevious(t *testing.T) {
	for _, phase := range []upgradetxn.Phase{
		upgradetxn.PhaseRollbackRestored,
		upgradetxn.PhasePreviousStarting,
		upgradetxn.PhasePreviousValidating,
		upgradetxn.PhaseRolledBack,
	} {
		t.Run(string(phase), func(t *testing.T) {
			inProgress := &upgradetxn.UpgradeInProgressError{
				TransactionID: "txn-2", ToVersion: "1.0.210", Phase: phase,
				Deadline: time.Now().Add(time.Minute),
			}
			stubEntrypointGate(t, func(context.Context, string, bool) error { return inProgress })

			decision, _ := checkUpgradeGate("/home/af", true)
			if decision != upgradeGateRestoringPrevious {
				t.Fatalf("phase %s must let the previous daemon bind; got decision=%v", phase, decision)
			}
		})
	}
}

// A stale journal whose actor never re-takes the recovery lock surfaces as a
// blocked-recovery result. It must PROCEED — the launch cannot wait forever on a
// dead actor.
func TestUpgradeGate_StaleTakeoverBlocked_Proceeds(t *testing.T) {
	blocked := &upgradetxn.UpgradeRecoveryBlockedError{
		TransactionID: "txn-3", ToVersion: "1.0.210", Phase: upgradetxn.PhaseDaemonStopping,
		Reason: "the preserved previous binary did not acquire the recovery lock",
	}
	stubEntrypointGate(t, func(context.Context, string, bool) error { return blocked })

	decision, err := checkUpgradeGate("/home/af", false)
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("a blocked/stale recovery must proceed, never wedge the launch; got decision=%v err=%v", decision, err)
	}
}

// A corrupt or partial journal makes the gate return a generic error. It must be
// treated as no-transaction and proceed, never as a fatal.
func TestUpgradeGate_CorruptJournalError_Proceeds(t *testing.T) {
	stubEntrypointGate(t, func(context.Context, string, bool) error {
		return errors.New("inspect active daemon upgrade before entrypoint startup: unexpected end of JSON input")
	})
	decision, err := checkUpgradeGate("/home/af", false)
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("a corrupt journal must proceed, never fatal; got decision=%v err=%v", decision, err)
	}
}

func TestUpgradeGate_ContextDeadline_Proceeds(t *testing.T) {
	stubEntrypointGate(t, func(context.Context, string, bool) error { return context.DeadlineExceeded })
	decision, err := checkUpgradeGate("/home/af", false)
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("a deadline must proceed; got decision=%v err=%v", decision, err)
	}
}

// The wrapper's context is the hard ceiling tied to upgradeGateTimeout: a gate
// that honors its deadline cannot make the launch wait past it. The bound is
// asserted as a small multiple of the CONFIGURED value, so a regression that
// hardcodes a larger ceiling (e.g. reverting to a fixed multi-second wait) fails.
func TestUpgradeGate_IsBoundedByTimeout(t *testing.T) {
	prevTimeout := upgradeGateTimeout
	upgradeGateTimeout = 100 * time.Millisecond
	t.Cleanup(func() { upgradeGateTimeout = prevTimeout })

	stubEntrypointGate(t, func(ctx context.Context, _ string, _ bool) error {
		<-ctx.Done() // a wedged gate that respects its deadline
		return ctx.Err()
	})

	start := time.Now()
	decision, err := checkUpgradeGate("/home/af", false)
	elapsed := time.Since(start)
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("a timed-out gate must proceed; got decision=%v err=%v", decision, err)
	}
	if elapsed > 5*upgradeGateTimeout {
		t.Fatalf("gate was not bounded by the configured %s ceiling; took %s", upgradeGateTimeout, elapsed)
	}
}

// The daemon-bind path passes skipWake=true; the client path passes false. The
// probe must receive exactly that so a bind never re-runs the client's wake.
func TestUpgradeGate_PassesSkipWakeThrough(t *testing.T) {
	var got []bool
	stubEntrypointGate(t, func(_ context.Context, _ string, skipWake bool) error {
		got = append(got, skipWake)
		return nil
	})
	_, _ = checkUpgradeGate("/home/af", false)
	_, _ = checkUpgradeGate("/home/af", true)
	if len(got) != 2 || got[0] != false || got[1] != true {
		t.Fatalf("skipWake was not forwarded to the probe; got %v", got)
	}
}

// End-to-end against the REAL gate: no journal on disk is the overwhelmingly
// common case and must proceed WITHOUT waiting. The fastness bound is a fraction
// of upgradeGateTimeout, so a regression that started waiting the whole budget
// (rather than returning immediately) fails.
func TestUpgradeGate_NoJournalOnDisk_Proceeds(t *testing.T) {
	prevTimeout := upgradeGateTimeout
	upgradeGateTimeout = 500 * time.Millisecond
	t.Cleanup(func() { upgradeGateTimeout = prevTimeout })

	home := t.TempDir()
	for _, skipWake := range []bool{false, true} {
		start := time.Now()
		decision, err := checkUpgradeGate(home, skipWake)
		elapsed := time.Since(start)
		if decision != upgradeGateProceed || err != nil {
			t.Fatalf("empty home must proceed (skipWake=%v); got decision=%v err=%v", skipWake, decision, err)
		}
		if elapsed > upgradeGateTimeout/2 {
			t.Fatalf("no-journal gate waited instead of proceeding (skipWake=%v, ceiling %s); took %s",
				skipWake, upgradeGateTimeout, elapsed)
		}
	}
}

// End-to-end against the REAL gate: a corrupt on-disk journal must not block or
// fatal an af launch (the exact hazard the fail-open gate exists for), and must
// return WITHOUT waiting — asserted as a fraction of upgradeGateTimeout.
func TestUpgradeGate_CorruptJournalOnDisk_Proceeds(t *testing.T) {
	prevTimeout := upgradeGateTimeout
	upgradeGateTimeout = 500 * time.Millisecond
	t.Cleanup(func() { upgradeGateTimeout = prevTimeout })

	home := t.TempDir()
	upgradeDir := filepath.Join(home, "upgrade")
	if err := os.MkdirAll(upgradeDir, 0o755); err != nil {
		t.Fatalf("make upgrade dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(upgradeDir, "active.json"), []byte("{ this is not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt journal: %v", err)
	}

	start := time.Now()
	decision, err := checkUpgradeGate(home, false)
	elapsed := time.Since(start)
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("a corrupt on-disk journal must proceed, never wedge the launch; got decision=%v err=%v", decision, err)
	}
	if elapsed > upgradeGateTimeout/2 {
		t.Fatalf("corrupt-journal gate waited instead of proceeding (ceiling %s); took %s", upgradeGateTimeout, elapsed)
	}
}

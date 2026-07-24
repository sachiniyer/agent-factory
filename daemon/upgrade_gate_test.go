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
func stubEntrypointGate(t *testing.T, fn func(context.Context, string) error) {
	t.Helper()
	prev := runEntrypointGate
	t.Cleanup(func() { runEntrypointGate = prev })
	runEntrypointGate = fn
}

// The classification is the whole safety contract (#2212 R1): only a provably
// live upgrade stops a launch; every other outcome proceeds, because the gate
// fronts every af invocation and a hang there is unrecoverable.

func TestUpgradeGate_NoTransaction_Proceeds(t *testing.T) {
	stubEntrypointGate(t, func(context.Context, string) error { return nil })
	decision, err := checkUpgradeGate("/home/af")
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("no transaction must proceed with no error; got decision=%v err=%v", decision, err)
	}
}

func TestUpgradeGate_LiveUpgrade_ReportsInProgress(t *testing.T) {
	inProgress := &upgradetxn.UpgradeInProgressError{
		TransactionID: "txn-1", ToVersion: "1.0.210", Phase: upgradetxn.PhaseSupervisorReady,
		Deadline: time.Now().Add(time.Minute),
	}
	stubEntrypointGate(t, func(context.Context, string) error { return inProgress })

	decision, err := checkUpgradeGate("/home/af")
	if decision != upgradeGateInProgress {
		t.Fatalf("a live recovery actor must stop the launch; got decision=%v", decision)
	}
	var typed *upgradetxn.UpgradeInProgressError
	if !errors.As(err, &typed) {
		t.Fatalf("in-progress must return the typed retryable error; got %v", err)
	}
}

// A stale journal whose actor never re-takes the recovery lock surfaces as a
// blocked-recovery result. It must PROCEED — the launch cannot wait forever on a
// dead actor.
func TestUpgradeGate_StaleTakeoverBlocked_Proceeds(t *testing.T) {
	blocked := &upgradetxn.UpgradeRecoveryBlockedError{
		TransactionID: "txn-2", ToVersion: "1.0.210", Phase: upgradetxn.PhaseDaemonStopping,
		Reason: "the preserved previous binary did not acquire the recovery lock",
	}
	stubEntrypointGate(t, func(context.Context, string) error { return blocked })

	decision, err := checkUpgradeGate("/home/af")
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("a blocked/stale recovery must proceed, never wedge the launch; got decision=%v err=%v", decision, err)
	}
}

// A corrupt or partial journal makes the gate return a generic error. It must be
// treated as no-transaction and proceed, never as a fatal.
func TestUpgradeGate_CorruptJournalError_Proceeds(t *testing.T) {
	stubEntrypointGate(t, func(context.Context, string) error {
		return errors.New("inspect active daemon upgrade before entrypoint startup: unexpected end of JSON input")
	})
	decision, err := checkUpgradeGate("/home/af")
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("a corrupt journal must proceed, never fatal; got decision=%v err=%v", decision, err)
	}
}

func TestUpgradeGate_ContextDeadline_Proceeds(t *testing.T) {
	stubEntrypointGate(t, func(context.Context, string) error { return context.DeadlineExceeded })
	decision, err := checkUpgradeGate("/home/af")
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("a deadline must proceed; got decision=%v err=%v", decision, err)
	}
}

// The wrapper's context is the hard ceiling: a gate that honors its deadline
// cannot make the launch wait past upgradeGateTimeout.
func TestUpgradeGate_IsBoundedByTimeout(t *testing.T) {
	prevTimeout := upgradeGateTimeout
	upgradeGateTimeout = 60 * time.Millisecond
	t.Cleanup(func() { upgradeGateTimeout = prevTimeout })

	stubEntrypointGate(t, func(ctx context.Context, _ string) error {
		<-ctx.Done() // a wedged gate that respects its deadline
		return ctx.Err()
	})

	start := time.Now()
	decision, err := checkUpgradeGate("/home/af")
	elapsed := time.Since(start)
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("a timed-out gate must proceed; got decision=%v err=%v", decision, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("gate was not bounded by the timeout; took %s", elapsed)
	}
}

// End-to-end against the REAL gate: no journal on disk is the overwhelmingly
// common case and must proceed with no wait.
func TestUpgradeGate_NoJournalOnDisk_Proceeds(t *testing.T) {
	home := t.TempDir()
	start := time.Now()
	decision, err := checkUpgradeGate(home)
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("empty home must proceed; got decision=%v err=%v", decision, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("no-journal gate was not fast; took %s", time.Since(start))
	}
}

// End-to-end against the REAL gate: a corrupt on-disk journal must not block or
// fatal an af launch (the exact hazard the fail-open gate exists for).
func TestUpgradeGate_CorruptJournalOnDisk_Proceeds(t *testing.T) {
	home := t.TempDir()
	upgradeDir := filepath.Join(home, "upgrade")
	if err := os.MkdirAll(upgradeDir, 0o755); err != nil {
		t.Fatalf("make upgrade dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(upgradeDir, "active.json"), []byte("{ this is not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt journal: %v", err)
	}

	start := time.Now()
	decision, err := checkUpgradeGate(home)
	if decision != upgradeGateProceed || err != nil {
		t.Fatalf("a corrupt on-disk journal must proceed, never wedge the launch; got decision=%v err=%v", decision, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("corrupt-journal gate was not fast; took %s", time.Since(start))
	}
}

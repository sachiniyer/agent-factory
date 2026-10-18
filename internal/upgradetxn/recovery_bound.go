package upgradetxn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// launchd has no native equivalent of systemd's StartLimitIntervalSec /
// StartLimitBurst, so a persistently failing launchd recovery actor would
// relaunch every ~2s (ThrottleInterval) forever under
// KeepAlive.SuccessfulExit=false. The recovery actor enforces the same budget
// in process, mirroring the systemd unit's bound: after this many failed runs
// in this interval it disarms the relaunch loop via launchctl disable and
// exits 0 — the operator-visible stand-down systemd produces as a failed
// unit, and that the unbounded launchd plist had no mechanism for. See
// renderLaunchdRecoveryPlist and renderSystemdRecoveryUnit for the asymmetry
// this closes, and boundLaunchdRecovery for where the actor consults it.
const (
	launchdRecoveryBurstInterval = 5 * time.Minute
	launchdRecoveryBurstLimit    = 3
)

// recoveryRetryCounter is the persisted attempt counter for the launchd
// stand-down path. It lives in the transaction directory, so a successful
// recovery's terminal cleanup removes it with the rest of the transaction
// (the budget resets for the next upgrade) and a reboot's RunAtLoad cannot
// restart a loop that already exhausted its budget: the counter survives the
// reboot, the pre-check stands the actor down before it re-enters the
// supervisor, matching systemd's "a failed unit stays failed until reset."
type recoveryRetryCounter struct {
	Attempts       int       `json:"attempts"`
	FirstAttemptAt time.Time `json:"first_attempt_at"`
}

func recoveryRetryCounterPath(home, txnID string) string {
	return filepath.Join(transactionDir(home, txnID), "recovery.attempts")
}

// readRecoveryRetryCounter returns the persisted counter and whether one
// exists. A missing or unparseable counter is not an error: the budget resets,
// matching a fresh transaction or a counter wiped by terminal cleanup.
func readRecoveryRetryCounter(home, txnID string) (recoveryRetryCounter, bool) {
	data, err := os.ReadFile(recoveryRetryCounterPath(home, txnID))
	if err != nil {
		return recoveryRetryCounter{}, false
	}
	var counter recoveryRetryCounter
	if err := json.Unmarshal(data, &counter); err != nil {
		return recoveryRetryCounter{}, false
	}
	return counter, true
}

// writeRecoveryRetryCounter persists the counter durably next to the journal
// so a reboot cannot lose the budget. The transaction directory exists from
// Prepare for every realistic persistent trigger (a corrupt active.json, a
// failing ledger write, a launchctl disable failure all happen after Prepare
// made it); it is created best-effort for the adversarial shape where only
// active.json survives, so the stand-down still lands when the journal cannot
// be read.
func writeRecoveryRetryCounter(home, txnID string, counter recoveryRetryCounter) error {
	if err := ensureRecoveryRetryCounterDir(home, txnID); err != nil {
		return err
	}
	data, err := json.MarshalIndent(counter, "", "  ")
	if err != nil {
		return fmt.Errorf("encode recovery retry counter: %w", err)
	}
	data = append(data, '\n')
	if err := durableAtomicWriteFile(recoveryRetryCounterPath(home, txnID), data, journalFileMode); err != nil {
		return fmt.Errorf("persist recovery retry counter: %w", err)
	}
	return nil
}

// ensureRecoveryRetryCounterDir guarantees the transaction directory exists
// and is a real directory (not a symlink leaf, which would redirect the
// counter outside the home). In production it already exists from Prepare, so
// this is a stat; only the adversarial "only active.json" shape reaches the
// MkdirAll, and the path is package-constructed under the canonical home so
// ancestor symlinks are already resolved.
func ensureRecoveryRetryCounterDir(home, txnID string) error {
	dir := transactionDir(home, txnID)
	if err := validateDirectoryNoSymlink(dir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("recovery retry counter directory is not a real directory: %w", err)
	}
	if err := os.MkdirAll(dir, transactionDirMode); err != nil {
		return fmt.Errorf("create recovery retry counter directory: %w", err)
	}
	return nil
}

// recoveryJobIsLaunchd reports whether the active transaction's recovery job
// is the launchd kind. The journal is the authority when it loads. When it
// does not — the corrupt-journal failure mode that is one of the persistent
// triggers this budget bounds — the detached fallback is a one-shot the
// service manager never relaunches, so on Darwin a re-invoked recovery actor
// must be in a launchd relaunch loop and the budget applies. On Linux the
// recovery job is never launchd (validateDaemonRecoveryPair forbids it), so a
// non-loading journal yields false and the budget is a no-op: systemd's own
// StartLimit* directives bound the same failure there.
func recoveryJobIsLaunchd(invocation RecoveryInvocation) bool {
	if txn, err := Load(invocation.HomeDir); err == nil {
		return txn.Journal().RecoveryJob.Kind == RecoveryJobLaunchd
	}
	return runtime.GOOS == "darwin"
}

// launchdRecoveryBudgetExhausted is the pre-check the actor runs before
// re-entering the supervisor. Once the budget is exhausted in the current
// burst window, the actor exits 0 without retrying, so a reboot's RunAtLoad
// cannot restart a loop that already stood down — matching systemd's "a
// failed unit stays failed until reset." A budget whose burst window has
// elapsed since the first recorded attempt is not exhausted: it resets, the
// way systemd's StartLimitIntervalSec rolls the window forward.
func launchdRecoveryBudgetExhausted(invocation RecoveryInvocation, now time.Time) bool {
	counter, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	if !ok {
		return false
	}
	if !counter.FirstAttemptAt.IsZero() && now.Sub(counter.FirstAttemptAt) >= launchdRecoveryBurstInterval {
		return false
	}
	return counter.Attempts >= launchdRecoveryBurstLimit
}

// recordLaunchdRecoveryFailure records one failed recovery run and, when the
// burst limit is reached within the interval, disarms the relaunch loop by
// disabling the job — the same launchctl disable the supervisor's
// DisableRecoveryJob uses, but derived from the transaction id alone so it
// works even when the journal is corrupt. stoodDown is true when the budget
// is exhausted in this call; the actor must exit 0 in that case to stop
// launchd's KeepAlive.SuccessfulExit=false from relaunching. A disable
// failure does not prevent the stand-down: exiting 0 alone stops the
// immediate relaunch, and a persistent launchctl failure is exactly the
// failure mode the budget exists to bound — not one that should keep the loop
// alive. The persisted counter ensures the next boot's pre-check stands down
// without retrying even when the disable could not stick, so a reboot cannot
// turn a one-shot launchctl outage back into the unbounded loop.
func recordLaunchdRecoveryFailure(
	ctx context.Context,
	invocation RecoveryInvocation,
	controller RecoveryJobController,
	now time.Time,
) (stoodDown bool, retErr error) {
	counter, _ := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	if counter.FirstAttemptAt.IsZero() || now.Sub(counter.FirstAttemptAt) >= launchdRecoveryBurstInterval {
		counter = recoveryRetryCounter{FirstAttemptAt: now}
	}
	counter.Attempts++
	if err := writeRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID, counter); err != nil {
		return false, err
	}
	if counter.Attempts < launchdRecoveryBurstLimit {
		return false, nil
	}
	if err := controller.disableLaunchdRecoveryJobByName(ctx, invocation.TransactionID); err != nil {
		log.WarningLog.Printf(
			"upgrade recovery: launchd retry budget exhausted for transaction %s after %d attempts, but the recovery job could not be disabled (%v); standing down (exit 0) anyway to stop the relaunch loop — the persisted counter means the next boot's RunAtLoad stands down without retrying",
			invocation.TransactionID, counter.Attempts, err)
		return true, nil
	}
	log.WarningLog.Printf(
		"upgrade recovery: launchd retry budget exhausted for transaction %s after %d attempts; the recovery job has been disabled and the actor is standing down (exit 0) so launchd's KeepAlive.SuccessfulExit=false stops relaunching",
		invocation.TransactionID, counter.Attempts)
	return true, nil
}

// boundLaunchdRecovery wraps the recovery actor runner with the launchd retry
// budget. For non-launchd jobs (systemd self-bounds via StartLimit*; detached
// never relaunches) it is a straight passthrough. For a launchd job it
// pre-checks the persisted budget — standing down before re-entering the
// supervisor if a prior run already exhausted it — runs the actor, and on a
// failing run records the attempt, disarming the relaunch loop and exiting
// 0 once the budget is reached. now is injected so the burst window is
// testable without real time.
func boundLaunchdRecovery(
	ctx context.Context,
	invocation RecoveryInvocation,
	controller RecoveryJobController,
	now func() time.Time,
	run func() error,
) error {
	launchd := recoveryJobIsLaunchd(invocation)
	if launchd && launchdRecoveryBudgetExhausted(invocation, now()) {
		return nil
	}
	err := run()
	if err == nil {
		return nil
	}
	if !launchd {
		return err
	}
	if stoodDown, _ := recordLaunchdRecoveryFailure(ctx, invocation, controller, now()); stoodDown {
		return nil
	}
	return err
}

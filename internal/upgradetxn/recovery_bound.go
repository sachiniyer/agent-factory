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

	"github.com/sachiniyer/agent-factory/internal/afhome"
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

// errAFHomeRemoved signals that the AF home or its upgrade storage was removed
// while a stale launchd recovery job was still armed. boundLaunchdRecovery
// treats it as a stand-down (exit 0) rather than recreating the deleted tree:
// see ensureRecoveryRetryCounterDir.
var errAFHomeRemoved = errors.New("upgrade home removed while recovery job still armed")

// recoveryRetryCounter is the persisted attempt counter for the launchd
// stand-down path. It lives in the transaction directory, so a successful
// recovery's terminal cleanup removes it with the rest of the transaction
// (the budget resets for the next upgrade) and a reboot's RunAtLoad cannot
// restart a loop that already exhausted its budget: the counter survives the
// reboot, the pre-check stands the actor down before it re-enters the
// supervisor, matching systemd's "a failed unit stays failed until reset."
//
// Exhausted is set once the burst limit is reached and is sticky:
// launchdRecoveryBudgetExhausted honors it regardless of the burst window, so
// a reboot later than launchdRecoveryBurstInterval cannot restart a loop that
// already stood down — the property systemd's "a failed unit stays failed
// until reset" gives for free and launchd does not, and the load-bearing
// guarantee when launchctl disable itself failed and the job is still armed.
// Terminal cleanup removing the transaction directory is the explicit reset.
type recoveryRetryCounter struct {
	Attempts       int       `json:"attempts"`
	FirstAttemptAt time.Time `json:"first_attempt_at"`
	Exhausted      bool      `json:"exhausted"`
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
// be read — but never by recreating a home or upgrade storage an operator
// deleted (see ensureRecoveryRetryCounterDir).
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

// clearRecoveryRetryCounter reconciles the pre-recorded attempt on a successful
// recovery run: the budget resets so the next upgrade starts fresh, the way
// systemd's window rolls forward on success. A missing file is not an error —
// terminal cleanup removes the transaction directory (and the counter with
// it) anyway, so a clear after cleanup is a no-op.
func clearRecoveryRetryCounter(home, txnID string) error {
	err := os.Remove(recoveryRetryCounterPath(home, txnID))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ensureRecoveryRetryCounterDir guarantees the transaction directory exists
// and is a real directory (not a symlink leaf, which would redirect the
// counter outside the home). In production it already exists from Prepare, so
// this is a stat; only the adversarial "only active.json" shape reaches the
// create path. It does NOT recreate a removed AF home or upgrade storage: if
// the upgrade root is gone, the home was uninstalled or intentionally removed
// while a stale launchd recovery job was still armed, and creating a
// fresh <home>/upgrade/transactions/<id> here would undo that removal and
// orphan a config tree the operator deleted. In that case it returns
// errAFHomeRemoved so boundLaunchdRecovery can stand the stale job down (exit
// 0) without recreating anything. With the upgrade root present, the
// create goes through afhome.MkdirAll — the guarded os.MkdirAll that
// TestNoBareMkdirAllUnderTheAFHome requires for any path at or inside the AF
// home, which in an abandoned daemon refuses rather than re-creating it —
// and only creates the transactions parent and the transaction leaf, so the
// adversarial shape still works without recreating the home or upgrade
// storage above it. The path is package-constructed under the canonical home
// so ancestor symlinks are already resolved.
func ensureRecoveryRetryCounterDir(home, txnID string) error {
	dir := transactionDir(home, txnID)
	if err := validateDirectoryNoSymlink(dir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("recovery retry counter directory is not a real directory: %w", err)
	}
	if err := validateDirectoryNoSymlink(upgradeRoot(home)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errAFHomeRemoved
		}
		return fmt.Errorf("validate upgrade storage root: %w", err)
	}
	if err := afhome.MkdirAll(dir, transactionDirMode); err != nil {
		// afhome.MkdirAll refuses when a process that armed the home-present
		// latch observes the home gone; that is the same removed-home shape the
		// upgrade-root check above stands down for, so collapse it to the same
		// sentinel and let boundLaunchdRecovery stand the stale job down rather
		// than recreate the home.
		if errors.Is(err, afhome.ErrRemoved) {
			return errAFHomeRemoved
		}
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
// re-entering the supervisor. Once the budget is exhausted it stays stood down
// regardless of the burst window — Exhausted is sticky until terminal cleanup
// removes the counter — so a reboot's RunAtLoad cannot restart a loop that
// already stood down, matching systemd's "a failed unit stays failed until
// reset." A budget whose burst window has elapsed since the first recorded
// attempt, and which has not been marked Exhausted, is not exhausted: the
// window rolls forward the way systemd's StartLimitIntervalSec does.
func launchdRecoveryBudgetExhausted(invocation RecoveryInvocation, now time.Time) bool {
	counter, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	if !ok {
		return false
	}
	if counter.Exhausted {
		return true
	}
	if !counter.FirstAttemptAt.IsZero() && now.Sub(counter.FirstAttemptAt) >= launchdRecoveryBurstInterval {
		return false
	}
	return counter.Attempts >= launchdRecoveryBurstLimit
}

// recordLaunchdRecoveryAttempt persists one recovery attempt BEFORE the actor
// runs, so a deterministic panic, fatal runtime error, or signal termination
// — which never returns to a post-run record path — still counts toward the
// burst budget. launchd treats that abnormal exit as unsuccessful and
// relaunches, which is exactly the unbounded loop this budget bounds. When
// the increment reaches the burst limit it also marks the counter Exhausted,
// so the budget stays sticky past the burst window even if the actor never
// returns to stand down (a panic on the final attempt) or launchctl disable
// later fails — the persisted marker is what keeps a later boot's RunAtLoad
// stood down. Returns errAFHomeRemoved if the AF home/storage was removed and
// must not be recreated; the caller stands down in that case.
func recordLaunchdRecoveryAttempt(invocation RecoveryInvocation, now time.Time) error {
	counter, _ := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	if counter.Exhausted {
		// Already stood down — the pre-check should have caught this, but
		// handle it defensively: don't increment past exhaustion.
		return nil
	}
	if counter.FirstAttemptAt.IsZero() || now.Sub(counter.FirstAttemptAt) >= launchdRecoveryBurstInterval {
		counter = recoveryRetryCounter{FirstAttemptAt: now}
	}
	counter.Attempts++
	if counter.Attempts >= launchdRecoveryBurstLimit {
		counter.Exhausted = true
	}
	return writeRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID, counter)
}

// standDownLaunchdRecoveryIfExhausted disarms the launchd relaunch loop when
// the persisted counter has reached the burst limit. It does NOT increment:
// the attempt was pre-recorded before the actor ran (recordLaunchdRecoveryAttempt).
// It ensures the Exhausted marker is persisted (defensively, in case the
// pre-record missed it) so a later boot's pre-check stays stood down past the
// burst window, then runs launchctl disable — the same disable the
// supervisor's DisableRecoveryJob uses, but derived from the transaction id
// alone so it works even when the journal is corrupt. A disable failure does
// not prevent the stand-down: exiting 0 alone stops the immediate relaunch,
// the Exhausted marker keeps the next boot stood down without retrying, and a
// persistent launchctl failure is exactly the loop the budget exists to bound
// — not one that should keep the loop alive.
func standDownLaunchdRecoveryIfExhausted(
	ctx context.Context,
	invocation RecoveryInvocation,
	controller RecoveryJobController,
) bool {
	counter, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	if !ok {
		return false
	}
	if !counter.Exhausted && counter.Attempts < launchdRecoveryBurstLimit {
		return false
	}
	if !counter.Exhausted {
		counter.Exhausted = true
		if err := writeRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID, counter); err != nil {
			log.WarningLog.Printf(
				"upgrade recovery: launchd retry budget exhausted for transaction %s after %d attempts, but the exhausted marker could not be persisted (%v); standing down (exit 0) anyway — the at-limit counter still bounds reboots within the burst window",
				invocation.TransactionID, counter.Attempts, err)
		}
	}
	if err := controller.disableLaunchdRecoveryJobByName(ctx, invocation.TransactionID); err != nil {
		log.WarningLog.Printf(
			"upgrade recovery: launchd retry budget exhausted for transaction %s after %d attempts, but the recovery job could not be disabled (%v); standing down (exit 0) anyway to stop the relaunch loop — the persisted exhausted marker means the next boot's RunAtLoad stands down without retrying",
			invocation.TransactionID, counter.Attempts, err)
		return true
	}
	log.WarningLog.Printf(
		"upgrade recovery: launchd retry budget exhausted for transaction %s after %d attempts; the recovery job has been disabled and the actor is standing down (exit 0) so launchd's KeepAlive.SuccessfulExit=false stops relaunching",
		invocation.TransactionID, counter.Attempts)
	return true
}

// boundLaunchdRecovery wraps the recovery actor runner with the launchd retry
// budget. For non-launchd jobs (systemd self-bounds via StartLimit*; detached
// never relaunches) it is a straight passthrough. For a launchd job it
// pre-checks the persisted budget — standing down before re-entering the
// supervisor if a prior run already exhausted it — pre-records the attempt so
// a panic or signal termination still counts, runs the actor, clears the
// counter on success (the budget resets), and on a failing run stands the
// job down and exits 0 once the budget is reached. now is injected so the
// burst window is testable without real time.
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
	// Persist the attempt BEFORE running the recovery actor so a panic, fatal
	// runtime error, or signal termination still counts toward the budget —
	// launchd treats that abnormal exit as unsuccessful and relaunches, which
	// is exactly the unbounded loop this budget bounds. A successful run
	// reconciles by clearing the counter below; a failing run checks the
	// budget after the actor returns and stands down if it is exhausted.
	if launchd {
		if err := recordLaunchdRecoveryAttempt(invocation, now()); err != nil {
			if errors.Is(err, errAFHomeRemoved) {
				// The AF home/storage was removed while a stale launchd job
				// was still armed. Recreating it would undo the operator's
				// removal; stand down (exit 0) so launchd stops relaunching and
				// the removal stays stable.
				return nil
			}
			return err
		}
	}
	err := run()
	if err == nil {
		if launchd {
			if clearErr := clearRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID); clearErr != nil {
				// A clear failure does not undo the successful recovery; the
				// transaction directory's terminal cleanup removes the
				// counter anyway. Log and continue with the success.
				log.WarningLog.Printf("upgrade recovery: could not clear launchd retry counter after a successful run for transaction %s: %v", invocation.TransactionID, clearErr)
			}
		}
		return nil
	}
	if !launchd {
		return err
	}
	if standDownLaunchdRecoveryIfExhausted(ctx, invocation, controller) {
		return nil
	}
	return err
}

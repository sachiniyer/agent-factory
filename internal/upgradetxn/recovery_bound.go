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
// 0) without recreating anything. With the upgrade root present, the create
// goes through afhome.RequirePresent — the guarded home-present latch that
// TestNoBareMkdirAllUnderTheAFHome requires for any path at or inside the AF
// home, which in an abandoned daemon refuses rather than re-creating it —
// then createDurableDirectory, which validates the transactions parent as a
// real directory and creates only the immediate transaction leaf (not the full
// hierarchy) so a symlinked transactions root cannot redirect the counter
// outside the home, and so the adversarial shape still works without
// recreating the home or upgrade storage above it. The path is
// package-constructed under the canonical home so ancestor symlinks are
// already resolved.
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
	// Validate the transactions parent as a real directory and create only the
	// immediate transaction leaf, mirroring ensureRecoveryLockDirectoryLocked:
	// if <home>/upgrade/transactions has been replaced by a symlink, validating
	// only upgradeRoot does not detect it and a recursive MkdirAll would follow
	// the symlink and create the counter outside the AF home. afhome.RequirePresent
	// preserves the home-present latch's refusal to re-create a removed home.
	//
	// A missing transactions parent is NOT a removed home: the upgrade root
	// survives above, so the operator did not uninstall the AF home. The
	// adversarial "only active.json survives" shape (a corrupt journal with no
	// transaction directory — the bug report's primary persistent trigger)
	// reaches this path, and Prepare's transactions root is the normal upgrade
	// storage it is safe to recreate here. Only a removed upgrade root is a
	// removed home (handled above), and a symlinked transactions root falls
	// through to the non-ErrNotExist branch below and is rejected.
	parent := filepath.Dir(dir)
	if err := validateDirectoryNoSymlink(parent); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("validate transactions root for recovery retry counter: %w", err)
		}
		if err := ensureDurableDirectory(upgradeRoot(home), parent, transactionDirMode); err != nil {
			return fmt.Errorf("create transactions root for recovery retry counter: %w", err)
		}
	}
	if err := afhome.RequirePresent(dir); err != nil {
		if errors.Is(err, afhome.ErrRemoved) {
			return errAFHomeRemoved
		}
		return fmt.Errorf("create recovery retry counter directory: %w", err)
	}
	if err := createDurableDirectory(parent, dir, transactionDirMode); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create recovery retry counter directory: %w", err)
	}
	return nil
}

// launchdFallbackOnCorruptJournal reports whether a journal that cannot load
// implies a launchd relaunch loop on this OS. Darwin's recovery jobs are
// launchd-managed, so a corrupt journal there must still be bounded; Linux's
// are never launchd (validateDaemonRecoveryPair forbids it), so the budget is a
// no-op there and systemd's own StartLimit* directives bound the same failure.
// It is a package var so a test can force the Darwin fallback on a Linux CI
// runner and cover the corrupt-journal budget path the macOS cell exercises
// end-to-end via the built af binary (TestRecoveryExitLogsHintOnActorFailure):
// without it, the fallback branch below is dead code on Linux and a regression
// that only surfaces on macOS (the original CI failure this guards) escapes CI.
var launchdFallbackOnCorruptJournal = func() bool { return runtime.GOOS == "darwin" }

// recoveryJobIsLaunchd reports whether the recovery job that is actually
// firing this invocation is the launchd kind, so the budget bounds the right
// relaunch loop. The active journal is the authority when it loads AND still
// names this invocation: the recovery job kind belongs to the transaction the
// job was armed for, so a matching id is the only case where the loaded
// journal's kind describes the job that is running. When the journal does not
// load — the corrupt-journal failure mode that is one of the persistent
// triggers this budget bounds — the detached fallback is a one-shot the
// service manager never relaunches, so on Darwin a re-invoked recovery actor
// must be in a launchd relaunch loop and the budget applies. On Linux the
// recovery job is never launchd (validateDaemonRecoveryPair forbids it), so a
// non-loading journal yields false and the budget is a no-op: systemd's own
// StartLimit* directives bound the same failure there.
//
// When the active journal loads but names a different transaction — a stale
// launchd job whose RunAtLoad/delayed restart fired after a newer transaction
// replaced it — the loaded journal's kind describes the newer transaction's
// recovery job, not the stale job that is actually running. The newer job's
// kind cannot downgrade the still-armed launchd job's relaunch loop, so this
// falls back to the same OS classification a corrupt journal uses, and the
// stale-transaction gate below (activeTransactionStillNamesInvocation) then
// disables and stands the stale job down. On Linux this is moot — a stale
// launchd job cannot exist (validateDaemonRecoveryPair forbids launchd on
// Linux) — so the fallback yields false and the budget stays a no-op.
func recoveryJobIsLaunchd(invocation RecoveryInvocation) bool {
	if txn, err := Load(invocation.HomeDir); err == nil {
		journal := txn.Journal()
		if journal.ID == invocation.TransactionID {
			return journal.RecoveryJob.Kind == RecoveryJobLaunchd
		}
		// The active transaction replaced the one this job was armed for, so
		// its kind describes the newer job, not the stale launchd job that is
		// actually firing this invocation. Classify from the platform.
		return launchdFallbackOnCorruptJournal()
	}
	return launchdFallbackOnCorruptJournal()
}

// activeTransactionStillNamesInvocation reports whether the active journal still
// names this invocation's transaction. The pre-record in boundLaunchdRecovery
// must not run when it does not: a stale launchd job whose RunAtLoad/delayed
// restart fires after terminal cleanup removed active.json, or after a newer
// transaction replaced it, must not recreate the deleted/replaced transaction
// directory the pre-record would otherwise leave as an orphan the actor's
// ErrNoActiveTransaction/stale-job path does not clean up. recoveryJobIsLaunchd
// falls back to the OS when the journal cannot load and cannot tell that case
// apart from a corrupt journal (the bug report's primary persistent trigger,
// which the budget must still bound), so this check gates only the pre-record:
// a corrupt journal loads to a non-ErrNoActiveTransaction error and returns
// true, a gone active.json returns ErrNoActiveTransaction and returns false,
// and a superseding transaction returns a different id and returns false.
func activeTransactionStillNamesInvocation(invocation RecoveryInvocation) bool {
	txn, err := Load(invocation.HomeDir)
	if err != nil {
		return !errors.Is(err, ErrNoActiveTransaction)
	}
	return txn.Journal().ID == invocation.TransactionID
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
// relaunches, which is exactly the unbounded loop this budget bounds. The
// pre-record counts the attempt but does NOT mark the counter Exhausted:
// exhaustion is decided at stand-down time, only when a start is actually
// rejected while the starts remain inside the burst window, so a slow final
// attempt that runs past the burst interval does not make the budget sticky
// (systemd's StartLimitIntervalSec rolls the window forward once it elapses).
// A panic on the final attempt is still bounded — the next launch's pre-check
// sees the at-limit counter within the window and stands the job down. Returns
// errAFHomeRemoved if the AF home/storage was removed and must not be
// recreated; the caller stands down in that case.
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
//
// A non-Exhausted counter that has reached the burst limit only stands down
// while the starts remain inside the burst window. A slow final attempt that
// runs past the interval rolls the window forward the way systemd's
// StartLimitIntervalSec does: the next start is allowed rather than stood down
// permanently, so the budget is not made sticky by the pre-record alone. now is
// injected so the window is testable without real time.
func standDownLaunchdRecoveryIfExhausted(
	ctx context.Context,
	invocation RecoveryInvocation,
	controller RecoveryJobController,
	now time.Time,
) bool {
	counter, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	if !ok {
		return false
	}
	if !counter.Exhausted && counter.Attempts < launchdRecoveryBurstLimit {
		return false
	}
	if !counter.Exhausted {
		// The at-limit counter has not been made sticky yet. Only stand down
		// (and persist the marker) while the starts remain inside the burst
		// window: a slow final attempt that ran past the interval rolls the
		// window forward instead, so the next start is allowed and the budget
		// resets. launchdRecoveryBudgetExhausted already gates this for the
		// pre-check path; the post-run path re-checks here because the actor
		// may have run past the interval between the pre-record and the return.
		if !counter.FirstAttemptAt.IsZero() && now.Sub(counter.FirstAttemptAt) >= launchdRecoveryBurstInterval {
			return false
		}
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
	nowTime := now()
	if launchd && launchdRecoveryBudgetExhausted(invocation, nowTime) {
		// The budget is already exhausted. A prior run pre-recorded the
		// exhausting attempt but never reached the post-run stand-down (a
		// panic, fatal error, or signal termination on the final attempt), so
		// this invocation must disable the job: exiting 0 alone stops the
		// immediate relaunch, but the job remains enabled and would run again
		// at every login or reboot. The stand-down helper reads the exhausted
		// counter, persists the marker defensively, and fires launchctl
		// disable.
		_ = standDownLaunchdRecoveryIfExhausted(ctx, invocation, controller, nowTime)
		return nil
	}
	// Persist the attempt BEFORE running the recovery actor so a panic, fatal
	// runtime error, or signal termination still counts toward the budget —
	// launchd treats that abnormal exit as unsuccessful and relaunches, which
	// is exactly the unbounded loop this budget bounds. A successful run
	// reconciles by clearing the counter below; a failing run checks the
	// budget after the actor returns and stands down if it is exhausted.
	if launchd {
		// A stale launchd job whose active transaction is gone (terminal
		// cleanup removed active.json) or superseded must not recreate the
		// deleted/replaced transaction directory the pre-record would
		// otherwise leave as an orphan: recoveryJobIsLaunchd falls back to the
		// OS when the journal cannot load and cannot tell that case apart from
		// a corrupt journal (which the budget must still bound), so check the
		// active transaction explicitly. The actor returns nil for the gone or
		// superseded case without recreating anything.
		if !activeTransactionStillNamesInvocation(invocation) {
			// The active transaction this job was armed for is gone (terminal
			// cleanup removed active.json) or has been superseded, so the actor
			// would return nil for a stale job. A successful exit stops
			// KeepAlive.SuccessfulExit=false from relaunching, but the plist
			// stays enabled with RunAtLoad, so a later login or boot would
			// re-launch the stale previous binary. Disable the job before
			// standing down so removal or replacement does not leave a
			// permanently recurring recovery job (launchctl disable persists
			// across boots). A disable failure does not keep the loop alive:
			// exit 0 already stops the immediate relaunch, and the next boot's
			// RunAtLoad re-enters this path and disables again.
			if disableErr := controller.disableLaunchdRecoveryJobByName(ctx, invocation.TransactionID); disableErr != nil {
				log.WarningLog.Printf(
					"upgrade recovery: the active transaction for launchd recovery job %s is gone, but the job could not be disabled (%v); standing down (exit 0) to stop the immediate relaunch loop — the job may re-launch on a later login or reboot",
					invocation.TransactionID, disableErr)
			}
			return nil
		}
		if err := recordLaunchdRecoveryAttempt(invocation, now()); err != nil {
			if errors.Is(err, errAFHomeRemoved) {
				// The AF home/storage was removed while a stale launchd job
				// was still armed. Recreating it would undo the operator's
				// removal; stand down (exit 0) so launchd stops relaunching and
				// the removal stays stable. The job is still enabled with
				// RunAtLoad, though, so a later login or boot would re-launch
				// the stale previous binary; disable it first (the same path
				// the gone-transaction and unwriteable-counter branches take).
				// A disable failure does not keep the loop alive: exit 0 already
				// stops the immediate relaunch (KeepAlive.SuccessfulExit=false),
				// and the next boot's RunAtLoad re-enters this path — the home
				// is still gone, so this branch stands down again.
				if disableErr := controller.disableLaunchdRecoveryJobByName(ctx, invocation.TransactionID); disableErr != nil {
					log.WarningLog.Printf(
						"upgrade recovery: the AF home for launchd recovery job %s was removed, but the job could not be disabled (%v); standing down (exit 0) to stop the immediate relaunch loop — the job may re-launch on a later login or reboot until the removed home is also cleaned up",
						invocation.TransactionID, disableErr)
				}
				return nil
			}
			// The budget cannot be persisted, so it cannot bound a persistent
			// failure: a read-only transaction directory, lost write
			// permission, or a persistent filesystem I/O error means launchd
			// would relaunch every ThrottleInterval without the on-disk
			// counter ever advancing — the unbounded restart loop this change
			// is intended to stop. Stand the job down (disable + exit 0) so the
			// loop stops. A disable failure does not keep the loop alive:
			// exiting 0 alone stops the immediate relaunch
			// (KeepAlive.SuccessfulExit=false), and the next boot's RunAtLoad
			// re-enters this path and stands down again.
			log.WarningLog.Printf(
				"upgrade recovery: could not persist the launchd retry counter for transaction %s (%v); disabling the recovery job and standing down to stop an unbounded relaunch loop the budget cannot bound without a durable counter",
				invocation.TransactionID, err)
			if disableErr := controller.disableLaunchdRecoveryJobByName(ctx, invocation.TransactionID); disableErr != nil {
				log.WarningLog.Printf(
					"upgrade recovery: could not disable the launchd recovery job for transaction %s after a failed counter write (%v); standing down (exit 0) anyway to stop the immediate relaunch loop",
					invocation.TransactionID, disableErr)
			}
			return nil
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
	if standDownLaunchdRecoveryIfExhausted(ctx, invocation, controller, now()) {
		return nil
	}
	return err
}

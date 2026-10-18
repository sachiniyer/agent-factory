package upgradetxn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// launchdBoundFixture prepares a real launchd-kind transaction whose journal
// loads and whose transaction directory exists, the shape the budget's
// production triggers (corrupt journal, failing ledger write, launchctl
// disable failure) all reach it with. It returns the invocation pointing at
// the prepared home and transaction id.
func launchdBoundFixture(t *testing.T) RecoveryInvocation {
	t.Helper()
	txn := prepareRecoveryJobFixture(t, RecoveryJobLaunchd)
	return RecoveryInvocation{HomeDir: txn.Journal().HomeDir, TransactionID: txn.Journal().ID}
}

func TestLaunchdRecoveryJobNameIsDeterministicFromTransactionID(t *testing.T) {
	require.Equal(t, "com.agent-factory.upgrade-recovery.txn-2212",
		launchdRecoveryJobName("txn-2212"))
	// NewRecoveryJob and validateRecoveryJob must agree with the helper, so a
	// job built through the public constructor round-trips through it and the
	// stand-down disable (which derives the label from the txn id alone) cannot
	// diverge from the plist's Label.
	unitDir, err := canonicalExistingDir(t.TempDir())
	require.NoError(t, err)
	job, err := NewRecoveryJob(RecoveryJobLaunchd, "txn-roundtrip", unitDir)
	require.NoError(t, err)
	require.Equal(t, launchdRecoveryJobName("txn-roundtrip"), job.Name)
	require.NoError(t, validateRecoveryJob("txn-roundtrip", job))
}

func TestDisableLaunchdRecoveryJobByNameUsesDeterministicLabel(t *testing.T) {
	runtime := &fakeRecoveryJobRuntime{launchdUserID: 501}
	controller := runtime.controller()

	require.NoError(t, controller.disableLaunchdRecoveryJobByName(context.Background(), "txn-disable"))

	require.Equal(t, []recoveryCommandCall{{
		name: "launchctl",
		args: []string{"disable", "gui/501/com.agent-factory.upgrade-recovery.txn-disable"},
	}}, runtime.commands)
}

// TestRecoveryJobIsLaunchdFallsBackToOSWhenJournalUnreadable covers both the
// corrupt-journal persistent trigger (a non-ErrNoActiveTransaction Load failure
// — the bug report's primary scenario) and the terminal-cleanup shape (active.json
// gone). In both cases the kind cannot be read, so on Darwin (the only platform
// whose service-managed recovery is launchd) the budget must still apply, and on
// Linux it must not (systemd self-bounds). The assertion holds on both CI cells:
// Linux yields false==false, macOS yields true==true.
func TestRecoveryJobIsLaunchdFallsBackToOSWhenJournalUnreadable(t *testing.T) {
	for _, shape := range []string{"corrupt", "missing"} {
		t.Run(shape, func(t *testing.T) {
			home := t.TempDir()
			if shape == "corrupt" {
				require.NoError(t, os.MkdirAll(filepath.Join(home, "upgrade"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(home, "upgrade", "active.json"),
					[]byte("{not-valid-json"), 0o600))
			}
			invocation := RecoveryInvocation{HomeDir: home, TransactionID: "some-txn"}
			require.Equal(t, runtime.GOOS == "darwin", recoveryJobIsLaunchd(invocation),
				"a %s journal cannot reveal the kind; on Darwin the budget applies (launchd relaunch loop), on Linux it must not (systemd self-bounds)", shape)
		})
	}
}

// TestLaunchdRecoveryBudgetExhaustedMatchesBurstAndInterval pins the
// StartLimitBurst=3 / StartLimitIntervalSec=300 mirror: below-limit never
// exhausts, at-limit within the window exhausts, after the interval the window
// rolls forward (resets) for a NOT-yet-exhausted budget, a corrupt counter
// resets rather than stranding the actor in stand-down forever, and an
// Exhausted budget stays sticky past the burst window until terminal cleanup
// removes the counter — the property a reboot later than the interval must not
// restart a loop that already stood down (the load-bearing guarantee when
// launchctl disable failed and the job is still armed).
func TestLaunchdRecoveryBudgetExhaustedMatchesBurstAndInterval(t *testing.T) {
	invocation := launchdBoundFixture(t)
	start := time.Now()

	require.False(t, launchdRecoveryBudgetExhausted(invocation, start), "no counter recorded: budget not exhausted")

	counter := recoveryRetryCounter{FirstAttemptAt: start}
	for attempt := 1; attempt < launchdRecoveryBurstLimit; attempt++ {
		counter.Attempts = attempt
		require.NoError(t, writeRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID, counter))
		require.False(t, launchdRecoveryBudgetExhausted(invocation, start.Add(time.Second)),
			"%d attempts (below the burst limit) must not exhaust the budget", attempt)
	}

	counter.Attempts = launchdRecoveryBurstLimit
	require.NoError(t, writeRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID, counter))
	require.True(t, launchdRecoveryBudgetExhausted(invocation, start.Add(time.Second)),
		"the burst limit within the interval must exhaust the budget")

	require.False(t, launchdRecoveryBudgetExhausted(invocation, start.Add(launchdRecoveryBurstInterval+time.Second)),
		"after the burst interval elapses a not-yet-exhausted budget resets")

	// An Exhausted budget stays sticky past the burst window: a reboot later
	// than the interval must not restart a loop that already stood down.
	counter.Exhausted = true
	require.NoError(t, writeRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID, counter))
	require.True(t, launchdRecoveryBudgetExhausted(invocation, start.Add(launchdRecoveryBurstInterval+time.Second)),
		"an exhausted budget must stay stood down past the burst window until terminal cleanup resets it")

	// A corrupt counter resets the budget rather than stranding the actor.
	require.NoError(t, os.WriteFile(recoveryRetryCounterPath(invocation.HomeDir, invocation.TransactionID),
		[]byte("{not-valid"), 0o600))
	require.False(t, launchdRecoveryBudgetExhausted(invocation, time.Now()),
		"a corrupt counter must reset the budget, not strand the actor in stand-down")
}

// TestRecordLaunchdRecoveryAttemptIncrementsAndMarksExhaustedAtBurstLimit is
// the core pre-record guard: the first N-1 attempts persist Attempts=N with
// Exhausted=false and fire no launchctl (the pre-record owns the count, not the
// stand-down), and the Nth within the window persists Attempts=N with
// Exhausted=true so the budget is sticky past the burst window even before the
// post-failure stand-down runs. It also covers the interval-reset (an attempt
// after the interval starts a fresh window with Attempts=1, Exhausted=false).
func TestRecordLaunchdRecoveryAttemptIncrementsAndMarksExhaustedAtBurstLimit(t *testing.T) {
	invocation := launchdBoundFixture(t)
	start := time.Now()

	for attempt := 1; attempt < launchdRecoveryBurstLimit; attempt++ {
		require.NoError(t, recordLaunchdRecoveryAttempt(invocation, start.Add(time.Duration(attempt)*time.Second)))
		counter, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
		require.True(t, ok)
		require.Equal(t, attempt, counter.Attempts)
		require.False(t, counter.Exhausted, "below the burst limit the budget must not be marked exhausted")
	}

	require.NoError(t, recordLaunchdRecoveryAttempt(invocation, start.Add(launchdRecoveryBurstLimit*time.Second)))
	counter, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.True(t, ok)
	require.Equal(t, launchdRecoveryBurstLimit, counter.Attempts)
	require.True(t, counter.Exhausted, "the burst limit must mark the budget exhausted so it stays sticky past the window")

	// After the interval elapses, a fresh attempt on a NOT-yet-exhausted budget
	// starts a new window with Attempts=1 — StartLimitIntervalSec rolls the
	// window forward. (An Exhausted budget would not reach here: the pre-check
	// stands it down before the pre-record.)
	require.NoError(t, os.RemoveAll(recoveryRetryCounterPath(invocation.HomeDir, invocation.TransactionID)))
	require.NoError(t, recordLaunchdRecoveryAttempt(invocation, start.Add(launchdRecoveryBurstInterval+5*time.Second)))
	counter, ok = readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.True(t, ok)
	require.Equal(t, 1, counter.Attempts, "the first attempt after the interval starts a fresh window")
	require.False(t, counter.Exhausted)
}

// TestStandDownLaunchdRecoveryIfExhaustedFiresDisableAtBurstLimit guards the
// stand-down lever: below the limit it is a no-op (no launchctl), and at the
// limit it fires launchctl disable on the deterministic label. The attempt was
// pre-recorded by recordLaunchdRecoveryAttempt, so standDownLaunchdRecoveryIfExhausted
// does not increment — it only reads the counter, ensures Exhausted is
// persisted, and disables.
func TestStandDownLaunchdRecoveryIfExhaustedFiresDisableAtBurstLimit(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{launchdUserID: 501}
	controller := runtime.controller()
	start := time.Now()

	for attempt := 1; attempt < launchdRecoveryBurstLimit; attempt++ {
		require.NoError(t, recordLaunchdRecoveryAttempt(invocation, start.Add(time.Duration(attempt)*time.Second)))
		require.False(t, standDownLaunchdRecoveryIfExhausted(context.Background(), invocation, controller),
			"below the burst limit the stand-down must not fire")
		require.Empty(t, runtime.commands, "the disable must not fire before the budget is exhausted")
	}

	// The Nth pre-record marks the budget exhausted; the post-failure
	// stand-down then fires launchctl disable on the deterministic label.
	require.NoError(t, recordLaunchdRecoveryAttempt(invocation, start.Add(launchdRecoveryBurstLimit*time.Second)))
	require.True(t, standDownLaunchdRecoveryIfExhausted(context.Background(), invocation, controller),
		"the third failure in the interval must stand down, mirroring StartLimitBurst=3")
	require.Equal(t, []recoveryCommandCall{{
		name: "launchctl",
		args: []string{"disable", "gui/501/" + launchdRecoveryJobName(invocation.TransactionID)},
	}}, runtime.commands, "the stand-down must disable the job by its deterministic label")

	counter, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.True(t, ok)
	require.True(t, counter.Exhausted, "the exhausted marker must be persisted so the budget stays sticky past the window")
}

// TestStandDownLaunchdRecoveryIfExhaustedStandsDownEvenWhenDisableFails guards
// the key invariant: the stand-down (exit 0) is NOT gated on the disable
// succeeding. A persistent launchctl failure is exactly the loop the budget
// bounds — not one that should keep it alive — and the persisted Exhausted
// marker ensures the next boot's pre-check stands down without retrying even
// when the disable could not stick, which is the property a reboot later than
// the burst window depends on.
func TestStandDownLaunchdRecoveryIfExhaustedStandsDownEvenWhenDisableFails(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{
		launchdUserID: 501,
		commandErr:    errors.New("launchctl persistently broken"),
	}
	controller := runtime.controller()
	start := time.Now()

	for attempt := 1; attempt <= launchdRecoveryBurstLimit; attempt++ {
		require.NoError(t, recordLaunchdRecoveryAttempt(invocation, start.Add(time.Duration(attempt)*time.Second)))
	}
	require.True(t, standDownLaunchdRecoveryIfExhausted(context.Background(), invocation, controller),
		"the disable failure must not prevent the stand-down")
	require.Equal(t, []string{"disable", "gui/501/" + launchdRecoveryJobName(invocation.TransactionID)},
		runtime.commands[0].args)

	// The Exhausted marker is what keeps a later boot stood down past the
	// burst window when the disable could not stick. A reboot later than the
	// interval must still stand down via the pre-check, not restart the loop.
	counter, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.True(t, ok)
	require.True(t, counter.Exhausted)
	require.True(t, launchdRecoveryBudgetExhausted(invocation, start.Add(launchdRecoveryBurstInterval+time.Second)),
		"a reboot later than the burst interval must not restart a loop whose disable failed; the exhausted marker keeps it stood down")
}

// TestRecordLaunchdRecoveryAttemptReturnsErrorWhenCounterUnwriteable guards
// the disk-full/ledger-broken shape from the bug report: the pre-record's
// counter write fails, so the attempt is not counted and the budget does not
// progress. The actor returns the error (exit 1, relaunch) — the budget does
// not stand down, and no disable fires. This is the one residual gap vs
// systemd's in-memory tracking, and it self-limits — a full disk also bounds
// recovery.log growth.
func TestRecordLaunchdRecoveryAttemptReturnsErrorWhenCounterUnwriteable(t *testing.T) {
	invocation := launchdBoundFixture(t)

	// A symlinked transaction directory is rejected by validateDirectoryNoSymlink,
	// so the counter cannot be persisted and the budget cannot progress.
	txnDir := transactionDir(invocation.HomeDir, invocation.TransactionID)
	require.NoError(t, os.RemoveAll(txnDir))
	require.NoError(t, os.Symlink(t.TempDir(), txnDir))

	err := recordLaunchdRecoveryAttempt(invocation, time.Now())
	require.Error(t, err)
	require.ErrorContains(t, err, "not a real directory")
}

// TestBoundLaunchdRecoveryStandsDownAfterBurstLimitAndPreventsNextRun is the
// end-to-end wrapper guard: the first N-1 failing runs pass the supervisor
// error through (exit 1), the Nth stands down (nil so the actor exits 0) and
// fires launchctl disable, and the (N+1)th invocation — a reboot's RunAtLoad —
// stands down via the pre-check WITHOUT re-entering the supervisor, so a reboot
// cannot restart the loop. A reboot later than the burst interval also stands
// down via the sticky Exhausted marker — the property systemd's "a failed unit
// stays failed until reset" gives for free and launchd does not.
func TestBoundLaunchdRecoveryStandsDownAfterBurstLimitAndPreventsNextRun(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{launchdUserID: 501}
	controller := runtime.controller()
	start := time.Now()

	supervisorFailure := errors.New("persistent journal load failure")
	runCalls := 0
	run := func() error {
		runCalls++
		return supervisorFailure
	}

	for attempt := 1; attempt < launchdRecoveryBurstLimit; attempt++ {
		err := boundLaunchdRecovery(context.Background(), invocation, controller,
			func() time.Time { return start.Add(time.Duration(attempt) * time.Second) }, run)
		require.ErrorIs(t, err, supervisorFailure)
	}
	require.Equal(t, launchdRecoveryBurstLimit-1, runCalls)
	require.Empty(t, runtime.commands)

	err := boundLaunchdRecovery(context.Background(), invocation, controller,
		func() time.Time { return start.Add(launchdRecoveryBurstLimit * time.Second) }, run)
	require.NoError(t, err, "the burst limit must stand the actor down (exit 0) so KeepAlive.SuccessfulExit=false stops relaunching")
	require.Equal(t, launchdRecoveryBurstLimit, runCalls, "the supervisor must run exactly once per failure")
	require.Equal(t, []string{"disable", "gui/501/" + launchdRecoveryJobName(invocation.TransactionID)},
		runtime.commands[0].args)

	// A reboot's RunAtLoad: the pre-check must stand down WITHOUT re-entering
	// the supervisor, so the next boot cannot restart the loop.
	err = boundLaunchdRecovery(context.Background(), invocation, controller,
		func() time.Time { return start.Add((launchdRecoveryBurstLimit + 1) * time.Second) }, run)
	require.NoError(t, err)
	require.Equal(t, launchdRecoveryBurstLimit, runCalls,
		"the pre-check must stand down before re-entering the supervisor; a reboot must not restart the loop")

	// A reboot later than the burst interval must ALSO stand down: the
	// Exhausted marker is sticky, so a once-exhausted loop cannot restart on
	// the next boot even after the window elapsed.
	err = boundLaunchdRecovery(context.Background(), invocation, controller,
		func() time.Time { return start.Add(launchdRecoveryBurstInterval + time.Hour) }, run)
	require.NoError(t, err)
	require.Equal(t, launchdRecoveryBurstLimit, runCalls,
		"the sticky exhausted marker must keep a once-exhausted loop stood down past the burst window")
}

// TestBoundLaunchdRecoveryPreRecordsBeforeRunSoAPanicCounts guards the P1
// property: the attempt is persisted BEFORE the actor runs, so a panic, fatal
// runtime error, or signal termination (which never returns to a post-run
// record) still counts toward the budget. A run that "does not return" leaves
// the counter incremented, and the next launchd relaunch sees it via the
// pre-check (or the pre-record) and progresses the budget toward stand-down
// rather than relaunching unboundedly.
func TestBoundLaunchdRecoveryPreRecordsBeforeRunSoAPanicCounts(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{launchdUserID: 501}
	controller := runtime.controller()
	start := time.Now()

	// Simulate a panic/signal-kill: run increments a side counter (so we can
	// observe it ran) then returns an error standing in for "the actor did not
	// return normally." The pre-record must have persisted the attempt before
	// this returns, so the budget progressed even though the post-run path was
	// never reached cleanly.
	ran := false
	panickedRun := func() error {
		ran = true
		return errors.New("actor killed by signal before recording")
	}
	require.Error(t, boundLaunchdRecovery(context.Background(), invocation, controller,
		func() time.Time { return start }, panickedRun))
	require.True(t, ran)
	counter, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.True(t, ok, "the attempt must be pre-recorded before the actor runs")
	require.Equal(t, 1, counter.Attempts)
	require.False(t, counter.Exhausted, "a single attempt must not exhaust the budget")
}

// TestBoundLaunchdRecoveryReconcilesCounterOnSuccess guards the success path:
// the pre-recorded attempt is cleared when the actor returns nil, so the
// budget resets on a successful recovery (the way systemd's window rolls
// forward on success) and no stand-down marker is left behind. Terminal
// cleanup removes the transaction directory anyway, but the clear keeps the
// budget honest between success and cleanup.
func TestBoundLaunchdRecoveryReconcilesCounterOnSuccess(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{launchdUserID: 501}
	controller := runtime.controller()

	// Pre-record one attempt, then succeed: the counter must be cleared.
	require.NoError(t, recordLaunchdRecoveryAttempt(invocation, time.Now()))
	_, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.True(t, ok)

	require.NoError(t, boundLaunchdRecovery(context.Background(), invocation, controller, time.Now,
		func() error { return nil }))

	_, ok = readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.False(t, ok, "a successful run must clear the pre-recorded counter so the budget resets")
	require.Empty(t, runtime.commands, "a successful run must not disable the job")
}

// TestBoundLaunchdRecoveryPassthroughForNonLaunchd guards the no-op for the
// two kinds the budget must never touch: systemd (self-bounds via its unit's
// StartLimit* directives) and detached (a one-shot the manager never
// relaunches). For both, the error passes straight through every time — no
// counter, no launchctl disable — so the actor-side budget can never
// accidentally suppress or stand-down a non-launchd job.
func TestBoundLaunchdRecoveryPassthroughForNonLaunchd(t *testing.T) {
	for _, tc := range []struct {
		kind RecoveryJobKind
		name string
	}{
		{kind: RecoveryJobSystemd, name: "systemd"},
		{kind: RecoveryJobDetached, name: "detached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			txn := prepareRecoveryJobFixture(t, tc.kind)
			invocation := RecoveryInvocation{HomeDir: txn.Journal().HomeDir, TransactionID: txn.Journal().ID}
			runtime := &fakeRecoveryJobRuntime{}
			controller := runtime.controller()

			supervisorFailure := errors.New(tc.name + " actor failure")
			run := func() error { return supervisorFailure }

			for i := 0; i < launchdRecoveryBurstLimit*3; i++ {
				err := boundLaunchdRecovery(context.Background(), invocation, controller, time.Now, run)
				require.ErrorIs(t, err, supervisorFailure)
			}
			require.Empty(t, runtime.commands, "no launchctl disable for a %s job", tc.name)
			_, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
			require.False(t, ok, "no retry counter for a non-launchd job")
		})
	}
}

// TestEnsureRecoveryRetryCounterDirDoesNotRecreateRemovedHome pins the P2
// fix: ensureRecoveryRetryCounterDir returns errAFHomeRemoved (rather than
// recreating the tree) when the upgrade root is gone, so a stale launchd job
// whose AF home was uninstalled or intentionally removed does not get the
// deleted <home>/upgrade/transactions/<id> hierarchy recreated under it; and
// it still creates the transaction leaf when only the transaction directory is
// missing but the upgrade root survives (the adversarial "only active.json"
// shape). recordLaunchdRecoveryAttempt surfaces the same sentinel so
// boundLaunchdRecovery can stand the stale job down (exit 0) without recreating
// anything — a branch reached only on Darwin, where recoveryJobIsLaunchd's
// fallback yields true when the journal cannot load; on Linux the budget is a
// no-op when the journal cannot load, so this test covers the sentinel at the
// unit level the Darwin CI cell exercises through boundLaunchdRecovery.
func TestEnsureRecoveryRetryCounterDirDoesNotRecreateRemovedHome(t *testing.T) {
	invocation := launchdBoundFixture(t)

	// Adversarial shape: the transaction directory is gone but the upgrade
	// root survives. The leaf must be recreated without recreating the home.
	require.NoError(t, os.RemoveAll(transactionDir(invocation.HomeDir, invocation.TransactionID)))
	require.NoError(t, ensureRecoveryRetryCounterDir(invocation.HomeDir, invocation.TransactionID))
	info, err := os.Stat(transactionDir(invocation.HomeDir, invocation.TransactionID))
	require.NoError(t, err)
	require.True(t, info.IsDir())

	// Removed home: the upgrade root is gone. The budget must not recreate it.
	require.NoError(t, os.RemoveAll(upgradeRoot(invocation.HomeDir)))
	require.ErrorIs(t, ensureRecoveryRetryCounterDir(invocation.HomeDir, invocation.TransactionID), errAFHomeRemoved)
	_, statErr := os.Stat(upgradeRoot(invocation.HomeDir))
	require.ErrorIs(t, statErr, os.ErrNotExist, "the upgrade root must not be recreated")

	// The pre-record surfaces the same sentinel so boundLaunchdRecovery can
	// convert it to a stand-down (exit 0) without running the actor.
	require.ErrorIs(t, recordLaunchdRecoveryAttempt(invocation, time.Now()), errAFHomeRemoved)
	_, statErr = os.Stat(upgradeRoot(invocation.HomeDir))
	require.ErrorIs(t, statErr, os.ErrNotExist, "the pre-record must not recreate the removed home")
}

// TestRecoveryRetryCounterRemovedWithTransactionDirectory confirms the counter
// rides in the transaction directory so terminal cleanup (which removes the
// directory) resets the budget for the next upgrade and leaves no stranded
// stand-down marker. The path lives under the transaction dir, so no separate
// cleanup entry is needed.
func TestRecoveryRetryCounterRemovedWithTransactionDirectory(t *testing.T) {
	invocation := launchdBoundFixture(t)
	require.NoError(t, writeRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID,
		recoveryRetryCounter{Attempts: launchdRecoveryBurstLimit, FirstAttemptAt: time.Now(), Exhausted: true}))
	_, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.True(t, ok)

	require.NoError(t, os.RemoveAll(transactionDir(invocation.HomeDir, invocation.TransactionID)))

	_, ok = readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.False(t, ok, "removing the transaction directory must remove the counter; the budget resets with the transaction")
}

// TestActiveTransactionStillNamesInvocation pins the gate that stops a stale
// launchd job from recreating a transaction directory the operator deleted or
// replaced (a P2 finding from review): a gone active.json (ErrNoActiveTransaction,
// the terminal-cleanup shape) and a superseding transaction (a different id) both
// return false so the pre-record does not run, while a matching id and a corrupt
// journal (the bug report's primary persistent trigger, a non-ErrNoActiveTransaction
// Load failure) return true so the budget still bounds it.
func TestActiveTransactionStillNamesInvocation(t *testing.T) {
	invocation := launchdBoundFixture(t)

	// The active transaction still names this invocation.
	require.True(t, activeTransactionStillNamesInvocation(invocation),
		"a matching active transaction must name the invocation so the budget applies")

	// A newer transaction superseded it.
	other := invocation
	other.TransactionID = invocation.TransactionID + "-superseded"
	require.False(t, activeTransactionStillNamesInvocation(other),
		"a superseding transaction must not name the stale invocation so the pre-record does not recreate the leaf")

	// Terminal cleanup removed active.json: the budget must not recreate the
	// transaction directory, and recoveryJobIsLaunchd's OS fallback cannot
	// tell this case apart from a corrupt journal on its own.
	require.NoError(t, os.Remove(activeJournalPath(invocation.HomeDir)))
	require.False(t, activeTransactionStillNamesInvocation(invocation),
		"a gone active.json must not name the invocation so the stale job stands down without recreating anything")

	// A corrupt journal is a persistent trigger the budget must still bound.
	require.NoError(t, os.WriteFile(activeJournalPath(invocation.HomeDir), []byte("{not-valid-json"), 0o600))
	require.True(t, activeTransactionStillNamesInvocation(invocation),
		"a corrupt journal (non-ErrNoActiveTransaction) must still name the invocation so the budget applies")
}

// TestBoundLaunchdRecoveryDoesNotRecreateSupersededTransaction pins the P2
// fix for the stale-final-restart shape: when the active journal names a
// different transaction (a newer transaction replaced the one this stale job
// was armed for), the pre-record must not recreate the replaced
// transactions/<id> directory the actor's stale-job path does not clean up.
// recoveryJobIsLaunchd loads the journal and returns true (the kind is still
// launchd), so the budget path runs; activeTransactionStillNamesInvocation
// gates the pre-record and stands the stale job down (exit 0) without writing a
// counter or recreating the leaf. The gone-active.json shape (terminal cleanup
// removed active.json) is the same gate on Darwin, where recoveryJobIsLaunchd
// falls back to the OS; it is covered by TestActiveTransactionStillNamesInvocation
// at the unit level because on Linux that fallback yields false and the budget
// is a no-op there.
func TestBoundLaunchdRecoveryDoesNotRecreateSupersededTransaction(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{launchdUserID: 501}
	controller := runtime.controller()

	// The active journal names a different id than the stale job's invocation.
	// The stale transaction directory does not exist (a newer transaction
	// replaced it and terminal cleanup removed it). The pre-record must not
	// recreate it, and the actor must not run.
	stale := RecoveryInvocation{HomeDir: invocation.HomeDir, TransactionID: "stale-transaction-id"}
	staleDir := transactionDir(invocation.HomeDir, "stale-transaction-id")
	_, statErr := os.Stat(staleDir)
	require.ErrorIs(t, statErr, os.ErrNotExist, "the stale transaction directory must not exist at the start")
	ran := false
	err := boundLaunchdRecovery(context.Background(), stale, controller, time.Now, func() error {
		ran = true
		return nil
	})
	require.NoError(t, err, "a stale job naming a superseded transaction must stand down (exit 0)")
	require.False(t, ran, "the actor must not run for a stale job naming a superseded transaction")
	require.Empty(t, runtime.commands, "no launchctl disable is needed when no attempt was recorded")
	_, counterExists := readRecoveryRetryCounter(invocation.HomeDir, "stale-transaction-id")
	require.False(t, counterExists, "the pre-record must not write a counter for a superseded transaction")
	_, statErr = os.Stat(staleDir)
	require.ErrorIs(t, statErr, os.ErrNotExist, "the stale transaction directory must not be recreated by the pre-record")
}

// TestBoundLaunchdRecoveryExhaustedPreCheckDisablesJob pins the P2 fix for the
// crash-on-the-final-attempt shape: if the third attempt pre-records the
// exhausted marker but panics or is killed before the post-run stand-down runs
// launchctl disable, the next launch's pre-check must disable the job. Exiting
// 0 alone stops the immediate relaunch, but the job remains enabled and would
// run again at every login or reboot without the disable.
func TestBoundLaunchdRecoveryExhaustedPreCheckDisablesJob(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{launchdUserID: 501}
	controller := runtime.controller()
	start := time.Now()

	// Pre-record the exhausting attempt (simulating a panic on the third
	// attempt after the pre-record but before the post-run stand-down). The
	// counter is exhausted on disk but the job was never disabled.
	require.NoError(t, recordLaunchdRecoveryAttempt(invocation, start))
	require.NoError(t, recordLaunchdRecoveryAttempt(invocation, start.Add(time.Second)))
	require.NoError(t, recordLaunchdRecoveryAttempt(invocation, start.Add(2*time.Second)))
	counter, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.True(t, ok)
	require.True(t, counter.Exhausted)
	require.Empty(t, runtime.commands, "the crash happened before the post-run stand-down, so the job is still enabled")

	// The next launch enters the pre-check, which must disable the job rather
	// than only returning nil.
	ran := false
	err := boundLaunchdRecovery(context.Background(), invocation, controller,
		func() time.Time { return start.Add(3 * time.Second) }, func() error {
			ran = true
			return nil
		})
	require.NoError(t, err, "an exhausted budget must stand the actor down (exit 0)")
	require.False(t, ran, "the pre-check must stand down without re-entering the supervisor")
	require.Equal(t, []recoveryCommandCall{{
		name: "launchctl",
		args: []string{"disable", "gui/501/" + launchdRecoveryJobName(invocation.TransactionID)},
	}}, runtime.commands, "the pre-check must disable the job so a crash on the final attempt cannot leave it armed")
}

// TestBoundLaunchdRecoveryStandsDownWhenCounterUnwriteable pins the P1 fix for
// the unwriteable-counter shape: when the counter cannot be persisted for a
// reason that does not itself cap resource use (a read-only transaction
// directory, lost write permission, a persistent filesystem I/O error), the
// budget can never advance and launchd would relaunch every ThrottleInterval
// forever. The actor must stand down (disable + exit 0) so the loop stops,
// rather than returning the error and relaunching unboundedly.
func TestBoundLaunchdRecoveryStandsDownWhenCounterUnwriteable(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{launchdUserID: 501}
	controller := runtime.controller()

	// Block the counter write without breaking the journal load: a non-empty
	// directory at the counter path makes the durable atomic rename fail
	// (rename(2) refuses to replace a non-empty directory), so the counter
	// cannot be persisted. The transaction directory is still a valid
	// directory, so the journal still loads and recoveryJobIsLaunchd returns
	// true (launchd); the budget path runs and the pre-record fails.
	counterPath := recoveryRetryCounterPath(invocation.HomeDir, invocation.TransactionID)
	require.NoError(t, os.MkdirAll(counterPath, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(counterPath, "blocker"), []byte("x"), 0o600))

	ran := false
	err := boundLaunchdRecovery(context.Background(), invocation, controller, time.Now, func() error {
		ran = true
		return nil
	})
	require.NoError(t, err, "an unwriteable counter must not relaunch the actor unboundedly; it must stand down (exit 0)")
	require.False(t, ran, "the actor must not run when the counter cannot be persisted; the job stands down instead")
	require.Equal(t, []recoveryCommandCall{{
		name: "launchctl",
		args: []string{"disable", "gui/501/" + launchdRecoveryJobName(invocation.TransactionID)},
	}}, runtime.commands, "an unwriteable counter must disable the job so the loop stops")
}

// TestBoundLaunchdRecoveryStandsDownWhenCounterUnwriteableAndDisableFails guards
// that an unwriteable counter still stands down (exit 0) even when launchctl
// disable itself fails: exiting 0 alone stops the immediate relaunch
// (KeepAlive.SuccessfulExit=false), and a disable failure must not keep the
// loop alive.
func TestBoundLaunchdRecoveryStandsDownWhenCounterUnwriteableAndDisableFails(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{
		launchdUserID: 501,
		commandErr:    errors.New("launchctl persistently broken"),
	}
	controller := runtime.controller()

	counterPath := recoveryRetryCounterPath(invocation.HomeDir, invocation.TransactionID)
	require.NoError(t, os.MkdirAll(counterPath, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(counterPath, "blocker"), []byte("x"), 0o600))

	err := boundLaunchdRecovery(context.Background(), invocation, controller, time.Now, func() error {
		return errors.New("actor must not run")
	})
	require.NoError(t, err, "an unwriteable counter must stand down (exit 0) even when the disable fails")
	require.Len(t, runtime.commands, 1)
	require.Equal(t, "disable", runtime.commands[0].args[0])
}

// TestEnsureRecoveryRetryCounterDirRejectsSymlinkedTransactionsParent pins the
// P2 fix: when <home>/upgrade/transactions has been replaced by a symlink,
// validating only upgradeRoot does not detect it, and a recursive MkdirAll
// would follow the symlink and create the counter outside the AF home. The
// counter dir creation must validate the transactions parent as a real
// directory and create only the immediate leaf, the way the recovery-lock
// recreation path does, so a symlinked transactions root is rejected.
func TestEnsureRecoveryRetryCounterDirRejectsSymlinkedTransactionsParent(t *testing.T) {
	invocation := launchdBoundFixture(t)
	txnDir := transactionDir(invocation.HomeDir, invocation.TransactionID)
	require.NoError(t, os.RemoveAll(txnDir))
	transactionsRoot := filepath.Dir(txnDir)
	require.NoError(t, os.RemoveAll(transactionsRoot))

	// Replace <home>/upgrade/transactions with a symlink to a directory outside
	// the AF home, so a recursive MkdirAll would create the counter there.
	outsideHome := t.TempDir()
	outsideTarget := filepath.Join(outsideHome, "evil-transactions")
	require.NoError(t, os.MkdirAll(outsideTarget, 0o700))
	require.NoError(t, os.Symlink(outsideTarget, transactionsRoot))

	err := ensureRecoveryRetryCounterDir(invocation.HomeDir, invocation.TransactionID)
	require.Error(t, err, "a symlinked transactions root must be rejected")
	require.ErrorContains(t, err, "transactions root")

	// The counter must not be created outside the AF home.
	_, statErr := os.Stat(filepath.Join(outsideTarget, invocation.TransactionID))
	require.ErrorIs(t, statErr, os.ErrNotExist,
		"a symlinked transactions root must not let the counter be created outside the AF home")

	// The symlinked transactions root itself must be left intact (not removed
	// or replaced); only the immediate leaf would have been created.
	info, err := os.Lstat(transactionsRoot)
	require.NoError(t, err)
	require.True(t, info.Mode()&os.ModeSymlink != 0, "the symlinked transactions root must not be replaced")
}

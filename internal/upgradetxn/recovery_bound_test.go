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
// rolls forward (resets), and a corrupt counter resets rather than stranding
// the actor in stand-down forever.
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
		"after the burst interval elapses the budget resets")

	// A corrupt counter resets the budget rather than stranding the actor.
	require.NoError(t, os.WriteFile(recoveryRetryCounterPath(invocation.HomeDir, invocation.TransactionID),
		[]byte("{not-valid"), 0o600))
	require.False(t, launchdRecoveryBudgetExhausted(invocation, time.Now()),
		"a corrupt counter must reset the budget, not strand the actor in stand-down")
}

// TestRecordLaunchdRecoveryFailureIncrementsAndStandsDownAtBurstLimit is the
// core stand-down guard: the first N-1 failures return (false, nil) with no
// launchctl (the actor keeps exiting non-zero so launchd keeps retrying, as
// systemd would below StartLimitBurst), and the Nth within the window returns
// (true, nil), fires launchctl disable on the deterministic label, and
// persists Attempts=N. It also covers the interval-reset (a failure after the
// interval starts a fresh window with Attempts=1).
func TestRecordLaunchdRecoveryFailureIncrementsAndStandsDownAtBurstLimit(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{launchdUserID: 501}
	controller := runtime.controller()
	start := time.Now()

	for attempt := 1; attempt < launchdRecoveryBurstLimit; attempt++ {
		stoodDown, err := recordLaunchdRecoveryFailure(context.Background(), invocation, controller,
			start.Add(time.Duration(attempt)*time.Second))
		require.NoError(t, err)
		require.Falsef(t, stoodDown, "attempt %d must not stand down; the burst limit has not been reached", attempt)
		require.Empty(t, runtime.commands, "the disable must not fire before the budget is exhausted")
	}

	stoodDown, err := recordLaunchdRecoveryFailure(context.Background(), invocation, controller,
		start.Add(launchdRecoveryBurstLimit*time.Second))
	require.NoError(t, err)
	require.True(t, stoodDown, "the third failure in the interval must stand down, mirroring StartLimitBurst=3")
	require.Equal(t, []recoveryCommandCall{{
		name: "launchctl",
		args: []string{"disable", "gui/501/" + launchdRecoveryJobName(invocation.TransactionID)},
	}}, runtime.commands, "the stand-down must disable the job by its deterministic label")

	counter, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.True(t, ok)
	require.Equal(t, launchdRecoveryBurstLimit, counter.Attempts)

	// After the interval elapses the next failure starts a fresh window with
	// Attempts=1 — StartLimitIntervalSec rolls the window forward.
	stoodDown, err = recordLaunchdRecoveryFailure(context.Background(), invocation, controller,
		start.Add(launchdRecoveryBurstInterval+5*time.Second))
	require.NoError(t, err)
	require.False(t, stoodDown, "the first failure after the interval elapses starts a fresh window")
	counter, ok = readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.True(t, ok)
	require.Equal(t, 1, counter.Attempts, "the burst window resets to a single attempt after the interval elapses")
}

// TestRecordLaunchdRecoveryFailureStandsDownEvenWhenDisableFails guards the
// key invariant: the stand-down (exit 0) is NOT gated on the disable
// succeeding. A persistent launchctl failure is exactly the loop the budget
// bounds — not one that should keep it alive — and the persisted counter
// ensures the next boot's pre-check stands down without retrying even when the
// disable could not stick.
func TestRecordLaunchdRecoveryFailureStandsDownEvenWhenDisableFails(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{
		launchdUserID: 501,
		commandErr:    errors.New("launchctl persistently broken"),
	}
	controller := runtime.controller()
	start := time.Now()

	for attempt := 1; attempt < launchdRecoveryBurstLimit; attempt++ {
		stoodDown, err := recordLaunchdRecoveryFailure(context.Background(), invocation, controller,
			start.Add(time.Duration(attempt)*time.Second))
		require.NoError(t, err)
		require.False(t, stoodDown)
	}
	stoodDown, err := recordLaunchdRecoveryFailure(context.Background(), invocation, controller,
		start.Add(launchdRecoveryBurstLimit*time.Second))
	require.NoError(t, err, "a disable failure must not surface as an error; the actor would then exit 1 and relaunch")
	require.True(t, stoodDown, "the disable failure must not prevent the stand-down")
	require.Equal(t, []string{"disable", "gui/501/" + launchdRecoveryJobName(invocation.TransactionID)},
		runtime.commands[0].args)
}

// TestRecordLaunchdRecoveryFailureReturnsErrorWhenCounterUnwriteable guards
// the disk-full/ledger-broken shape from the bug report: the counter write
// fails, the actor returns the error (exit 1, relaunch) and the budget does
// not stand down. The disable must not fire when the counter could not be
// recorded. This is the one residual gap vs systemd's in-memory tracking, and
// it self-limits — a full disk also bounds recovery.log growth.
func TestRecordLaunchdRecoveryFailureReturnsErrorWhenCounterUnwriteable(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{launchdUserID: 501}
	controller := runtime.controller()

	// A symlinked transaction directory is rejected by validateDirectoryNoSymlink,
	// so the counter cannot be persisted and the budget cannot progress.
	txnDir := transactionDir(invocation.HomeDir, invocation.TransactionID)
	require.NoError(t, os.RemoveAll(txnDir))
	require.NoError(t, os.Symlink(t.TempDir(), txnDir))

	stoodDown, err := recordLaunchdRecoveryFailure(context.Background(), invocation, controller, time.Now())
	require.Error(t, err)
	require.ErrorContains(t, err, "not a real directory")
	require.False(t, stoodDown, "when the counter cannot persist the budget must not stand down; the actor keeps retrying")
	require.Empty(t, runtime.commands, "the disable must not fire when the counter could not be recorded")
}

// TestBoundLaunchdRecoveryStandsDownAfterBurstLimitAndPreventsNextRun is the
// end-to-end wrapper guard: the first N-1 failing runs pass the supervisor
// error through (exit 1), the Nth stands down (nil so the actor exits 0) and
// fires launchctl disable, and the (N+1)th invocation — a reboot's RunAtLoad —
// stands down via the pre-check WITHOUT re-entering the supervisor, so a reboot
// cannot restart the loop. That last assertion is the property systemd's "a
// failed unit stays failed until reset" gives for free and launchd does not.
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
}

// TestBoundLaunchdRecoveryReturnsNilOnSuccessWithoutRecording guards that a
// successful recovery records no attempt and touches no service manager — the
// budget must not record a failure or persist a counter on the happy path.
func TestBoundLaunchdRecoveryReturnsNilOnSuccessWithoutRecording(t *testing.T) {
	invocation := launchdBoundFixture(t)
	runtime := &fakeRecoveryJobRuntime{launchdUserID: 501}
	controller := runtime.controller()

	require.NoError(t, boundLaunchdRecovery(context.Background(), invocation, controller, time.Now,
		func() error { return nil }))

	_, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.False(t, ok, "a successful run must not record a failure or persist a counter")
	require.Empty(t, runtime.commands)
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

// TestRecoveryRetryCounterRemovedWithTransactionDirectory confirms the counter
// rides in the transaction directory so terminal cleanup (which removes the
// directory) resets the budget for the next upgrade and leaves no stranded
// stand-down marker. The path lives under the transaction dir, so no separate
// cleanup entry is needed.
func TestRecoveryRetryCounterRemovedWithTransactionDirectory(t *testing.T) {
	invocation := launchdBoundFixture(t)
	require.NoError(t, writeRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID,
		recoveryRetryCounter{Attempts: launchdRecoveryBurstLimit, FirstAttemptAt: time.Now()}))
	_, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.True(t, ok)

	require.NoError(t, os.RemoveAll(transactionDir(invocation.HomeDir, invocation.TransactionID)))

	_, ok = readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.False(t, ok, "removing the transaction directory must remove the counter; the budget resets with the transaction")
}

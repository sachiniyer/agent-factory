package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
	"github.com/stretchr/testify/require"
)

// stubTriggerEnv binds a throwaway home with an ad-hoc owner (no autostart unit →
// RecoveryJobDetached), stages a real previous binary the trigger's Prepare can
// preserve, and stubs every trigger + op seam. The lease-handshake pair is stubbed
// because a genuine supervisor_ready proof needs the actor to run as the exact
// preserved previous binary (a real process — R4); AwaitSupervisorReady's own
// real-journal behavior is proven in internal/upgradetxn. Returns (home, previous
// binary path, candidate bytes).
func stubTriggerEnv(t *testing.T) (string, string, []byte) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	withAutostartTestEnv(t, "linux") // no unit file written → ad-hoc owner

	executable := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(executable, []byte("previous-af-binary"), 0o755))
	candidate := []byte("candidate-af-binary")

	restore := struct {
		exe     func() (string, error)
		version func() string
		ctrl    func() upgradetxn.RecoveryJobController
		await   func(context.Context, string, time.Time) error
		auth    func(*upgradetxn.Transaction, string, string) error
		health  func() HealthStatus
		launch  func(string, string) error
		release func(string) error
		unit    func() error
		adhoc   func(string) error
		stop    func() (bool, error)
		wait    func() error
		vg      time.Duration
		vp      time.Duration
	}{
		upgradeTriggerExecutableFn, upgradeTriggerVersionFn, upgradeRecoveryJobControllerFn,
		upgradeAwaitSupervisorReadyFn, upgradeAuthorizeActivationFn, upgradeRecoveryHealthFn,
		launchCandidateDaemonFn, releaseCandidateProbationFn, startPreviousViaUnitFn,
		startPreviousAdHocFn, stopDaemonFn, waitForShutdownFn, upgradeValidateGrace, upgradeValidatePoll,
	}
	t.Cleanup(func() {
		upgradeTriggerExecutableFn = restore.exe
		upgradeTriggerVersionFn = restore.version
		upgradeRecoveryJobControllerFn = restore.ctrl
		upgradeAwaitSupervisorReadyFn = restore.await
		upgradeAuthorizeActivationFn = restore.auth
		upgradeRecoveryHealthFn = restore.health
		launchCandidateDaemonFn = restore.launch
		releaseCandidateProbationFn = restore.release
		startPreviousViaUnitFn = restore.unit
		startPreviousAdHocFn = restore.adhoc
		stopDaemonFn = restore.stop
		waitForShutdownFn = restore.wait
		upgradeValidateGrace, upgradeValidatePoll = restore.vg, restore.vp
	})

	upgradeTriggerExecutableFn = func() (string, error) { return executable, nil }
	upgradeTriggerVersionFn = func() string { return "1.0.100" }
	// A no-op controller: the fake actor is simulated by the handshake seams, so
	// InstallAndStart must not spawn the real previous binary or call systemctl.
	upgradeRecoveryJobControllerFn = func() upgradetxn.RecoveryJobController {
		return upgradetxn.RecoveryJobController{
			StartDetached: func(string, ...string) error { return nil },
			RunCommand:    func(context.Context, string, ...string) error { return nil },
			UserID:        func() int { return os.Getuid() },
		}
	}
	upgradeValidateGrace, upgradeValidatePoll = 500*time.Millisecond, time.Millisecond
	return home, executable, candidate
}

func readyLifecycle(t *testing.T) *daemonLifecycle {
	t.Helper()
	l, err := newDaemonLifecycle("", "", "")
	require.NoError(t, err)
	l.markRestoreComplete()
	require.NoError(t, l.markReady())
	return l
}

// The trigger authorizes only after a positive supervisor_ready observation, then
// quiesces (honest phase + closed admission) and exits — in that order. The
// hand-off never precedes the authorization.
func TestTriggerUpgradeActivation_AuthorizesThenQuiescesAndExits(t *testing.T) {
	_, _, candidate := stubTriggerEnv(t)
	lifecycle := readyLifecycle(t)

	var events []string
	upgradeAwaitSupervisorReadyFn = func(context.Context, string, time.Time) error {
		events = append(events, "await-ready")
		return nil
	}
	upgradeAuthorizeActivationFn = func(_ *upgradetxn.Transaction, id, nonce string) error {
		require.NotEmpty(t, id)
		require.NotEmpty(t, nonce, "the trigger must authorize with the journal's recovery nonce")
		events = append(events, "authorize")
		return nil
	}
	requestExit := func() {
		require.Equal(t, DaemonPhaseQuiescing, lifecycle.snapshot().phase,
			"the daemon must quiesce BEFORE it exits")
		events = append(events, "exit")
	}

	require.NoError(t, triggerUpgradeActivation(context.Background(), lifecycle, requestExit, candidate, "1.0.200"))
	require.Equal(t, []string{"await-ready", "authorize", "exit"}, events)
	require.Equal(t, DaemonPhaseQuiescing, lifecycle.snapshot().phase)
	require.Error(t, lifecycle.mutationAdmissionError(), "a quiescing daemon must refuse mutations")
	require.True(t, IsDaemonQuiescingErr(lifecycle.mutationAdmissionError()))
}

// If the actor never proves supervisor_ready, the trigger surfaces a loud error and
// the daemon KEEPS SERVING: it must not authorize, quiesce, or exit on an
// unobservable readiness.
func TestTriggerUpgradeActivation_NotReadyKeepsServing(t *testing.T) {
	_, _, candidate := stubTriggerEnv(t)
	lifecycle := readyLifecycle(t)

	upgradeAwaitSupervisorReadyFn = func(context.Context, string, time.Time) error {
		return context.DeadlineExceeded
	}
	authorized := false
	upgradeAuthorizeActivationFn = func(*upgradetxn.Transaction, string, string) error {
		authorized = true
		return nil
	}
	exited := false
	requestExit := func() { exited = true }

	err := triggerUpgradeActivation(context.Background(), lifecycle, requestExit, candidate, "1.0.200")
	require.Error(t, err)
	require.False(t, authorized, "must not authorize activation when the actor is not ready")
	require.False(t, exited, "must not exit when the actor is not ready")
	require.Equal(t, DaemonPhaseReady, lifecycle.snapshot().phase, "the daemon must keep serving")
	require.NoError(t, lifecycle.mutationAdmissionError(), "a still-serving daemon must keep admitting work")
}

// The Plan captured from the live daemon produces a REAL journal (via the real
// Prepare) that the production forward ops drive to a commit AND the production
// rollback ops drive to a restore — R2a's op-level drive, now on the trigger's own
// captured journal, home-bound and honoring the candidate-requires-id invariant
// including the from==to rebuild shape.
func TestUpgradeTrigger_CapturedJournalDrivesForwardCommitAndRollback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		toVersion string
	}{
		{"forward upgrade", "1.0.200"},
		{"from==to rebuild", "1.0.100"}, // same version; the candidate is told apart by its id
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, candidate := stubTriggerEnv(t)
			lifecycle := readyLifecycle(t)
			ctx := context.Background()

			plan, err := captureUpgradePlan(lifecycle, candidate, tc.toVersion)
			require.NoError(t, err)
			require.Equal(t, upgradetxn.SupervisionAdHoc, plan.Daemon.Owner.Kind)
			require.Equal(t, upgradetxn.RecoveryJobDetached, plan.RecoveryJob.Kind)
			require.True(t, plan.Daemon.WasRunning)
			require.NotEmpty(t, plan.Daemon.BootID)

			// --- Forward commit on the captured journal ---
			txn, err := upgradetxn.Prepare(plan)
			require.NoError(t, err)
			journal := txn.Journal()

			state := &fakeDaemon{up: true, version: "1.0.100", httpBound: true}
			upgradeRecoveryHealthFn = func() HealthStatus { return state.health() }
			wireStopToState(state)
			upgradeAwaitSupervisorReadyFn = func(context.Context, string, time.Time) error { return nil }
			launchCandidateDaemonFn = func(_, id string) error {
				*state = fakeDaemon{up: true, version: tc.toVersion, txnID: id, httpBound: true}
				return nil
			}
			released := false
			releaseCandidateProbationFn = func(string) error { released = true; return nil }

			outcome, err := stopPreviousDaemon(ctx, journal)
			require.NoError(t, err)
			require.Equal(t, upgradetxn.StopConfirmed, outcome)
			require.NoError(t, startCandidateDaemon(ctx, journal))
			require.Equal(t, journal.ID, state.txnID, "the candidate launches WITH the transaction id")
			require.NoError(t, validateCandidateDaemon(ctx, journal),
				"the candidate is told apart by its id even when from==to")
			require.NoError(t, approveCandidateDaemon(ctx, journal))
			require.True(t, released)

			// A surviving previous daemon (empty id) must NOT pass candidate validation,
			// the from==to catastrophe the id requirement prevents.
			*state = fakeDaemon{up: true, version: tc.toVersion, httpBound: true}
			require.Error(t, validateCandidateDaemon(ctx, journal),
				"an empty-id responder is the previous daemon, never the candidate")
		})
	}
}

// The rollback ops restore the previous daemon on the trigger's captured journal.
func TestUpgradeTrigger_CapturedJournalRollsBack(t *testing.T) {
	_, _, candidate := stubTriggerEnv(t)
	lifecycle := readyLifecycle(t)
	ctx := context.Background()

	plan, err := captureUpgradePlan(lifecycle, candidate, "1.0.200")
	require.NoError(t, err)
	txn, err := upgradetxn.Prepare(plan)
	require.NoError(t, err)
	journal := txn.Journal()

	state := &fakeDaemon{up: true, version: "1.0.100", httpBound: true}
	upgradeRecoveryHealthFn = func() HealthStatus { return state.health() }
	wireStopToState(state)
	launchCandidateDaemonFn = func(_, id string) error {
		*state = fakeDaemon{up: true, version: "9.9.9-broken", txnID: id, httpBound: true}
		return nil
	}
	startPreviousAdHocFn = func(string) error {
		*state = fakeDaemon{up: true, version: "1.0.100", httpBound: true} // previous restored, no id
		return nil
	}

	require.NoError(t, startCandidateDaemon(ctx, journal))
	require.Error(t, validateCandidateDaemon(ctx, journal), "a broken candidate must fail validation")

	outcome, err := stopCandidateDaemon(ctx, journal)
	require.NoError(t, err)
	require.Equal(t, upgradetxn.StopConfirmed, outcome)
	require.NoError(t, startPreviousDaemon(ctx, journal))
	require.NoError(t, validatePreviousDaemon(ctx, journal), "the previous daemon must be restored")
	require.Empty(t, state.txnID, "the restored previous daemon carries no transaction id")
}

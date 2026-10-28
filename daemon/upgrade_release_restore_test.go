package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// #5693: the previous-binary supervisor validates a candidate, commits, and
// approves it in one breath. The candidate binds its control socket before it
// restores instances, so validation could pass mid-restore, and the candidate
// then REFUSED the release ("cannot release upgrade probation before instance
// restore completes"). The approve is single-shot and the actor's only retry is
// a systemd restart budget (StartLimitBurst=3 / RestartSec=2) sized for a crash
// loop, so a restore slower than a few seconds wedged the upgrade permanently:
// the candidate parked in probation forever and the committed journal blocked
// every later `af upgrade`.
//
// Two layers close it, each pinned below on its own: the candidate accepts a
// release that races its restore and applies it when the restore completes (the
// candidate is the NEW binary, so this protects even an upgrade supervised by an
// older, unfixed binary), and candidate validation waits for the restore (the
// supervisor is the PREVIOUS binary, so this protects upgrades from this version
// on — the release then never races the restore at all).

// candidateHealth is what the supervisor's health probe sees from a candidate
// whose admission state is a REAL daemonLifecycle — phase, transaction id, and
// release semantics are production, only the transport is faked.
func candidateHealth(l *daemonLifecycle, version string) HealthStatus {
	snap := l.snapshot()
	return HealthStatus{
		DaemonVersion: version,
		TransactionID: snap.transactionID,
		Phase:         snap.phase,
		Listeners:     DaemonListenerStatus{HTTPUnixBound: true},
	}
}

func TestReleaseDuringRestoreIsAppliedWhenRestoreCompletes(t *testing.T) {
	lifecycle, err := newDaemonLifecycle("txn-1", "", "")
	require.NoError(t, err)

	require.NoError(t, lifecycle.releaseUpgradeProbation("txn-1"),
		"a release that races the restore must be accepted, not refused — a refusal fails the supervisor's single approve and wedges the upgrade")
	require.Equal(t, DaemonPhaseWarming, lifecycle.snapshot().phase,
		"an early release must not advance the phase past the restore")
	require.True(t, IsDaemonUpgradeProbationErr(lifecycle.mutationAdmissionError()),
		"admission must stay closed until the restore completes — released implies restored")
	require.True(t, lifecycle.isUpgradeProbation())
	require.Error(t, lifecycle.releaseUpgradeProbation("other-txn"),
		"a mismatched transaction id is refused mid-restore too")
	require.NoError(t, lifecycle.releaseUpgradeProbation("txn-1"),
		"a repeated release (a supervisor re-run) stays a no-op")

	lifecycle.markRestoreComplete()
	require.Equal(t, DaemonPhaseHandoffPending, lifecycle.snapshot().phase,
		"the deferred release takes effect at restore: the candidate is released, not left in probation")
	require.NoError(t, lifecycle.mutationAdmissionError(), "admission opens once restore and release both hold")
	require.False(t, lifecycle.isUpgradeProbation())
	require.Equal(t, "txn-1", lifecycle.snapshot().transactionID,
		"the transaction id stays for the whole boot (#1947)")

	ordinary, err := newDaemonLifecycle("", "", "")
	require.NoError(t, err)
	require.Error(t, ordinary.releaseUpgradeProbation("txn-1"),
		"a daemon that is not an upgrade candidate is refused mid-restore as well")
}

func TestCandidateValidationWaitsForRestore(t *testing.T) {
	home := stubForwardEnv(t)
	journal := forwardJournal(home)
	ctx := context.Background()

	t.Run("passes only once the restore has completed", func(t *testing.T) {
		lifecycle, err := newDaemonLifecycle(journal.ID, "", "")
		require.NoError(t, err)
		polls := 0
		upgradeRecoveryHealthFn = func() HealthStatus {
			polls++
			if polls == 5 {
				lifecycle.markRestoreComplete()
			}
			return candidateHealth(lifecycle, journal.ToVersion)
		}
		require.NoError(t, validateCandidateDaemon(ctx, journal))
		require.Equal(t, 5, polls, "validation must keep polling through the warming phase, not pass on the first answer")
	})

	t.Run("a restoring candidate gets the restore allowance, a wrong one does not", func(t *testing.T) {
		prevGrace, prevRestore := upgradeValidateGrace, upgradeCandidateRestoreGrace
		t.Cleanup(func() { upgradeValidateGrace, upgradeCandidateRestoreGrace = prevGrace, prevRestore })
		upgradeValidateGrace = 0 // every non-restore failure is final on its first poll
		upgradeCandidateRestoreGrace = time.Minute

		lifecycle, err := newDaemonLifecycle(journal.ID, "", "")
		require.NoError(t, err)
		polls := 0
		upgradeRecoveryHealthFn = func() HealthStatus {
			polls++
			if polls == 3 {
				lifecycle.markRestoreComplete()
			}
			return candidateHealth(lifecycle, journal.ToVersion)
		}
		require.NoError(t, validateCandidateDaemon(ctx, journal),
			"a candidate that matches everything but is still restoring must be waited for past the ordinary grace")

		wrong := 0
		upgradeRecoveryHealthFn = func() HealthStatus {
			wrong++
			h := candidateHealth(lifecycle, "9.9.9-broken")
			h.Phase = DaemonPhaseWarming
			return h
		}
		err = validateCandidateDaemon(ctx, journal)
		require.Error(t, err)
		require.NotErrorIs(t, err, errDaemonStillRestoring, "the version mismatch is reported, not the restore")
		require.Equal(t, 1, wrong, "a wrong candidate must not ride the restore allowance — it fails on the ordinary grace")
	})

	t.Run("a restore that never completes fails validation and rolls back", func(t *testing.T) {
		prevRestore := upgradeCandidateRestoreGrace
		t.Cleanup(func() { upgradeCandidateRestoreGrace = prevRestore })
		upgradeCandidateRestoreGrace = 20 * time.Millisecond
		lifecycle, err := newDaemonLifecycle(journal.ID, "", "")
		require.NoError(t, err)
		upgradeRecoveryHealthFn = func() HealthStatus { return candidateHealth(lifecycle, journal.ToVersion) }
		err = validateCandidateDaemon(ctx, journal)
		require.ErrorIs(t, err, errDaemonStillRestoring,
			"a candidate that cannot finish restoring must fail validation (rollback), never be committed")
	})

	t.Run("only an explicit warming phase holds validation back", func(t *testing.T) {
		candidate := HealthStatus{DaemonVersion: journal.ToVersion, TransactionID: journal.ID,
			Listeners: DaemonListenerStatus{HTTPUnixBound: true}}
		want := daemonIdentity{role: "candidate", version: journal.ToVersion,
			requireTransactionID: journal.ID, listeners: journal.Daemon.Listeners}
		require.NoError(t, daemonMatchesIdentity(candidate, want),
			"a daemon too old to report a phase answers \"\" and must still validate")
		for _, phase := range []DaemonPhase{DaemonPhaseUpgradeProbation, DaemonPhaseHandoffPending} {
			candidate.Phase = phase
			require.NoError(t, daemonMatchesIdentity(candidate, want), "phase %s is past the restore", phase)
		}

		previous := HealthStatus{DaemonVersion: journal.FromVersion, Phase: DaemonPhaseWarming,
			Listeners: DaemonListenerStatus{HTTPUnixBound: true}}
		require.NoError(t, daemonMatchesIdentity(previous, previousDaemonIdentity(journal)),
			"the restored PREVIOUS daemon is held to liveness, not restore — rollback validation is unchanged")
	})
}

// TestCommittedReentryRecoversCandidateStillRestoring proves a wedged upgrade
// recovers on the next start instead of wedging again. The wedged state is a
// committed journal whose candidate answers with its transaction id but has not
// been released. Any later run of the recovery actor (the unit restarting at
// login or boot, or a client entrypoint waking it) re-enters PhaseCommitted,
// whose arm runs StartCandidate → ValidateCandidate → ApproveCandidate
// (internal/upgradetxn/supervisor.go). This drives those production ops in that
// order against a candidate that is still restoring when the run begins — the
// shape that used to fail the approve on every restart.
func TestCommittedReentryRecoversCandidateStillRestoring(t *testing.T) {
	home := stubForwardEnv(t)
	journal := forwardJournal(home)
	ctx := context.Background()

	lifecycle, err := newDaemonLifecycle(journal.ID, "", "")
	require.NoError(t, err)
	polls := 0
	upgradeRecoveryHealthFn = func() HealthStatus {
		polls++
		if polls == 4 {
			lifecycle.markRestoreComplete() // restoreInstances finishes mid-run
		}
		return candidateHealth(lifecycle, journal.ToVersion)
	}
	launchCandidateDaemonFn = func(string, string) error {
		t.Fatal("the committed candidate is already answering — re-entry must not launch a second one")
		return nil
	}
	var phaseAtRelease DaemonPhase
	releaseCandidateProbationFn = func(id string) error {
		phaseAtRelease = lifecycle.snapshot().phase
		return lifecycle.releaseUpgradeProbation(id)
	}

	require.NoError(t, startCandidateDaemon(ctx, journal), "StartCandidate")
	require.NoError(t, validateCandidateDaemon(ctx, journal), "ValidateCandidate")
	require.NoError(t, approveCandidateDaemon(ctx, journal),
		"ApproveCandidate must succeed on the next start — this is the call that failed every restart and wedged the upgrade")

	require.NotEqual(t, DaemonPhaseWarming, phaseAtRelease,
		"the supervisor must not send the release before the candidate's restore completes")
	require.Equal(t, DaemonPhaseHandoffPending, lifecycle.snapshot().phase)
	require.NoError(t, lifecycle.mutationAdmissionError(), "the recovered candidate admits work")
}

// TestApproveFromSupervisorThatSkipsRestoreWaitStillConverges covers the upgrade
// INTO this version: its supervisor is the previous, unfixed binary, which
// validates without looking at the phase and approves mid-restore. The candidate
// is this binary, so its acceptance of the early release is what converges.
func TestApproveFromSupervisorThatSkipsRestoreWaitStillConverges(t *testing.T) {
	home := stubForwardEnv(t)
	journal := forwardJournal(home)

	lifecycle, err := newDaemonLifecycle(journal.ID, "", "")
	require.NoError(t, err)
	releaseCandidateProbationFn = lifecycle.releaseUpgradeProbation

	require.NoError(t, approveCandidateDaemon(context.Background(), journal),
		"an approve that lands mid-restore must succeed, or the actor fails and its restart budget runs out")
	require.Equal(t, DaemonPhaseWarming, lifecycle.snapshot().phase)
	require.True(t, IsDaemonUpgradeProbationErr(lifecycle.mutationAdmissionError()),
		"the early release must not open admission over a half-loaded manager")

	lifecycle.markRestoreComplete()
	require.Equal(t, DaemonPhaseHandoffPending, lifecycle.snapshot().phase)
	require.NoError(t, lifecycle.mutationAdmissionError())
}

// TestRunDaemonAppliesReleaseThatRacedRestoreAndStaysParked drives the real
// daemon: the ReleaseUpgradeProbation RPC lands while restoreInstances is still
// running, is accepted, takes effect when the restore completes — and the
// released candidate still PARKS. Parking used to key on "still in probation",
// so a candidate released at restore would have fallen through to the orphan
// sweeps, cron, watchers, and the update driver on a daemon the hand-off is
// about to stop. The sweeps are the oracle: they run on the fall-through path
// only, and nothing between the park decision and them listens for Shutdown, so
// a fall-through always records them before runDaemon returns.
func TestRunDaemonAppliesReleaseThatRacedRestoreAndStaysParked(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "" // keep this lifecycle test on AF-owned Unix sockets only
	const transactionID = "transaction-5693"

	restoreGate := make(chan struct{})
	var openGate sync.Once
	releaseRestore := func() { openGate.Do(func() { close(restoreGate) }) }
	prevRestore := restoreManagerForStartup
	restoreManagerForStartup = func(m *Manager) error {
		<-restoreGate
		return m.restoreInstances()
	}
	t.Cleanup(func() { restoreManagerForStartup = prevRestore })
	t.Cleanup(releaseRestore)

	var fellThrough atomic.Bool
	prevTunnels, prevContainers := sweepOrphanSandboxTunnels, sweepOrphanContainers
	sweepOrphanSandboxTunnels = func(string) { fellThrough.Store(true) }
	sweepOrphanContainers = func(string, map[string]bool) session.OrphanSweepResult {
		fellThrough.Store(true)
		return session.OrphanSweepResult{}
	}
	t.Cleanup(func() { sweepOrphanSandboxTunnels, sweepOrphanContainers = prevTunnels, prevContainers })

	runDone := make(chan error, 1)
	go func() { runDone <- runDaemon(cfg, transactionID) }()
	stopped := false
	defer func() {
		if stopped {
			return
		}
		releaseRestore()
		var resp ShutdownResponse
		_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Errorf("candidate daemon did not stop during cleanup")
		}
	}()

	warming := waitForDaemonPhase(t, DaemonPhaseWarming)
	require.Equal(t, transactionID, warming.TransactionID)

	var release ReleaseUpgradeProbationResponse
	require.NoError(t, callDaemonNoEnsure("ReleaseUpgradeProbation",
		ReleaseUpgradeProbationRequest{TransactionID: transactionID}, &release),
		"the supervisor's release must be accepted while restoreInstances is still running")
	require.True(t, release.OK)
	require.Equal(t, DaemonPhaseWarming, Health().Phase, "the release is deferred until the restore completes")
	var create CreateSessionResponse
	createErr := callDaemonNoEnsure("CreateSession", CreateSessionRequest{}, &create)
	require.True(t, IsDaemonAdmissionRetryable(createErr),
		"a mutation is still refused, retryably, while the restore runs: %v", createErr)

	releaseRestore()
	released := waitForDaemonPhase(t, DaemonPhaseHandoffPending)
	require.Equal(t, transactionID, released.TransactionID)

	var shutdown ShutdownResponse
	require.NoError(t, callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &shutdown))
	select {
	case err := <-runDone:
		require.NoError(t, err)
		stopped = true
	case <-time.After(5 * time.Second):
		t.Fatal("the released candidate did not stop after Shutdown")
	}
	require.False(t, fellThrough.Load(),
		"a candidate released during its restore must stay parked, never arm the operational loops")
}

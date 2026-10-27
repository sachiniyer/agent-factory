package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
)

// fakeDaemon is the responder the health seam reflects, so a forward/rollback
// sequence can be driven through the REAL production ops without a spawned daemon.
type fakeDaemon struct {
	up        bool
	version   string
	txnID     string
	httpBound bool
}

func (f fakeDaemon) health() HealthStatus {
	if !f.up {
		return HealthStatus{PingErr: errors.New("no daemon")}
	}
	return HealthStatus{
		DaemonVersion: f.version,
		TransactionID: f.txnID,
		Listeners:     DaemonListenerStatus{HTTPUnixBound: f.httpBound},
	}
}

// stubForwardEnv binds a throwaway home (so recoveryHomeGuard passes and the real
// StopDaemon/WaitForShutdownCompletion see no daemon → StopConfirmed), shortens
// the polls, and restores every forward/recovery seam on cleanup.
func stubForwardEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	pg, pp := upgradeValidateGrace, upgradeValidatePoll
	ag, ap := awaitActivationGrace, awaitActivationPoll
	cg, cp := adoptConfirmGrace, adoptConfirmPoll
	pHealth := upgradeRecoveryHealthFn
	pUnit := startPreviousViaUnitFn
	pAdHoc := startPreviousAdHocFn
	pLaunch := launchCandidateDaemonFn
	pApproved := activationApprovedFn
	pRelease := releaseCandidateProbationFn
	pAdopt := adoptAfterUpgradeCommitFn
	pRun := runRecoveryActorFn
	pStop := stopDaemonFn
	pWait := waitForShutdownFn
	pReady := waitForDaemonReadyFn
	t.Cleanup(func() {
		upgradeValidateGrace, upgradeValidatePoll = pg, pp
		awaitActivationGrace, awaitActivationPoll = ag, ap
		adoptConfirmGrace, adoptConfirmPoll = cg, cp
		upgradeRecoveryHealthFn = pHealth
		startPreviousViaUnitFn = pUnit
		startPreviousAdHocFn = pAdHoc
		launchCandidateDaemonFn = pLaunch
		activationApprovedFn = pApproved
		releaseCandidateProbationFn = pRelease
		adoptAfterUpgradeCommitFn = pAdopt
		runRecoveryActorFn = pRun
		stopDaemonFn = pStop
		waitForShutdownFn = pWait
		waitForDaemonReadyFn = pReady
	})
	upgradeValidateGrace, upgradeValidatePoll = 500*time.Millisecond, time.Millisecond
	awaitActivationGrace, awaitActivationPoll = 500*time.Millisecond, time.Millisecond
	adoptConfirmGrace, adoptConfirmPoll = 500*time.Millisecond, time.Millisecond
	// Ready by default: the arm-observation tests that care override this.
	waitForDaemonReadyFn = func(time.Time) error { return nil }
	return home
}

func forwardJournal(home string) upgradetxn.Journal {
	return upgradetxn.Journal{
		ID: "txn-1", HomeDir: home, ExecutablePath: "/opt/agent-factory/bin/af",
		FromVersion: "1.0.100", ToVersion: "1.0.200",
		Daemon: upgradetxn.DaemonSnapshot{
			Owner:     upgradetxn.DaemonOwner{Kind: upgradetxn.SupervisionSystemd, ServiceName: "agent-factory-daemon.service"},
			Listeners: upgradetxn.ListenerExpectation{HTTPUnixBound: true},
		},
	}
}

// wireStopToState makes the stop seam actually take the fake daemon down, so a
// StopConfirmed assertion is LOAD-BEARING: the stop must clear the daemon AND the
// wait must then observe it gone. If stopDaemonForRecovery ever regressed to
// confirm a stop it never observed (the fabricated-confirmation class), the wait
// would still see the daemon up and the sequence would report StopStillRunning.
func wireStopToState(state *fakeDaemon) {
	stopDaemonFn = func() (bool, error) { state.up = false; return true, nil }
	waitForShutdownFn = func(ShutdownTarget) error {
		if state.up {
			return errors.New("control socket still answering")
		}
		return nil
	}
}

// TestUpgradeForward_CommitSequence drives the production forward ops in the order
// Supervisor.Run invokes them, proving they interoperate: the candidate is
// launched WITH the transaction id, validated at ToVersion, and released from
// probation at commit.
func TestUpgradeForward_CommitSequence(t *testing.T) {
	home := stubForwardEnv(t)
	journal := forwardJournal(home)
	ctx := context.Background()

	state := &fakeDaemon{up: true, version: "1.0.100", httpBound: true} // previous daemon serving
	upgradeRecoveryHealthFn = func() HealthStatus { return state.health() }
	wireStopToState(state)
	activationApprovedFn = func(upgradetxn.Journal) (bool, error) { return true, nil }
	launchCandidateDaemonFn = func(_, id string) error {
		*state = fakeDaemon{up: true, version: "1.0.200", txnID: id, httpBound: true}
		return nil
	}
	released := false
	releaseCandidateProbationFn = func(id string) error {
		// Release lifts probation but KEEPS the transaction id — the candidate
		// reports it for its whole boot (#1947), so the supervisor's post-commit
		// re-runs still recognize it.
		if id == state.txnID {
			released = true
		}
		return nil
	}

	if err := awaitOldDaemonActivation(ctx, journal); err != nil {
		t.Fatalf("AwaitActivation: %v", err)
	}
	if outcome, err := stopPreviousDaemon(ctx, journal); err != nil || outcome != upgradetxn.StopConfirmed {
		t.Fatalf("StopPrevious: outcome=%v err=%v", outcome, err)
	}
	if err := startCandidateDaemon(ctx, journal); err != nil {
		t.Fatalf("StartCandidate: %v", err)
	}
	if state.txnID != journal.ID {
		t.Fatalf("candidate was not launched with the transaction id; got %q", state.txnID)
	}
	if err := validateCandidateDaemon(ctx, journal); err != nil {
		t.Fatalf("ValidateCandidate: %v", err)
	}
	if err := approveCandidateDaemon(ctx, journal); err != nil {
		t.Fatalf("ApproveCandidate: %v", err)
	}
	if !released {
		t.Fatal("commit did not release the candidate from probation")
	}
	if state.txnID != journal.ID {
		t.Fatal("release must KEEP the candidate's transaction id (#1947), not erase it")
	}
	// The supervisor re-runs StartCandidate/ValidateCandidate on any PhaseCommitted
	// re-entry; both must still recognize the released candidate by its retained id.
	if err := startCandidateDaemon(ctx, journal); err != nil {
		t.Fatalf("StartCandidate re-run after release: %v", err)
	}
	if err := validateCandidateDaemon(ctx, journal); err != nil {
		t.Fatalf("ValidateCandidate re-run after release must still recognize the candidate: %v", err)
	}
}

// TestUpgradeForward_BrokenCandidateRollsBack drives the same forward ops until a
// candidate that never reaches ToVersion fails validation, then drives the
// production ROLLBACK ops, proving the previous daemon is actually restored — the
// real rollback the review requires, through the production ops end to end.
func TestUpgradeForward_BrokenCandidateRollsBack(t *testing.T) {
	home := stubForwardEnv(t)
	journal := forwardJournal(home)
	ctx := context.Background()

	state := &fakeDaemon{up: true, version: "1.0.100", httpBound: true}
	upgradeRecoveryHealthFn = func() HealthStatus { return state.health() }
	wireStopToState(state)
	activationApprovedFn = func(upgradetxn.Journal) (bool, error) { return true, nil }
	launchCandidateDaemonFn = func(_, id string) error {
		*state = fakeDaemon{up: true, version: "9.9.9-broken", txnID: id, httpBound: true}
		return nil
	}
	startPreviousViaUnitFn = func() error {
		*state = fakeDaemon{up: true, version: "1.0.100", httpBound: true} // previous restored, no id
		return nil
	}

	if err := awaitOldDaemonActivation(ctx, journal); err != nil {
		t.Fatalf("AwaitActivation: %v", err)
	}
	if outcome, err := stopPreviousDaemon(ctx, journal); err != nil || outcome != upgradetxn.StopConfirmed {
		t.Fatalf("StopPrevious: outcome=%v err=%v", outcome, err)
	}
	if err := startCandidateDaemon(ctx, journal); err != nil {
		t.Fatalf("StartCandidate: %v", err)
	}
	if err := validateCandidateDaemon(ctx, journal); err == nil {
		t.Fatal("a candidate that never reaches ToVersion must fail validation")
	}

	// Rollback path.
	if outcome, err := stopCandidateDaemon(ctx, journal); err != nil || outcome != upgradetxn.StopConfirmed {
		t.Fatalf("StopCandidate: outcome=%v err=%v", outcome, err)
	}
	if err := startPreviousDaemon(ctx, journal); err != nil {
		t.Fatalf("StartPrevious: %v", err)
	}
	if state.version != "1.0.100" || state.txnID != "" {
		t.Fatalf("previous daemon was not restored; state=%+v", *state)
	}
	if err := validatePreviousDaemon(ctx, journal); err != nil {
		t.Fatalf("ValidatePrevious after rollback: %v", err)
	}
}

// ValidateCandidate is the mirror of validatePreviousDaemon: it must REQUIRE the
// transaction id. A responder with an EMPTY id at ToVersion is the previous daemon
// (or a surviving one after a from==to rebuild), not the candidate, and must be
// rejected.
func TestValidateCandidate_RequiresTransactionId(t *testing.T) {
	home := stubForwardEnv(t)
	journal := forwardJournal(home)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		health  HealthStatus
		wantErr bool
	}{
		{"matching candidate", HealthStatus{DaemonVersion: "1.0.200", TransactionID: "txn-1", Listeners: DaemonListenerStatus{HTTPUnixBound: true}}, false},
		{"empty id is the previous daemon, not the candidate", HealthStatus{DaemonVersion: "1.0.200", Listeners: DaemonListenerStatus{HTTPUnixBound: true}}, true},
		{"wrong id", HealthStatus{DaemonVersion: "1.0.200", TransactionID: "other", Listeners: DaemonListenerStatus{HTTPUnixBound: true}}, true},
		{"wrong version", HealthStatus{DaemonVersion: "1.0.100", TransactionID: "txn-1", Listeners: DaemonListenerStatus{HTTPUnixBound: true}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upgradeRecoveryHealthFn = func() HealthStatus { return tc.health }
			err := validateCandidateDaemon(ctx, journal)
			if tc.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v got %v", tc.wantErr, err)
			}
		})
	}
}

func TestStartCandidate_HomeMismatchRefuses(t *testing.T) {
	stubForwardEnv(t)
	launched := false
	launchCandidateDaemonFn = func(string, string) error { launched = true; return nil }
	journal := upgradetxn.Journal{HomeDir: "/some/other/home", ID: "txn-1"}
	if err := startCandidateDaemon(context.Background(), journal); err == nil {
		t.Fatal("a home mismatch must refuse to launch a candidate")
	}
	if launched {
		t.Fatal("a home mismatch must not spawn a candidate on the wrong home")
	}
}

func TestStartCandidate_IdempotentWhenAlreadyRunning(t *testing.T) {
	home := stubForwardEnv(t)
	journal := forwardJournal(home)
	upgradeRecoveryHealthFn = func() HealthStatus {
		return HealthStatus{DaemonVersion: "1.0.200", TransactionID: journal.ID}
	}
	launched := false
	launchCandidateDaemonFn = func(string, string) error { launched = true; return nil }
	if err := startCandidateDaemon(context.Background(), journal); err != nil {
		t.Fatalf("StartCandidate: %v", err)
	}
	if launched {
		t.Fatal("a candidate already answering with this transaction id must not be relaunched")
	}
}

func TestApproveCandidate_HomeMismatchRefuses(t *testing.T) {
	stubForwardEnv(t)
	released := false
	releaseCandidateProbationFn = func(string) error { released = true; return nil }
	journal := upgradetxn.Journal{HomeDir: "/some/other/home", ID: "txn-1"}
	if err := approveCandidateDaemon(context.Background(), journal); err == nil {
		t.Fatal("a home mismatch must refuse to release probation")
	}
	if released {
		t.Fatal("a home mismatch must not release a daemon on the wrong home")
	}
}

func TestAwaitActivation_WaitsThenReturns(t *testing.T) {
	home := stubForwardEnv(t)
	journal := forwardJournal(home)
	calls := 0
	activationApprovedFn = func(upgradetxn.Journal) (bool, error) {
		calls++
		return calls >= 3, nil
	}
	if err := awaitOldDaemonActivation(context.Background(), journal); err != nil {
		t.Fatalf("AwaitActivation should return once approved: %v", err)
	}
}

func TestReleaseUpgradeProbation(t *testing.T) {
	lifecycle, err := newDaemonLifecycle("txn-1", "", "")
	if err != nil {
		t.Fatalf("newDaemonLifecycle: %v", err)
	}
	lifecycle.markRestoreComplete() // → probation

	if lifecycle.mutationAdmissionError() == nil {
		t.Fatal("mutations must be blocked while in probation")
	}
	if err := lifecycle.releaseUpgradeProbation("other-txn"); err == nil {
		t.Fatal("a mismatched transaction id must be refused")
	}
	if err := lifecycle.releaseUpgradeProbation("txn-1"); err != nil {
		t.Fatalf("matching release: %v", err)
	}
	// Admission opens and probation is lifted, but the transaction id is KEPT for
	// the whole boot (#1947): erasing it would break the supervisor's post-commit
	// re-runs and its ability to reject a different daemon on the same socket.
	if lifecycle.isUpgradeProbation() {
		t.Fatal("release must lift probation")
	}
	// The phase must NOT be Ready: this candidate is parked and never arms its
	// operational loops, so reporting it ready would hide a failed/skipped hand-off.
	if lifecycle.snapshot().phase != DaemonPhaseHandoffPending {
		t.Fatalf("release must mark the candidate handoff-pending, not ready; got %q", lifecycle.snapshot().phase)
	}
	if lifecycle.mutationAdmissionError() != nil {
		t.Fatal("mutations must be admitted after release")
	}
	if lifecycle.snapshot().transactionID != "txn-1" {
		t.Fatal("release must KEEP the transaction id, not erase it (#1947)")
	}
	// Idempotent: a re-run of ApproveCandidate at PhaseCommitted must not fail.
	if err := lifecycle.releaseUpgradeProbation("txn-1"); err != nil {
		t.Fatalf("a second release for the same transaction must be a no-op, got %v", err)
	}

	// A daemon that never entered probation cannot be released.
	ordinary, err := newDaemonLifecycle("", "", "")
	if err != nil {
		t.Fatalf("newDaemonLifecycle: %v", err)
	}
	ordinary.markRestoreComplete()
	if err := ordinary.releaseUpgradeProbation("txn-1"); err == nil {
		t.Fatal("releasing a daemon that is not an upgrade candidate must be refused")
	}
}

// adoptAfterUpgradeCommit is the last step of the irreversible path, so it holds
// itself to the rollback path's evidentiary discipline: it CONFIRMS the committed
// candidate is serving before touching it (a dial timeout is not proof of absence),
// stops only the identity-matched candidate, replaces it under whatever owns the
// home, and OBSERVES the fresh daemon become ready — falling back to an ad-hoc spawn
// if a unit fails to start OR to become ready (P2), and surfacing a state it could
// not confirm as a loud error rather than a silent success.
func TestAdoptAfterUpgradeCommit_ReplacesParkedCandidateUnderEveryOwner(t *testing.T) {
	const canonicalExec = "/opt/agent-factory/bin/af"
	ourCandidate := HealthStatus{DaemonVersion: "1.0.200", TransactionID: "txn-1", ServingPID: 42}
	for _, tc := range []struct {
		name           string
		installUnit    bool // a home-serving unit exists → OwnerUnit, else OwnerAdHoc
		health         HealthStatus
		unitStartErr   error
		readyResults   []error // successive waitForDaemonReady returns
		wantStopped    bool
		wantUnitStart  bool
		wantAdHocStart bool
		wantErr        bool
	}{
		{
			name:          "unit owner: stop the ad-hoc candidate, start it under the unit",
			installUnit:   true,
			health:        ourCandidate,
			wantStopped:   true,
			wantUnitStart: true,
		},
		{
			// The P1 fix: an ad-hoc home has no unit, so without this hand-off the
			// parked candidate would be the permanent daemon with its loops unarmed.
			name:           "ad-hoc owner: stop the parked candidate, respawn it ad-hoc",
			installUnit:    false,
			health:         ourCandidate,
			wantStopped:    true,
			wantAdHocStart: true,
		},
		{
			// The P2 fix: never leave zero daemons when the unit fails to START.
			name:           "unit start fails → ad-hoc fallback",
			installUnit:    true,
			health:         ourCandidate,
			unitStartErr:   errors.New("systemctl unavailable"),
			wantStopped:    true,
			wantUnitStart:  true,
			wantAdHocStart: true,
		},
		{
			// The round-3 (2) fix: a fork is not a serving daemon. The unit forks but
			// its daemon never answers → fall back to ad-hoc rather than report success.
			name:           "unit starts but never becomes ready → ad-hoc fallback",
			installUnit:    true,
			health:         ourCandidate,
			readyResults:   []error{errors.New("unit daemon never answered"), nil},
			wantStopped:    true,
			wantUnitStart:  true,
			wantAdHocStart: true,
		},
		{
			// The round-3 (1) fix: a DEFINITE absence (ECONNREFUSED) is the only "gone".
			// Nothing to stop, but the home still must get a fresh daemon.
			name:           "definite absence → start fresh without a stop",
			installUnit:    false,
			health:         HealthStatus{PingErr: syscall.ECONNREFUSED},
			wantAdHocStart: true,
		},
		{
			name:        "a different daemon is never stopped (identity guard)",
			installUnit: true,
			health:      HealthStatus{DaemonVersion: "1.0.100", TransactionID: "other", ServingPID: 42},
		},
		{
			// The round-3 (1) fix: an UNDETERMINED probe (a dial timeout) is NOT proof
			// the candidate is gone — never silently skip; surface it loudly instead.
			name:        "unconfirmed (timeout) → loud error, no action",
			installUnit: true,
			health:      HealthStatus{PingErr: errors.New("dial tcp: i/o timeout")},
			wantErr:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := stubForwardEnv(t)
			unitDir := withAutostartTestEnv(t, "linux")
			if tc.installUnit {
				unit := systemdAutostartUnit(canonicalExec, "", "", home)
				if err := os.WriteFile(filepath.Join(unitDir, autostartUnitName), []byte(unit), 0o600); err != nil {
					t.Fatalf("write home-serving unit: %v", err)
				}
			}
			upgradeRecoveryHealthFn = func() HealthStatus { return tc.health }
			stopped := false
			stopDaemonFn = func() (bool, error) { stopped = true; return true, nil }
			waitForShutdownFn = func(ShutdownTarget) error { return nil }
			unitStarted := false
			startPreviousViaUnitFn = func() error { unitStarted = true; return tc.unitStartErr }
			adhocStarted := false
			startPreviousAdHocFn = func(execPath string) error {
				adhocStarted = true
				if execPath != canonicalExec {
					t.Fatalf("ad-hoc respawn used %q, want the canonical path %q", execPath, canonicalExec)
				}
				return nil
			}
			readyCall := 0
			waitForDaemonReadyFn = func(time.Time) error {
				var err error
				if readyCall < len(tc.readyResults) {
					err = tc.readyResults[readyCall]
				}
				readyCall++
				return err
			}

			err := adoptAfterUpgradeCommit("txn-1", canonicalExec)
			if tc.wantErr != (err != nil) {
				t.Fatalf("error: got %v want wantErr=%v", err, tc.wantErr)
			}
			if stopped != tc.wantStopped {
				t.Fatalf("stopped: got %v want %v", stopped, tc.wantStopped)
			}
			if unitStarted != tc.wantUnitStart {
				t.Fatalf("unit start: got %v want %v", unitStarted, tc.wantUnitStart)
			}
			if adhocStarted != tc.wantAdHocStart {
				t.Fatalf("ad-hoc start: got %v want %v", adhocStarted, tc.wantAdHocStart)
			}
		})
	}
}

// RunUpgradeRecoveryActor arms the post-upgrade daemon ONLY on the actor's
// positive commit signal — not on any journal read RunUpgradeRecoveryActor
// performs itself. The prior code gated the irreversible hand-off on TWO
// non-retried upgradetxn.Load reads (a pre-actor ourTransaction capture and a
// post-actor journal-absence confirmation); a transient read failure at either
// gate silently stranded a committed candidate in DaemonPhaseHandoffPending
// forever. The fix derives the signal from the supervisor's own already-loaded
// transaction, so the table below DECOUPLES the commit signal (committed) from
// the on-disk journal state (journalGone) to prove the signal is the sole driver:
// adoption follows committed regardless of whether a racy read would still see
// the journal.
func TestRunUpgradeRecoveryActor_AdoptsOnlyOnCommit(t *testing.T) {
	systemdJob := upgradetxn.RecoveryJob{
		Kind:     upgradetxn.RecoveryJobSystemd,
		Name:     "agent-factory-upgrade-recovery-txn-1.service",
		UnitPath: "/tmp/agent-factory-upgrade-recovery-txn-1.service",
	}
	for _, tc := range []struct {
		name        string
		ownerKind   upgradetxn.SupervisionKind
		serviceName string
		recoveryJob upgradetxn.RecoveryJob
		committed   bool // the actor's positive commit signal
		journalGone bool // whether the actor removed the on-disk journal (Cleanup)
		wantAdopt   bool
	}{
		{"systemd owner, committed", upgradetxn.SupervisionSystemd, "agent-factory-daemon.service", systemdJob, true, true, true},
		// Regression guard for the post-actor gate: the old code re-Load()ed the
		// journal after the actor returned and skipped the hand-off whenever it
		// was still present, even on a real commit. The positive commit signal
		// must drive adoption regardless of a racy journal read — committed=true
		// with the journal STILL ON DISK must adopt.
		{"systemd owner, committed but journal still present (racy post-actor read)", upgradetxn.SupervisionSystemd, "agent-factory-daemon.service", systemdJob, true, false, true},
		{"systemd owner, stand-down (journal retained)", upgradetxn.SupervisionSystemd, "agent-factory-daemon.service", systemdJob, false, false, false},
		// Regression guard for the pre-actor gate: the old code captured an
		// ourTransaction flag via a single pre-supervisor Load; a transient
		// failure left it false, skipping the hand-off even after a real commit.
		// The signal — not a racy pre-actor read, and not journal absence — gates
		// the hand-off, so journal absence ALONE (Committed=false) must NOT adopt.
		{"systemd owner, stand-down with journal already gone (no false adopt)", upgradetxn.SupervisionSystemd, "agent-factory-daemon.service", systemdJob, false, true, false},
		{"ad-hoc owner, committed", upgradetxn.SupervisionAdHoc, "", upgradetxn.RecoveryJob{Kind: upgradetxn.RecoveryJobDetached}, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := stubForwardEnv(t)
			exe := filepath.Join(t.TempDir(), "af")
			if err := os.WriteFile(exe, []byte("previous-binary"), 0o755); err != nil {
				t.Fatalf("write fake previous binary: %v", err)
			}
			if _, err := upgradetxn.Prepare(upgradetxn.Plan{
				ID: "txn-1", HomeDir: home, ExecutablePath: exe,
				FromVersion: "1.0.100", ToVersion: "1.0.200", Candidate: []byte("candidate"),
				Daemon: upgradetxn.DaemonSnapshot{
					WasRunning: true, BootID: "boot-1",
					Owner: upgradetxn.DaemonOwner{Kind: tc.ownerKind, ServiceName: tc.serviceName},
				},
				RecoveryJob: tc.recoveryJob,
			}); err != nil {
				t.Fatalf("Prepare: %v", err)
			}

			// The stub is the actor. It returns the positive commit signal
			// (committed) AND optionally simulates lease.Cleanup removing the
			// journal (journalGone). The two are DECOUPLED so the test proves
			// RunUpgradeRecoveryActor gates adoption on the signal, not on a
			// journal read it performs itself — the race that stranded a
			// committed candidate when a transient I/O failure hit either Load.
			runRecoveryActorFn = func(_ context.Context, _ upgradetxn.RecoveryInvocation, _ upgradetxn.Supervisor) (upgradetxn.RecoveryActorResult, error) {
				if tc.journalGone {
					_ = os.Remove(filepath.Join(home, "upgrade", "active.json"))
				}
				if tc.committed {
					return upgradetxn.RecoveryActorResult{Committed: true, JournalID: "txn-1", ExecutablePath: exe}, nil
				}
				return upgradetxn.RecoveryActorResult{}, nil
			}
			adopted := false
			var adoptTxn, adoptExec string
			adoptAfterUpgradeCommitFn = func(txnID, execPath string) error {
				adopted = true
				adoptTxn = txnID
				adoptExec = execPath
				return nil
			}

			if err := RunUpgradeRecoveryActor(context.Background(),
				upgradetxn.RecoveryInvocation{HomeDir: home, TransactionID: "txn-1"}); err != nil {
				t.Fatalf("RunUpgradeRecoveryActor: %v", err)
			}
			if adopted != tc.wantAdopt {
				t.Fatalf("hand-off: got adopted=%v want %v (%s)", adopted, tc.wantAdopt, tc.name)
			}
			// When adoption happens, the hand-off must be parameterized from the
			// commit SIGNAL (the supervisor's own transaction identity + canonical
			// path), not from a racy pre-actor Load RunUpgradeRecoveryActor ran
			// itself. Assert both the transaction id and the executable path
			// reach adoptAfterUpgradeCommit from the result.
			if tc.wantAdopt {
				if adoptTxn != "txn-1" {
					t.Fatalf("hand-off used transaction %q, want the committed journal id %q", adoptTxn, "txn-1")
				}
				if adoptExec != exe {
					t.Fatalf("hand-off used executable %q, want the canonical path from the commit signal %q", adoptExec, exe)
				}
			}
		})
	}
}

// TestRunUpgradeRecoveryActor_PreservesActiveJournalInterlock guards the
// post-commit interlock the commit signal alone cannot provide: a positive
// result.Committed proves OUR transaction committed, but not that the home is
// still free when the hand-off runs. If the actor was descheduled after
// lease.Cleanup removed our journal and a subsequent upgrade published its own
// active.json, an unconditional hand-off could stop the old candidate (or start
// a normal daemon) while the new transaction owns the home. RunUpgradeRecoveryActor
// must skip the hand-off when a DIFFERENT active transaction is present, while
// treating a transient Load failure as non-blocking so the stranded-candidate
// regression the commit signal was introduced to fix does not return.
func TestRunUpgradeRecoveryActor_PreservesActiveJournalInterlock(t *testing.T) {
	for _, tc := range []struct {
		name        string
		setupActive func(t *testing.T, home, exe string)
		wantAdopt   bool
	}{
		{
			// A subsequent upgrade published its own active.json with a different
			// transaction id after our cleanup. The hand-off must be skipped: the
			// new transaction owns the home, and confirmCommittedCandidate would
			// not see our daemon (it has exited) and could start a fresh one that
			// collides with the in-flight transaction.
			name: "different active transaction present skips hand-off",
			setupActive: func(t *testing.T, home, exe string) {
				t.Helper()
				// A real Prepare is the only way to publish a Load-valid journal:
				// validateJournal checks the transaction directory, recovery-lock
				// identity/nonce, and binary-snapshot pairing, so a hand-written
				// active.json would not load. The successor uses a different
				// executable so the staged-artifact guard does not refuse it as a
				// re-stage of the committed transaction over the same binary.
				successorExe := filepath.Join(t.TempDir(), "af-other")
				if err := os.WriteFile(successorExe, []byte("previous-binary-other"), 0o755); err != nil {
					t.Fatalf("write successor previous binary: %v", err)
				}
				if _, err := upgradetxn.Prepare(upgradetxn.Plan{
					ID: "txn-other", HomeDir: home, ExecutablePath: successorExe,
					FromVersion: "1.0.100", ToVersion: "1.0.200", Candidate: []byte("candidate-other"),
					Daemon: upgradetxn.DaemonSnapshot{
						WasRunning: true, BootID: "boot-other",
						Owner: upgradetxn.DaemonOwner{Kind: upgradetxn.SupervisionSystemd, ServiceName: "agent-factory-daemon.service"},
					},
					RecoveryJob: upgradetxn.RecoveryJob{Kind: upgradetxn.RecoveryJobSystemd,
						Name:     "agent-factory-upgrade-recovery-txn-other.service",
						UnitPath: filepath.Join(t.TempDir(), "agent-factory-upgrade-recovery-txn-other.service")},
				}); err != nil {
					t.Fatalf("Prepare successor transaction: %v", err)
				}
			},
			wantAdopt: false,
		},
		{
			// A transient Load failure (a malformed active.json, not the clean
			// ErrNoActiveTransaction our cleanup produced) must NOT block the
			// hand-off: the commit signal is authoritative, and re-gating the
			// irreversible hand-off on a read this call does not depend on would
			// reintroduce the stranded-candidate regression.
			name: "transient load failure is non-blocking",
			setupActive: func(t *testing.T, home, _ string) {
				t.Helper()
				// A malformed active.json makes Load return a JSON decode error
				// (not ErrNoActiveTransaction), the cheapest hermetic transient
				// failure.
				if err := os.MkdirAll(filepath.Join(home, "upgrade"), 0o755); err != nil {
					t.Fatalf("mkdir upgrade dir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(home, "upgrade", "active.json"), []byte("{not json"), 0o600); err != nil {
					t.Fatalf("write corrupt active.json: %v", err)
				}
			},
			wantAdopt: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := stubForwardEnv(t)
			exe := filepath.Join(t.TempDir(), "af")
			if err := os.WriteFile(exe, []byte("previous-binary"), 0o755); err != nil {
				t.Fatalf("write fake previous binary: %v", err)
			}
			if _, err := upgradetxn.Prepare(upgradetxn.Plan{
				ID: "txn-1", HomeDir: home, ExecutablePath: exe,
				FromVersion: "1.0.100", ToVersion: "1.0.200", Candidate: []byte("candidate"),
				Daemon: upgradetxn.DaemonSnapshot{
					WasRunning: true, BootID: "boot-1",
					Owner: upgradetxn.DaemonOwner{Kind: upgradetxn.SupervisionSystemd, ServiceName: "agent-factory-daemon.service"},
				},
				RecoveryJob: upgradetxn.RecoveryJob{Kind: upgradetxn.RecoveryJobSystemd,
					Name:     "agent-factory-upgrade-recovery-txn-1.service",
					UnitPath: filepath.Join(t.TempDir(), "agent-factory-upgrade-recovery-txn-1.service")},
			}); err != nil {
				t.Fatalf("Prepare: %v", err)
			}

			// The actor reports a positive commit signal: lease.Cleanup removed our
			// active.json, then the successor state in tc.setupActive took its place.
			runRecoveryActorFn = func(_ context.Context, _ upgradetxn.RecoveryInvocation, _ upgradetxn.Supervisor) (upgradetxn.RecoveryActorResult, error) {
				_ = os.Remove(filepath.Join(home, "upgrade", "active.json"))
				tc.setupActive(t, home, exe)
				return upgradetxn.RecoveryActorResult{Committed: true, JournalID: "txn-1", ExecutablePath: exe}, nil
			}
			adopted := false
			adoptAfterUpgradeCommitFn = func(string, string) error { adopted = true; return nil }

			if err := RunUpgradeRecoveryActor(context.Background(),
				upgradetxn.RecoveryInvocation{HomeDir: home, TransactionID: "txn-1"}); err != nil {
				t.Fatalf("RunUpgradeRecoveryActor: %v", err)
			}
			if adopted != tc.wantAdopt {
				t.Fatalf("hand-off: got adopted=%v want %v (%s)", adopted, tc.wantAdopt, tc.name)
			}
		})
	}
}

// TestRunUpgradeRecoveryActor_HoldsPrepareLockAcrossHandOff proves the active-journal
// interlock is not a TOCTOU: the Load that confirms no other active transaction owns the
// home and the destructive adoptAfterUpgradeCommit that arms the post-upgrade daemon run
// under the SAME preparation lock Prepare takes, so a concurrent Prepare cannot publish
// its own active.json between them. The adopt is held open while a successor Prepare
// races in; that Prepare must block until the hand-off releases the lock, proving the
// two operations are serialized against transaction publication rather than read-then-act
// on a journal a successor can land in the gap.
func TestRunUpgradeRecoveryActor_HoldsPrepareLockAcrossHandOff(t *testing.T) {
	home := stubForwardEnv(t)
	exe := filepath.Join(t.TempDir(), "af")
	if err := os.WriteFile(exe, []byte("previous-binary"), 0o755); err != nil {
		t.Fatalf("write fake previous binary: %v", err)
	}
	if _, err := upgradetxn.Prepare(upgradetxn.Plan{
		ID: "txn-1", HomeDir: home, ExecutablePath: exe,
		FromVersion: "1.0.100", ToVersion: "1.0.200", Candidate: []byte("candidate"),
		Daemon: upgradetxn.DaemonSnapshot{
			WasRunning: true, BootID: "boot-1",
			Owner: upgradetxn.DaemonOwner{Kind: upgradetxn.SupervisionSystemd, ServiceName: "agent-factory-daemon.service"},
		},
		RecoveryJob: upgradetxn.RecoveryJob{Kind: upgradetxn.RecoveryJobSystemd,
			Name:     "agent-factory-upgrade-recovery-txn-1.service",
			UnitPath: filepath.Join(t.TempDir(), "agent-factory-upgrade-recovery-txn-1.service")},
	}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// The actor commits: lease.Cleanup removed our active.json, and the commit
	// signal authorises the hand-off.
	runRecoveryActorFn = func(_ context.Context, _ upgradetxn.RecoveryInvocation, _ upgradetxn.Supervisor) (upgradetxn.RecoveryActorResult, error) {
		_ = os.Remove(filepath.Join(home, "upgrade", "active.json"))
		return upgradetxn.RecoveryActorResult{Committed: true, JournalID: "txn-1", ExecutablePath: exe}, nil
	}

	// Block the hand-off inside the prepare.lock so a concurrent Prepare can be
	// observed racing it. adoptStarted is closed when the hand-off is running
	// (inside the lock); adoptRelease gates its completion so the test controls
	// when the lock is dropped.
	adoptStarted := make(chan struct{})
	adoptRelease := make(chan struct{})
	adoptAfterUpgradeCommitFn = func(string, string) error {
		close(adoptStarted)
		<-adoptRelease
		return nil
	}

	runDone := make(chan error, 1)
	go func() {
		runDone <- RunUpgradeRecoveryActor(context.Background(),
			upgradetxn.RecoveryInvocation{HomeDir: home, TransactionID: "txn-1"})
	}()

	// Wait until the hand-off is running inside the lock before probing it.
	<-adoptStarted

	// Deterministically prove the hand-off is holding the preparation lock: a
	// non-blocking flock on prepare.lock must report EWOULDBLOCK while the
	// hand-off is inside adoptAfterUpgradeCommit. A read-then-act without the
	// lock would have dropped it by now.
	lockPath := filepath.Join(home, "upgrade", "prepare.lock")
	probe, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open prepare.lock to probe: %v", err)
	}
	probeErr := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	_ = probe.Close()
	if probeErr == nil {
		t.Fatalf("the preparation lock was not held while the hand-off was running; the interlock did not serialize Prepare")
	}

	// A successor Prepare on the same home must block on the preparation lock the
	// hand-off is holding, so its active.json cannot land between the Load and the
	// adopt — the TOCTOU the interlock closes. Confirm it has not published yet.
	successorExe := filepath.Join(t.TempDir(), "af-other")
	if err := os.WriteFile(successorExe, []byte("previous-binary-other"), 0o755); err != nil {
		t.Fatalf("write successor previous binary: %v", err)
	}
	prepareDone := make(chan error, 1)
	go func() {
		_, err := upgradetxn.Prepare(upgradetxn.Plan{
			ID: "txn-other", HomeDir: home, ExecutablePath: successorExe,
			FromVersion: "1.0.100", ToVersion: "1.0.200", Candidate: []byte("candidate-other"),
			Daemon: upgradetxn.DaemonSnapshot{
				WasRunning: true, BootID: "boot-other",
				Owner: upgradetxn.DaemonOwner{Kind: upgradetxn.SupervisionSystemd, ServiceName: "agent-factory-daemon.service"},
			},
			RecoveryJob: upgradetxn.RecoveryJob{Kind: upgradetxn.RecoveryJobSystemd,
				Name:     "agent-factory-upgrade-recovery-txn-other.service",
				UnitPath: filepath.Join(t.TempDir(), "agent-factory-upgrade-recovery-txn-other.service")},
		})
		prepareDone <- err
	}()

	select {
	case err := <-prepareDone:
		t.Fatalf("successor Prepare completed while the hand-off held the preparation lock; the interlock did not serialize Prepare: %v", err)
	case <-time.After(200 * time.Millisecond):
		// The Prepare is blocked on the lock the hand-off holds — the interlock holds.
	}

	// Releasing the adopt lets the hand-off (and thus the lock) drop. The
	// successor Prepare must then complete and publish its own transaction.
	close(adoptRelease)

	select {
	case err := <-prepareDone:
		if err != nil {
			t.Fatalf("successor Prepare failed after the lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("successor Prepare did not complete after the lock was released")
	}

	// The home now belongs to the successor: the Load the hand-off serialized
	// against must see the successor's transaction, not our committed one.
	txn, err := upgradetxn.Load(home)
	if err != nil {
		t.Fatalf("Load after successor Prepare: %v", err)
	}
	if got := txn.Journal().ID; got != "txn-other" {
		t.Fatalf("after the hand-off the active journal is %q, want the successor's txn-other", got)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("RunUpgradeRecoveryActor: %v", err)
	}
}

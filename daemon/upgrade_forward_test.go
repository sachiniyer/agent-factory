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
	waitForShutdownFn = func() error {
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
			waitForShutdownFn = func() error { return nil }
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

// RunUpgradeRecoveryActor arms the post-upgrade daemon ONLY on a positive commit
// signal: OUR transaction whose journal is gone afterward. A nil return from a
// stand-down (journal still present) must NOT trigger the hand-off — that
// conflation would let a stale recovery job kill a live daemon from a different,
// in-flight transaction (the P1-b failure). Both owner kinds hand off on commit:
// the ad-hoc candidate is parked in probation and must be respawned too, not just
// unit-owned homes.
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
		committed   bool // whether the actor removed the journal (a real commit)
		wantAdopt   bool
	}{
		{"systemd owner, committed", upgradetxn.SupervisionSystemd, "agent-factory-daemon.service", systemdJob, true, true},
		{"systemd owner, stand-down (journal retained)", upgradetxn.SupervisionSystemd, "agent-factory-daemon.service", systemdJob, false, false},
		{"ad-hoc owner, committed", upgradetxn.SupervisionAdHoc, "", upgradetxn.RecoveryJob{Kind: upgradetxn.RecoveryJobDetached}, true, true},
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

			runRecoveryActorFn = func(context.Context, upgradetxn.RecoveryInvocation, upgradetxn.Supervisor) error {
				if tc.committed {
					// Simulate the commit path's lease.Cleanup() removing the journal.
					_ = os.Remove(filepath.Join(home, "upgrade", "active.json"))
				}
				return nil
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

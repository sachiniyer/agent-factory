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
	pHealth := upgradeRecoveryHealthFn
	pUnit := startPreviousViaUnitFn
	pAdHoc := startPreviousAdHocFn
	pLaunch := launchCandidateDaemonFn
	pApproved := activationApprovedFn
	pRelease := releaseCandidateProbationFn
	pAdopt := adoptAfterUpgradeCommitFn
	pRun := runRecoveryActorFn
	t.Cleanup(func() {
		upgradeValidateGrace, upgradeValidatePoll = pg, pp
		awaitActivationGrace, awaitActivationPoll = ag, ap
		upgradeRecoveryHealthFn = pHealth
		startPreviousViaUnitFn = pUnit
		startPreviousAdHocFn = pAdHoc
		launchCandidateDaemonFn = pLaunch
		activationApprovedFn = pApproved
		releaseCandidateProbationFn = pRelease
		adoptAfterUpgradeCommitFn = pAdopt
		runRecoveryActorFn = pRun
	})
	upgradeValidateGrace, upgradeValidatePoll = 500*time.Millisecond, time.Millisecond
	awaitActivationGrace, awaitActivationPoll = 500*time.Millisecond, time.Millisecond
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
	activationApprovedFn = func(upgradetxn.Journal) (bool, error) { return true, nil }
	launchCandidateDaemonFn = func(_, id string) error {
		*state = fakeDaemon{up: true, version: "1.0.200", txnID: id, httpBound: true}
		return nil
	}
	released := false
	releaseCandidateProbationFn = func(id string) error {
		if id == state.txnID {
			state.txnID = ""
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
	if !released || state.txnID != "" {
		t.Fatal("commit did not release the candidate from probation")
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

// ValidateCandidate is the mirror of previousDaemonHealthy: it must REQUIRE the
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

	if err := lifecycle.releaseUpgradeProbation("other-txn"); err == nil {
		t.Fatal("a mismatched transaction id must be refused")
	}
	if err := lifecycle.releaseUpgradeProbation("txn-1"); err != nil {
		t.Fatalf("matching release: %v", err)
	}
	if lifecycle.snapshot().phase != DaemonPhaseReady || lifecycle.isUpgradeProbation() {
		t.Fatal("release must clear probation and mark ready")
	}
	if err := lifecycle.releaseUpgradeProbation("txn-1"); err == nil {
		t.Fatal("releasing a daemon that is not in probation must be refused")
	}
}

func TestAdoptAfterUpgradeCommit_NoUnitOwnerNoOps(t *testing.T) {
	// A throwaway home has no installed unit, so ResolveSupervisionOwner is ad-hoc
	// and there is nothing to hand off to — a no-op success, never a StopDaemon.
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	withAutostartTestEnv(t, "linux") // no unit file written → ad-hoc owner
	if err := adoptAfterUpgradeCommit(); err != nil {
		t.Fatalf("with no installed unit adopt must no-op, got %v", err)
	}
}

// RunUpgradeRecoveryActor hands a unit-owned home's committed daemon to the
// installed unit after Supervisor.Run returns, and skips the hand-off for an
// ad-hoc home.
func TestRunUpgradeRecoveryActor_AdoptsUnitOwnerAfterSuccess(t *testing.T) {
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
		wantAdopt   bool
	}{
		{"systemd owner adopts", upgradetxn.SupervisionSystemd, "agent-factory-daemon.service", systemdJob, true},
		{"ad-hoc owner does not adopt", upgradetxn.SupervisionAdHoc, "", upgradetxn.RecoveryJob{Kind: upgradetxn.RecoveryJobDetached}, false},
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
				return nil // commit succeeded
			}
			adopted := false
			adoptAfterUpgradeCommitFn = func() error { adopted = true; return nil }

			if err := RunUpgradeRecoveryActor(context.Background(),
				upgradetxn.RecoveryInvocation{HomeDir: home, TransactionID: "txn-1"}); err != nil {
				t.Fatalf("RunUpgradeRecoveryActor: %v", err)
			}
			if adopted != tc.wantAdopt {
				t.Fatalf("adopt-at-commit: got %v want %v for %s owner", adopted, tc.wantAdopt, tc.ownerKind)
			}
		})
	}
}

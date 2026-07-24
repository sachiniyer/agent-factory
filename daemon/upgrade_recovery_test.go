package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
)

// stubRecoveryOps replaces the recovery-op collaborators with hermetic fakes,
// shortens validatePreviousDaemon's polling, and binds AGENT_FACTORY_HOME to a
// throwaway home so the recovery-home guard passes. It returns that home so
// tests build journals whose HomeDir matches. Everything is restored on cleanup.
func stubRecoveryOps(t *testing.T) string {
	t.Helper()
	prevHealth := upgradeRecoveryHealthFn
	prevUnit := startPreviousViaUnitFn
	prevAdHoc := startPreviousAdHocFn
	prevGrace := upgradeValidateGrace
	prevPoll := upgradeValidatePoll
	t.Cleanup(func() {
		upgradeRecoveryHealthFn = prevHealth
		startPreviousViaUnitFn = prevUnit
		startPreviousAdHocFn = prevAdHoc
		upgradeValidateGrace = prevGrace
		upgradeValidatePoll = prevPoll
	})
	upgradeValidateGrace = 500 * time.Millisecond
	upgradeValidatePoll = time.Millisecond
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	return home
}

func journalAt(home, fromVersion string, listeners upgradetxn.ListenerExpectation) upgradetxn.Journal {
	return upgradetxn.Journal{
		HomeDir:     home,
		FromVersion: fromVersion,
		Daemon:      upgradetxn.DaemonSnapshot{Listeners: listeners},
	}
}

// previousDaemonHealthy is the rollback readiness gate: "something answered" is
// not health (#1947). It must require the exact FromVersion, every listener that
// was healthy before the upgrade, and — critically — that the responder is NOT a
// surviving upgrade candidate (which carries a transaction id).
func TestPreviousDaemonHealthy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		health  HealthStatus
		journal upgradetxn.Journal
		wantErr bool
	}{
		{
			name:    "healthy exact version, no listeners required",
			health:  HealthStatus{DaemonVersion: "1.0.100"},
			journal: journalAt("", "1.0.100", upgradetxn.ListenerExpectation{}),
			wantErr: false,
		},
		{
			name:    "not answering",
			health:  HealthStatus{PingErr: errors.New("connection refused")},
			journal: journalAt("", "1.0.100", upgradetxn.ListenerExpectation{}),
			wantErr: true,
		},
		{
			name:    "answered but no version reported (older responder)",
			health:  HealthStatus{},
			journal: journalAt("", "1.0.100", upgradetxn.ListenerExpectation{}),
			wantErr: true,
		},
		{
			name:    "wrong version is not the previous daemon",
			health:  HealthStatus{DaemonVersion: "1.0.210"},
			journal: journalAt("", "1.0.100", upgradetxn.ListenerExpectation{}),
			wantErr: true,
		},
		{
			// The from==to case (a rebuild/reinstall): a surviving candidate matches
			// the version, but its non-empty transaction id gives it away. Without
			// the transaction-id check this would falsely pass as rolled back.
			name:    "surviving candidate at the same version is rejected by its transaction id",
			health:  HealthStatus{DaemonVersion: "1.0.100", TransactionID: "txn-abc"},
			journal: journalAt("", "1.0.100", upgradetxn.ListenerExpectation{}),
			wantErr: true,
		},
		{
			name:    "http listener not rebound",
			health:  HealthStatus{DaemonVersion: "1.0.100", Listeners: DaemonListenerStatus{HTTPUnixBound: false}},
			journal: journalAt("", "1.0.100", upgradetxn.ListenerExpectation{HTTPUnixBound: true}),
			wantErr: true,
		},
		{
			name:    "http listener rebound",
			health:  HealthStatus{DaemonVersion: "1.0.100", Listeners: DaemonListenerStatus{HTTPUnixBound: true}},
			journal: journalAt("", "1.0.100", upgradetxn.ListenerExpectation{HTTPUnixBound: true}),
			wantErr: false,
		},
		{
			name:    "tcp listener not rebound",
			health:  HealthStatus{DaemonVersion: "1.0.100", Listeners: DaemonListenerStatus{TCPBound: false}},
			journal: journalAt("", "1.0.100", upgradetxn.ListenerExpectation{TCPConfigured: true, TCPBound: true}),
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := previousDaemonHealthy(tc.health, tc.journal)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an unhealthy verdict, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected healthy, got %v", err)
			}
		})
	}
}

// The forward-activation operations are fail-closed in R1: this build can never
// quiesce or replace a live daemon, so an interrupted transaction can only ever
// resolve toward rollback, never forward.
func TestForwardActivationOpsAreNotEnabled(t *testing.T) {
	ops := productionSupervisorOperations()
	journal := upgradetxn.Journal{}
	ctx := context.Background()

	forward := map[string]func() error{
		"AwaitActivation":   func() error { return ops.AwaitActivation(ctx, journal) },
		"StartCandidate":    func() error { return ops.StartCandidate(ctx, journal) },
		"ValidateCandidate": func() error { return ops.ValidateCandidate(ctx, journal) },
		"ApproveCandidate":  func() error { return ops.ApproveCandidate(ctx, journal) },
		"StopPrevious": func() error {
			_, err := ops.StopPrevious(ctx, journal)
			return err
		},
	}
	for name, call := range forward {
		if err := call(); !errors.Is(err, ErrUpgradeActivationNotEnabled) {
			t.Fatalf("%s must be fail-closed in R1; got %v", name, err)
		}
	}

	// The recovery-side operations must NOT be the not-enabled sentinel — they are
	// the real path this slice delivers.
	if ops.StopCandidate == nil || ops.StartPrevious == nil || ops.ValidatePrevious == nil || ops.DisableRecoveryJob == nil {
		t.Fatal("recovery-side operations must be wired in R1")
	}
}

// P2: every home-dependent op acts on config.GetConfigDir()'s home, so if the
// process is not bound to the transaction's home, a "stop" would report success
// against a daemon it cannot even see — restoring the previous binary under a
// live candidate. The op must fail closed (StopUnknown), never fabricate a stop.
func TestStopCandidateDaemon_HomeMismatchFailsClosed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	journal := upgradetxn.Journal{HomeDir: "/some/other/af/home"}

	outcome, err := stopCandidateDaemon(context.Background(), journal)
	if outcome != upgradetxn.StopUnknown {
		t.Fatalf("a home mismatch must return StopUnknown, never a fabricated stop; got %v", outcome)
	}
	if err == nil {
		t.Fatal("a home mismatch must return an error, not proceed on the wrong daemon")
	}
}

func TestStartPreviousDaemon_HomeMismatchRefuses(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	prevUnit := startPreviousViaUnitFn
	t.Cleanup(func() { startPreviousViaUnitFn = prevUnit })
	called := false
	startPreviousViaUnitFn = func() error { called = true; return nil }

	journal := upgradetxn.Journal{
		HomeDir: "/some/other/af/home",
		Daemon:  upgradetxn.DaemonSnapshot{Owner: upgradetxn.DaemonOwner{Kind: upgradetxn.SupervisionSystemd}},
	}
	if err := startPreviousDaemon(context.Background(), journal); err == nil {
		t.Fatal("a home mismatch must refuse to start a daemon for the wrong home")
	}
	if called {
		t.Fatal("a home mismatch must not touch the local unit")
	}
}

func TestStartPreviousDaemon_DispatchesByOwner(t *testing.T) {
	for _, tc := range []struct {
		kind      upgradetxn.SupervisionKind
		wantUnit  bool
		wantAdHoc bool
	}{
		{upgradetxn.SupervisionSystemd, true, false},
		{upgradetxn.SupervisionLaunchd, true, false},
		{upgradetxn.SupervisionAdHoc, false, true},
		{upgradetxn.SupervisionNone, false, true},
	} {
		t.Run(string(tc.kind)+"-owner", func(t *testing.T) {
			home := stubRecoveryOps(t)
			unitCalled, adHocCalled := false, false
			startPreviousViaUnitFn = func() error { unitCalled = true; return nil }
			startPreviousAdHocFn = func(string) error { adHocCalled = true; return nil }

			j := upgradetxn.Journal{
				HomeDir:        home,
				ExecutablePath: "/opt/agent-factory/bin/af",
				Daemon:         upgradetxn.DaemonSnapshot{Owner: upgradetxn.DaemonOwner{Kind: tc.kind}},
			}
			if err := startPreviousDaemon(context.Background(), j); err != nil {
				t.Fatalf("startPreviousDaemon: %v", err)
			}
			if unitCalled != tc.wantUnit || adHocCalled != tc.wantAdHoc {
				t.Fatalf("owner %s dispatched wrong: unit=%v adhoc=%v", tc.kind, unitCalled, adHocCalled)
			}
		})
	}
}

func TestValidatePreviousDaemon_WaitsThenValidates(t *testing.T) {
	home := stubRecoveryOps(t)
	calls := 0
	upgradeRecoveryHealthFn = func() HealthStatus {
		calls++
		if calls < 3 {
			return HealthStatus{PingErr: errors.New("previous daemon not up yet")}
		}
		return HealthStatus{DaemonVersion: "1.0.100"}
	}
	if err := validatePreviousDaemon(context.Background(), journalAt(home, "1.0.100", upgradetxn.ListenerExpectation{})); err != nil {
		t.Fatalf("validate must succeed once the previous daemon answers at FromVersion: %v", err)
	}
}

func TestValidatePreviousDaemon_WrongVersionFailsRollback(t *testing.T) {
	home := stubRecoveryOps(t)
	// The candidate never went away; a newer version keeps answering. Validation
	// must fail so the supervisor rolls back rather than declaring success.
	upgradeRecoveryHealthFn = func() HealthStatus { return HealthStatus{DaemonVersion: "1.0.210"} }
	if err := validatePreviousDaemon(context.Background(), journalAt(home, "1.0.100", upgradetxn.ListenerExpectation{})); err == nil {
		t.Fatal("validate must fail when the responder is not the previous version")
	}
}

// The P1 end-to-end at the ops layer the review flagged as missing: a live
// recovery actor restarts the previous daemon and then validates it. The gate's
// rollback-restore exemption (which lets that restarted daemon bind instead of
// deferring to this very actor) is proven in upgrade_gate_test.go; here we prove
// the operations the actor drives actually bring the previous daemon back.
func TestRollbackRestore_RestartsThenValidatesPreviousDaemon(t *testing.T) {
	home := stubRecoveryOps(t)
	started := false
	startPreviousViaUnitFn = func() error { started = true; return nil }
	upgradeRecoveryHealthFn = func() HealthStatus {
		if !started {
			// Before StartPrevious the candidate is gone and nothing answers.
			return HealthStatus{PingErr: errors.New("no daemon")}
		}
		return HealthStatus{DaemonVersion: "1.0.100", Listeners: DaemonListenerStatus{HTTPUnixBound: true}}
	}

	ops := productionSupervisorOperations()
	journal := upgradetxn.Journal{
		HomeDir:        home,
		FromVersion:    "1.0.100",
		ExecutablePath: "/opt/agent-factory/bin/af",
		Daemon: upgradetxn.DaemonSnapshot{
			Owner:     upgradetxn.DaemonOwner{Kind: upgradetxn.SupervisionSystemd, ServiceName: "agent-factory-daemon.service"},
			Listeners: upgradetxn.ListenerExpectation{HTTPUnixBound: true},
		},
	}
	ctx := context.Background()

	if err := ops.StartPrevious(ctx, journal); err != nil {
		t.Fatalf("StartPrevious: %v", err)
	}
	if !started {
		t.Fatal("StartPrevious did not restart the previous daemon under its unit")
	}
	if err := ops.ValidatePrevious(ctx, journal); err != nil {
		t.Fatalf("ValidatePrevious must confirm the restored previous daemon: %v", err)
	}
}

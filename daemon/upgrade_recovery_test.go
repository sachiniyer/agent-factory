package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
)

// stubRecoveryOps replaces the recovery-op collaborators with hermetic fakes and
// shortens validatePreviousDaemon's polling, restoring everything on cleanup.
func stubRecoveryOps(t *testing.T) {
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
}

func journalAt(fromVersion string, listeners upgradetxn.ListenerExpectation) upgradetxn.Journal {
	return upgradetxn.Journal{
		FromVersion: fromVersion,
		Daemon:      upgradetxn.DaemonSnapshot{Listeners: listeners},
	}
}

// previousDaemonHealthy is the rollback readiness gate: "something answered" is
// not health (#1947). It must require the exact FromVersion and every listener
// that was healthy before the upgrade.
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
			journal: journalAt("1.0.100", upgradetxn.ListenerExpectation{}),
			wantErr: false,
		},
		{
			name:    "not answering",
			health:  HealthStatus{PingErr: errors.New("connection refused")},
			journal: journalAt("1.0.100", upgradetxn.ListenerExpectation{}),
			wantErr: true,
		},
		{
			name:    "answered but no version reported (older responder)",
			health:  HealthStatus{},
			journal: journalAt("1.0.100", upgradetxn.ListenerExpectation{}),
			wantErr: true,
		},
		{
			name:    "wrong version is not the previous daemon",
			health:  HealthStatus{DaemonVersion: "1.0.210"},
			journal: journalAt("1.0.100", upgradetxn.ListenerExpectation{}),
			wantErr: true,
		},
		{
			name:    "http listener not rebound",
			health:  HealthStatus{DaemonVersion: "1.0.100", Listeners: DaemonListenerStatus{HTTPUnixBound: false}},
			journal: journalAt("1.0.100", upgradetxn.ListenerExpectation{HTTPUnixBound: true}),
			wantErr: true,
		},
		{
			name:    "http listener rebound",
			health:  HealthStatus{DaemonVersion: "1.0.100", Listeners: DaemonListenerStatus{HTTPUnixBound: true}},
			journal: journalAt("1.0.100", upgradetxn.ListenerExpectation{HTTPUnixBound: true}),
			wantErr: false,
		},
		{
			name:    "tcp listener not rebound",
			health:  HealthStatus{DaemonVersion: "1.0.100", Listeners: DaemonListenerStatus{TCPBound: false}},
			journal: journalAt("1.0.100", upgradetxn.ListenerExpectation{TCPConfigured: true, TCPBound: true}),
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
			stubRecoveryOps(t)
			unitCalled, adHocCalled := false, false
			startPreviousViaUnitFn = func() error { unitCalled = true; return nil }
			startPreviousAdHocFn = func(string) error { adHocCalled = true; return nil }

			j := upgradetxn.Journal{
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
	stubRecoveryOps(t)
	calls := 0
	upgradeRecoveryHealthFn = func() HealthStatus {
		calls++
		if calls < 3 {
			return HealthStatus{PingErr: errors.New("previous daemon not up yet")}
		}
		return HealthStatus{DaemonVersion: "1.0.100"}
	}
	if err := validatePreviousDaemon(context.Background(), journalAt("1.0.100", upgradetxn.ListenerExpectation{})); err != nil {
		t.Fatalf("validate must succeed once the previous daemon answers at FromVersion: %v", err)
	}
}

func TestValidatePreviousDaemon_WrongVersionFailsRollback(t *testing.T) {
	stubRecoveryOps(t)
	// The candidate never went away; a newer version keeps answering. Validation
	// must fail so the supervisor rolls back rather than declaring success.
	upgradeRecoveryHealthFn = func() HealthStatus { return HealthStatus{DaemonVersion: "1.0.210"} }
	if err := validatePreviousDaemon(context.Background(), journalAt("1.0.100", upgradetxn.ListenerExpectation{})); err == nil {
		t.Fatal("validate must fail when the responder is not the previous version")
	}
}

// The P1 end-to-end at the ops layer the review flagged as missing: a live
// recovery actor restarts the previous daemon and then validates it. The gate's
// rollback-restore exemption (which lets that restarted daemon bind instead of
// deferring to this very actor) is proven in upgrade_gate_test.go; here we prove
// the operations the actor drives actually bring the previous daemon back.
func TestRollbackRestore_RestartsThenValidatesPreviousDaemon(t *testing.T) {
	stubRecoveryOps(t)
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

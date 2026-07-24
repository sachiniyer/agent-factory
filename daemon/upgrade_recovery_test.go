package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
)

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

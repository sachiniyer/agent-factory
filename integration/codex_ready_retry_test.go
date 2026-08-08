package integration_test

import (
	"context"
	"errors"
	"testing"
)

func TestRunAFUntilDaemonAdmitsRetriesWarmupRefusal(t *testing.T) {
	t.Parallel()

	const daemonStarting = "agent-factory daemon is starting (restoring sessions); retry shortly"
	calls := 0
	out, err := runAFUntilDaemonAdmits(context.Background(), 0, func() (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New(daemonStarting)
		}
		return "created", nil
	})
	if err != nil {
		t.Fatalf("runAFUntilDaemonAdmits: %v", err)
	}
	if out != "created" {
		t.Fatalf("output = %q, want created", out)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestRunAFUntilDaemonAdmitsDoesNotRetryOtherFailures(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("agent readiness failed")
	calls := 0
	out, err := runAFUntilDaemonAdmits(context.Background(), 0, func() (string, error) {
		calls++
		return "pane diagnostics", wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if out != "pane diagnostics" {
		t.Fatalf("output = %q, want pane diagnostics", out)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

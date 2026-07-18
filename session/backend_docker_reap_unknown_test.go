package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// #2049: a wedged / timed-out `docker rm -f` reap actually LEAKED a container, but
// the reap returned a PLAIN error. KillSession/deleteSessionRecord classify a
// teardown by session.TeardownStateUnknown — retain on ErrWorkspaceStateUnknown /
// ErrPaneMayBeLive, otherwise treat it as a KNOWN teardown and delete the row. So
// a timeout was classified "known" → the row was deleted → the leaked container
// was orphaned with no record pointing at it.
//
// This is the fabricated-negative / teardown-taxonomy family: a two-valued error
// cannot say "I don't know whether the container is gone", so a timeout reads as
// "gone". The fix maps a tripped deadline to ErrWorkspaceStateUnknown so the row
// is RETAINED (and the leaked container surfaced) rather than silently deleted.

// withShortDockerReapTimeout shortens the reap deadline so the timeout path is
// exercised in milliseconds, restoring it after the test.
func withShortDockerReapTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := dockerReapTimeout
	dockerReapTimeout = d
	t.Cleanup(func() { dockerReapTimeout = prev })
}

// TestDockerReapTimeoutIsWorkspaceStateUnknown is the #2049 regression: a reap
// whose `docker rm -f` is killed on its deadline (the container's state is
// therefore unknown — it may still be running) must wrap ErrWorkspaceStateUnknown,
// so TeardownStateUnknown returns true and the daemon RETAINS the row.
//
// The injected dockerExec faithfully models a deadline kill: it blocks until the
// caller's context is cancelled and returns ctx.Err() (context.DeadlineExceeded),
// exactly as exec.CommandContext's kill surfaces to reap via ctx.Err(). Before the
// fix reap returned a plain error and TeardownStateUnknown was false.
func TestDockerReapTimeoutIsWorkspaceStateUnknown(t *testing.T) {
	withShortDockerReapTimeout(t, 150*time.Millisecond)
	restore := SetDockerExecForTest(func(ctx context.Context, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	defer restore()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "wedged"}, containerID: "deadbeefcafe0000"}

	done := make(chan error, 1)
	go func() { done <- p.reap() }()

	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reap did not return within 10s — dockerReapTimeout/dockerWaitDelay is not bounding the reap")
	}

	if err == nil {
		t.Fatal("a timed-out `docker rm -f` must return an error, got nil")
	}
	if !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("a wedged/timed-out reap must wrap ErrWorkspaceStateUnknown so the row is retained (#2049); got a plain error: %v", err)
	}
	if !TeardownStateUnknown(err) {
		t.Fatalf("TeardownStateUnknown must be true for a timed-out reap so KillSession/deleteSessionRecord RETAIN the row instead of orphaning the leaked container; got false for: %v", err)
	}
}

// TestDockerReapReportedErrorStaysKnown locks the polarity in the other
// direction: a reap that docker ANSWERED with an error (the container is already
// gone, or docker reported a real problem) is a teardown that TOLD us something —
// it must stay a KNOWN-state error so the row may be deleted, per the documented
// deleteSessionRecord contract. Mapping every reap failure to unknown would make
// the "No such container" (already-gone) case wedge the record forever.
func TestDockerReapReportedErrorStaysKnown(t *testing.T) {
	withShortDockerReapTimeout(t, 5*time.Second) // never reached; the fake answers instantly
	restore := SetDockerExecForTest(func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("Error: No such container: deadbeef"), fmt.Errorf("exit status 1")
	})
	defer restore()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "answered"}, containerID: "deadbeefcafe0000"}
	err := p.reap()
	if err == nil {
		t.Fatal("a failed `docker rm -f` must return an error")
	}
	if errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("a reap docker ANSWERED with an error must NOT be classified unknown — that would wedge the already-gone case forever: %v", err)
	}
	if TeardownStateUnknown(err) {
		t.Fatalf("TeardownStateUnknown must be false for an answered reap failure so the row may be deleted: %v", err)
	}
}

// TestDockerReapSuccessReturnsNil guards the healthy path: a clean reap returns
// nil, so the WaitDelay/timeout plumbing never turns a successful `docker rm -f`
// into a phantom leak report.
func TestDockerReapSuccessReturnsNil(t *testing.T) {
	restore := SetDockerExecForTest(func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("deadbeefcafe0000\n"), nil
	})
	defer restore()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "clean"}, containerID: "deadbeefcafe0000"}
	if err := p.reap(); err != nil {
		t.Fatalf("a successful reap must return nil, got %v", err)
	}
}

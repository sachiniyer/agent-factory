package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const dockerReapTestEngineID = "engine-reap-test"

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

// TestDockerProvisionFailureSurfacesUnknownReap covers the no-record failure
// path: if provisioning fails after creating a container and its immediate reap
// times out, both causes and the orphan risk must reach the session creator.
func TestDockerProvisionFailureSurfacesUnknownReap(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// A local engine so the pre-run locality guard passes; this test exercises
	// the create-then-fail reap path, not the remote-engine refusal.
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{
		"backend": "docker",
		"docker":  map[string]any{"image": "img:latest"},
	})
	defer SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil })()
	defer SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af"))()
	withShortDockerReapTimeout(t, 50*time.Millisecond)

	provisionErr := errors.New("permission denied")
	var reapCalls atomic.Int32
	defer SetDockerExecForTest(func(ctx context.Context, _ []string, args ...string) ([]byte, error) {
		switch args[0] {
		case "info":
			return []byte(dockerReapTestEngineID + "\n"), nil
		case "run":
			return []byte(dockerCreatedID + "\n"), nil
		case "exec":
			return nil, provisionErr
		case "rm":
			reapCalls.Add(1)
			<-ctx.Done()
			// exec.CommandContext reports the killed process (for example,
			// "signal: killed"), not ctx.Err(); reap must add the deadline.
			return nil, errors.New("signal: killed")
		default:
			return nil, fmt.Errorf("unexpected docker command: %v", args)
		}
	})()

	_, err := dockerRuntime{}.Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "partial-container",
		CloneURL: "file:///repo",
	})
	if !errors.Is(err, provisionErr) || !errors.Is(err, ErrWorkspaceStateUnknown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("provision and unknown cleanup causes must all survive, got %v", err)
	}
	if reapCalls.Load() != 1 {
		t.Fatalf("failed provision attempted container cleanup %d times, want 1", reapCalls.Load())
	}
	for _, detail := range []string{shortContainerID(dockerCreatedID), "partial-container", "may still be running", "inspect it before retrying"} {
		if !strings.Contains(err.Error(), detail) {
			t.Fatalf("provision cleanup error omitted %q: %v", detail, err)
		}
	}
}

// TestDockerReapTimeoutIsWorkspaceStateUnknown is the #2049 regression: a reap
// whose `docker rm -f` is killed on its deadline (the container's state is
// therefore unknown — it may still be running) must wrap ErrWorkspaceStateUnknown,
// so TeardownStateUnknown returns true and the daemon RETAINS the row.
//
// The injected dockerExec faithfully models a deadline kill: it blocks until the
// caller's context is cancelled, then returns the killed process error rather
// than ctx.Err(), exactly as exec.CommandContext does. reap must add the context
// deadline itself. Before the fix it returned only a plain process error.
func TestDockerReapTimeoutIsWorkspaceStateUnknown(t *testing.T) {
	withShortDockerReapTimeout(t, 150*time.Millisecond)
	restore := SetDockerExecForTest(func(ctx context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte(dockerReapTestEngineID + "\n"), nil
		}
		<-ctx.Done()
		return nil, errors.New("signal: killed")
	})
	defer restore()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "wedged"}, containerID: "deadbeefcafe0000", engineID: dockerReapTestEngineID}

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
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a timed-out reap must preserve the actual context deadline, got %v", err)
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
	restore := SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte(dockerReapTestEngineID + "\n"), nil
		}
		return []byte("Error: No such container: deadbeef"), fmt.Errorf("exit status 1")
	})
	defer restore()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "answered"}, containerID: "deadbeefcafe0000", engineID: dockerReapTestEngineID}
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
	restore := SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte(dockerReapTestEngineID + "\n"), nil
		}
		return []byte("deadbeefcafe0000\n"), nil
	})
	defer restore()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "clean"}, containerID: "deadbeefcafe0000", engineID: dockerReapTestEngineID}
	if err := p.reap(); err != nil {
		t.Fatalf("a successful reap must return nil, got %v", err)
	}
}

// reapWithin runs p.reap() (which blocks up to dockerReapTimeout) in a goroutine and
// returns its error, failing the test if it does not return within guard.
func reapWithin(t *testing.T, p *dockerProvisioner, guard time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.reap() }()
	select {
	case err := <-done:
		return err
	case <-time.After(guard):
		t.Fatalf("reap did not return within %s", guard)
		return nil
	}
}

// TestDockerReapTimeoutIsReRunnable is the #2063-review guard: a timed-out reap must
// NOT latch. The daemon's finishUserKill re-invokes instance.Kill() → reap() on
// every poll for a retained (tombstoned) record; if reap latched on the first
// timeout, the second call would return nil — the container never re-reaped — so the
// row would be deleted and the container orphaned exactly one poll later, defeating
// the fix. A second reap() while docker is still wedged must (i) actually re-attempt
// `docker rm -f` and (ii) keep returning the unknown sentinel, never nil.
func TestDockerReapTimeoutIsReRunnable(t *testing.T) {
	withShortDockerReapTimeout(t, 150*time.Millisecond)
	var calls int32
	restore := SetDockerExecForTest(func(ctx context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte(dockerReapTestEngineID + "\n"), nil
		}
		atomic.AddInt32(&calls, 1)
		<-ctx.Done()
		return nil, errors.New("signal: killed")
	})
	defer restore()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "wedged"}, containerID: "deadbeefcafe0000", engineID: dockerReapTestEngineID}

	first := reapWithin(t, p, 10*time.Second)
	if !errors.Is(first, ErrWorkspaceStateUnknown) {
		t.Fatalf("first timed-out reap must wrap ErrWorkspaceStateUnknown, got %v", first)
	}

	second := reapWithin(t, p, 10*time.Second)
	if second == nil {
		t.Fatal("second reap after a timeout returned nil — the latch skipped the retry, so the row " +
			"would be deleted and the container orphaned one poll later (#2063 review)")
	}
	if !errors.Is(second, ErrWorkspaceStateUnknown) {
		t.Fatalf("a second reap while docker is still wedged must keep returning ErrWorkspaceStateUnknown, got %v", second)
	}
	if !TeardownStateUnknown(second) {
		t.Fatalf("the retained row depends on TeardownStateUnknown staying true across retries, got false for: %v", second)
	}
	if n := atomic.LoadInt32(&calls); n < 2 {
		t.Fatalf("a timed-out reap must actually re-attempt `docker rm -f` on the next call; dockerExec ran %d times, want >= 2", n)
	}
}

// TestDockerReapTimeoutThenSuccessClears completes the retry contract: once docker
// recovers, a reap after a prior timeout actually reaps the container and returns
// nil, so the row is finally deleted rather than retained forever — and a further
// reap stays latched at nil.
//
// The call-count assertion is what makes this discriminating rather than
// decorative: a latching reap ALSO returns nil on the second call (that is the
// #2063-review bug), so "returned nil" alone passes on the broken code. Only proving
// `docker rm -f` actually RAN again distinguishes a real re-attempt from a stale
// latched nil.
func TestDockerReapTimeoutThenSuccessClears(t *testing.T) {
	withShortDockerReapTimeout(t, 150*time.Millisecond)
	var wedged atomic.Bool
	var calls int32
	wedged.Store(true)
	restore := SetDockerExecForTest(func(ctx context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte(dockerReapTestEngineID + "\n"), nil
		}
		atomic.AddInt32(&calls, 1)
		if wedged.Load() {
			<-ctx.Done()
			return nil, errors.New("signal: killed")
		}
		return []byte("deadbeefcafe0000\n"), nil
	})
	defer restore()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "recovers"}, containerID: "deadbeefcafe0000", engineID: dockerReapTestEngineID}

	if err := reapWithin(t, p, 10*time.Second); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("first (wedged) reap must be unknown, got %v", err)
	}
	wedged.Store(false) // docker recovers
	if err := reapWithin(t, p, 10*time.Second); err != nil {
		t.Fatalf("after docker recovers, the retry must reap and return nil so the row is deleted, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n < 2 {
		t.Fatalf("the post-recovery reap must actually re-run `docker rm -f`, not return a latched nil; "+
			"dockerExec ran %d times, want >= 2 (a latched nil would leave the container leaked)", n)
	}
	// Now that a reap COMPLETED, it latches: no further docker command, still nil.
	after := atomic.LoadInt32(&calls)
	if err := reapWithin(t, p, 10*time.Second); err != nil {
		t.Fatalf("a completed reap must latch nil, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != after {
		t.Fatalf("a completed reap must not re-run `docker rm -f` (the Kill-retry collapse the latch exists for); ran %d more times", n-after)
	}
}

// TestDockerAnsweredTransportFailure pins the matcher that classifies a
// `docker rm -f` failure as a transport failure (daemon never reached) rather
// than a daemon answer. The reap's answer taxonomy hinges on this predicate,
// so a docker CLI wording change that silently escapes it would regress the
// fix exactly the way the original omission did. Matched strings are the
// transports the bug reclassifies; the non-matched ones guard the opposite
// polarity (a real answer must still latch known-state).
func TestDockerAnsweredTransportFailure(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{name: "daemon down unix socket", out: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", want: true},
		{name: "daemon down tcp host", out: "Cannot connect to the Docker daemon at tcp://10.0.0.1:2376. Is the docker daemon running?", want: true},
		{name: "daemon down docker 29+ banner", out: "failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running: dial unix /var/run/docker.sock: connect: no such file or directory", want: true},
		{name: "socket permission denied", out: "Got permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: dial unix /var/run/docker.sock: connect: permission denied", want: true},
		{name: "unresolvable pinned context", out: `context "missing": context not found`, want: true},
		{name: "no such container is a real answer", out: "Error: No such container: deadbeefcafe0000", want: false},
		{name: "real daemon answer", out: "Error response from daemon: removal of container deadbeefcafe0000 is already in progress", want: false},
		{name: "bare permission denied is not transport", out: "rm: permission denied", want: false},
		{name: "empty output", out: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dockerAnsweredTransportFailure([]byte(tc.out)); got != tc.want {
				t.Fatalf("dockerAnsweredTransportFailure(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// TestDockerReapDaemonUnreachableIsWorkspaceStateUnknown is the #2063-review
// omission a dockerd blip exposes: a `docker rm -f` the CLI fails because the
// daemon is UNREACHABLE (down/restarting, socket access denied, an unresolvable
// pinned DOCKER_CONTEXT) is a non-deadline "answer" the buggy latch classified
// as a KNOWN-state teardown — so the row was deleted while the container lived
// and no in-process handle remained to retry it. It must, like a timeout, wrap
// ErrWorkspaceStateUnknown so TeardownStateUnknown keeps the row, and it must
// NOT latch so the daemon's kill-retry can re-run the rm.
func TestDockerReapDaemonUnreachableIsWorkspaceStateUnknown(t *testing.T) {
	cases := []struct {
		name string
		out  string
	}{
		{name: "daemon down", out: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"},
		{name: "daemon down docker 29+ banner", out: "failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct and if the daemon is running: dial unix /var/run/docker.sock: connect: no such file or directory"},
		{name: "socket permission denied", out: "Got permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: dial unix /var/run/docker.sock: connect: permission denied"},
		{name: "unresolvable context", out: `context "missing": context not found`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withShortDockerReapTimeout(t, 5*time.Second) // never reached; the fake answers instantly
			restore := SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "info" {
					return []byte(dockerReapTestEngineID + "\n"), nil
				}
				return []byte(tc.out), errors.New("exit status 1")
			})
			defer restore()

			p := &dockerProvisioner{spec: ProvisionSpec{Title: "unreachable"}, containerID: "deadbeefcafe0000", engineID: dockerReapTestEngineID}
			err := p.reap()
			if err == nil {
				t.Fatal("a transport-failing `docker rm -f` must return an error, got nil")
			}
			if !errors.Is(err, ErrWorkspaceStateUnknown) {
				t.Fatalf("a daemon-unreachable reap must wrap ErrWorkspaceStateUnknown so the row is retained; got a plain error: %v", err)
			}
			if !TeardownStateUnknown(err) {
				t.Fatalf("TeardownStateUnknown must be true for a daemon-unreachable reap so KillSession/deleteSessionRecord RETAIN the row; got false for: %v", err)
			}
			if p.reaped {
				t.Fatalf("a daemon-unreachable reap must NOT latch so the kill-retry can re-run `docker rm -f` once Docker access is restored; p.reaped was true")
			}
		})
	}
}

// TestDockerReapDaemonUnreachableIsReRunnable is the no-latch contract for a
// transport failure, mirroring TestDockerReapTimeoutIsReRunnable: the daemon's
// finishUserKill re-invokes reap() on every poll for a retained record, so a
// transport failure that latched would make the second call return a stale
// error and never re-run `docker rm -f` — the row gets deleted and the
// container is orphaned. A second reap while docker is still unreachable must
// (i) actually re-attempt the rm and (ii) keep returning the unknown sentinel.
func TestDockerReapDaemonUnreachableIsReRunnable(t *testing.T) {
	withShortDockerReapTimeout(t, 5*time.Second) // never reached; the fake answers instantly
	var calls int32
	restore := SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte(dockerReapTestEngineID + "\n"), nil
		}
		atomic.AddInt32(&calls, 1)
		return []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"), errors.New("exit status 1")
	})
	defer restore()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "unreachable"}, containerID: "deadbeefcafe0000", engineID: dockerReapTestEngineID}

	first := reapWithin(t, p, 10*time.Second)
	if !errors.Is(first, ErrWorkspaceStateUnknown) {
		t.Fatalf("first daemon-unreachable reap must wrap ErrWorkspaceStateUnknown, got %v", first)
	}
	if p.reaped {
		t.Fatalf("first daemon-unreachable reap must NOT latch, got p.reaped=true")
	}

	second := reapWithin(t, p, 10*time.Second)
	if second == nil {
		t.Fatal("second reap after a transport failure returned nil — the latch skipped the retry, so the row " +
			"would be deleted and the container orphaned")
	}
	if !errors.Is(second, ErrWorkspaceStateUnknown) {
		t.Fatalf("a second reap while docker is still unreachable must keep returning ErrWorkspaceStateUnknown, got %v", second)
	}
	if !TeardownStateUnknown(second) {
		t.Fatalf("the retained row depends on TeardownStateUnknown staying true across retries, got false for: %v", second)
	}
	if n := atomic.LoadInt32(&calls); n < 2 {
		t.Fatalf("a daemon-unreachable reap must actually re-attempt `docker rm -f` on the next call; dockerExec ran %d times, want >= 2", n)
	}
}

// TestDockerReapDaemonUnreachableThenSuccessClears completes the retry
// contract, mirroring TestDockerReapTimeoutThenSuccessClears: once Docker
// access is restored, a reap after a prior transport failure actually reaps
// the container and returns nil so the row is finally deleted rather than
// retained forever — and a further reap stays latched at nil. The call-count
// assertion is what distinguishes a real re-attempt from a latched nil.
func TestDockerReapDaemonUnreachableThenSuccessClears(t *testing.T) {
	withShortDockerReapTimeout(t, 5*time.Second)
	var down atomic.Bool
	var calls int32
	down.Store(true)
	restore := SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte(dockerReapTestEngineID + "\n"), nil
		}
		atomic.AddInt32(&calls, 1)
		if down.Load() {
			return []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"), errors.New("exit status 1")
		}
		return []byte("deadbeefcafe0000\n"), nil
	})
	defer restore()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "recovers"}, containerID: "deadbeefcafe0000", engineID: dockerReapTestEngineID}

	if err := reapWithin(t, p, 10*time.Second); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("first (unreachable) reap must be unknown, got %v", err)
	}
	if p.reaped {
		t.Fatalf("first (unreachable) reap must NOT latch so docker recovery can retry, got p.reaped=true")
	}
	down.Store(false) // docker recovers
	if err := reapWithin(t, p, 10*time.Second); err != nil {
		t.Fatalf("after docker recovers, the retry must reap and return nil so the row is deleted, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n < 2 {
		t.Fatalf("the post-recovery reap must actually re-run `docker rm -f`, not return a stale latched result; "+
			"dockerExec ran %d times, want >= 2 (a latched result would leave the container leaked)", n)
	}
	// Now that a reap COMPLETED, it latches: no further docker command, still nil.
	after := atomic.LoadInt32(&calls)
	if err := reapWithin(t, p, 10*time.Second); err != nil {
		t.Fatalf("a completed reap must latch nil, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != after {
		t.Fatalf("a completed reap must not re-run `docker rm -f` (the Kill-retry collapse the latch exists for); ran %d more times", n-after)
	}
}

// TestDockerReapDaemonUnreachableRetainsStagingDir guards the staging-dir
// invariant the bug report rests on: while the container's fate is unproven,
// the staging directory stays on disk so the cleanup record's StagingDir still
// names a real directory a later reap owns. A transport failure must not clear
// it (only a success or a "No such container" answer does), or a later reap
// would find the binding missing for a container that was never removed.
func TestDockerReapDaemonUnreachableRetainsStagingDir(t *testing.T) {
	staging := t.TempDir()
	restore := SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte(dockerReapTestEngineID + "\n"), nil
		}
		return []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"), errors.New("exit status 1")
	})
	defer restore()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "unreachable"}, containerID: "deadbeefcafe0000", engineID: dockerReapTestEngineID, stagingDir: staging}
	if err := p.reap(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("expected ErrWorkspaceStateUnknown, got %v", err)
	}
	if p.stagingDir == "" {
		t.Fatalf("a daemon-unreachable reap must NOT clear the staging dir — the container may still exist and a later reap still owns it")
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("the staging dir must still be on disk after a daemon-unreachable reap: %v", err)
	}
}

// TestDockerReapRealDaemonAnswerStaysKnown locks the polarity on the OTHER side
// of the fix: a `docker rm -f` the daemon ANSWERED with a real error (it is up
// and reported a problem) is NOT a transport failure and must still latch as a
// KNOWN-state teardown whose row may be deleted. Without this guard the
// transport-failure matcher could over-match a genuine answer and wedge the
// record forever — the inverse of the bug, and exactly the regression the
// "No such container" polarity test (TestDockerReapReportedErrorStaysKnown)
// pins for that one answered error.
func TestDockerReapRealDaemonAnswerStaysKnown(t *testing.T) {
	restore := SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte(dockerReapTestEngineID + "\n"), nil
		}
		return []byte("Error response from daemon: removal of container deadbeefcafe0000 is already in progress"), errors.New("exit status 1")
	})
	defer restore()

	p := &dockerProvisioner{spec: ProvisionSpec{Title: "answered"}, containerID: "deadbeefcafe0000", engineID: dockerReapTestEngineID}
	err := p.reap()
	if err == nil {
		t.Fatal("a failed `docker rm -f` must return an error")
	}
	if errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("a real daemon ANSWER must NOT be classified unknown — only a transport failure is; mapping it to unknown would wedge the row forever: %v", err)
	}
	if TeardownStateUnknown(err) {
		t.Fatalf("TeardownStateUnknown must be false for a real daemon answer so the row may be deleted: %v", err)
	}
	if !p.reaped {
		t.Fatalf("a real daemon answer must latch known-state so the kill-retry collapses (the contract the latch exists for); got p.reaped=false")
	}
}

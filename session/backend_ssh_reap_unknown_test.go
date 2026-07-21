package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"

	"golang.org/x/crypto/ssh"
)

// TestAwaitSSHSessionTimeoutWrapsDeadline pins the classification source. Reap
// can retain a row only if runSession preserves the context deadline in its
// error chain; a human-readable timeout string alone is indistinguishable from
// an answered remote-command failure.
func TestAwaitSSHSessionTimeoutWrapsDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	closer := &countingCloser{}

	_, err := awaitSSHSession(ctx, closer, make(chan sshSessionResult), time.Second, "rm -rf /remote/session")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SSH command timeout must wrap context.DeadlineExceeded, got %v", err)
	}
	if closer.calls != 1 {
		t.Fatalf("timed-out SSH session closed %d times, want 1", closer.calls)
	}
}

// TestSSHReapTimeoutRetriesUntilCleanupCompletes covers both #2198 failures and
// the transport consequence the report omitted. Every timeout must remain an
// unknown-state error, every retained-row poll must execute rm again, and each
// retry must reconnect because the prior attempt closed its SSH client.
func TestSSHReapTimeoutRetriesUntilCleanupCompletes(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "remote-secret"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/remote-secret.1234",
		remotePID:  "4242",
		client:     &ssh.Client{},
	}
	var killCalls, rmCalls, dialCalls, closeCalls int
	p.reapRunCombined = func(_ time.Duration, script string) ([]byte, error) {
		if strings.HasPrefix(script, "kill ") {
			killCalls++
			return nil, nil
		}
		if !strings.HasPrefix(script, "rm -rf ") {
			t.Fatalf("unexpected reap command %q", script)
		}
		rmCalls++
		if rmCalls <= 2 {
			return nil, fmt.Errorf("session stopped at deadline: %w", context.DeadlineExceeded)
		}
		return nil, nil
	}
	p.reapDial = func() error {
		dialCalls++
		p.client = &ssh.Client{}
		return nil
	}
	p.reapCloseClient = func() { closeCalls++ }

	for attempt := 1; attempt <= 2; attempt++ {
		err := p.reap()
		if !errors.Is(err, ErrWorkspaceStateUnknown) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timed-out reap %d must retain the row with both sentinels, got %v", attempt, err)
		}
		if !TeardownStateUnknown(err) {
			t.Fatalf("timed-out reap %d was not classified unknown: %v", attempt, err)
		}
	}
	if err := p.reap(); err != nil {
		t.Fatalf("cleanup retry after SSH recovery must succeed, got %v", err)
	}
	if rmCalls != 3 {
		t.Fatalf("retained-row retries ran remote rm %d times, want 3", rmCalls)
	}
	if dialCalls != 2 {
		t.Fatalf("retries re-dialed SSH %d times, want 2", dialCalls)
	}
	if closeCalls != 3 {
		t.Fatalf("reap attempts closed SSH client %d times, want 3", closeCalls)
	}
	if killCalls != 1 {
		t.Fatalf("reap retries targeted remote PID %d times, want 1 (the PID may be recycled)", killCalls)
	}

	// A completed reap latches. A later Kill call must neither reconnect nor run
	// rm again.
	if err := p.reap(); err != nil {
		t.Fatalf("completed reap did not latch success: %v", err)
	}
	if killCalls != 1 || rmCalls != 3 || dialCalls != 2 || closeCalls != 3 {
		t.Fatalf("completed reap ran again: kill=%d rm=%d dial=%d close=%d", killCalls, rmCalls, dialCalls, closeCalls)
	}
}

// A re-dial failure is itself unknown: the daemon still cannot know whether
// the retained remote directory exists. It must not convert one cleanup timeout
// into a known error on the very next poll merely because the old client closed.
func TestSSHReapReconnectFailureStaysRetryable(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "remote-secret"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/remote-secret.1234",
		client:     &ssh.Client{},
	}
	var rmCalls, dialCalls int
	p.reapRunCombined = func(_ time.Duration, _ string) ([]byte, error) {
		rmCalls++
		if rmCalls == 1 {
			return nil, context.DeadlineExceeded
		}
		return nil, nil
	}
	p.reapDial = func() error {
		dialCalls++
		if dialCalls == 1 {
			return errors.New("remote host temporarily unreachable")
		}
		p.client = &ssh.Client{}
		return nil
	}
	p.reapCloseClient = func() {}

	if err := p.reap(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("first timeout must retain the row, got %v", err)
	}
	if err := p.reap(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("failed cleanup re-dial must retain the row, got %v", err)
	}
	if rmCalls != 1 {
		t.Fatalf("rm ran without a cleanup connection: calls=%d, want 1", rmCalls)
	}
	if err := p.reap(); err != nil {
		t.Fatalf("later re-dial and cleanup should converge, got %v", err)
	}
	if rmCalls != 2 || dialCalls != 2 {
		t.Fatalf("cleanup did not retry after re-dial recovered: rm=%d dial=%d", rmCalls, dialCalls)
	}
}

// Losing the SSH transport before the remote command reports an exit status is
// just as unknown as a timeout: rm may not have started, or it may still have
// completed after the connection disappeared. The record is the retry handle in
// either case, so transport errors must not be latched as answered failures.
func TestSSHReapTransportFailureStaysRetryable(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "remote-secret"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/remote-secret.1234",
		client:     &ssh.Client{},
	}
	var rmCalls, dialCalls int
	p.reapRunCombined = func(_ time.Duration, _ string) ([]byte, error) {
		rmCalls++
		if rmCalls == 1 {
			return nil, io.EOF
		}
		return nil, nil
	}
	p.reapDial = func() error {
		dialCalls++
		p.client = &ssh.Client{}
		return nil
	}
	p.reapCloseClient = func() {}

	if err := p.reap(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("lost SSH transport must retain the row, got %v", err)
	}
	if err := p.reap(); err != nil {
		t.Fatalf("cleanup should retry after re-dialing the lost transport, got %v", err)
	}
	if rmCalls != 2 || dialCalls != 1 {
		t.Fatalf("cleanup did not retry after transport recovery: rm=%d dial=%d", rmCalls, dialCalls)
	}
}

// Preserve the established teardown polarity for a command that answered with
// a normal error: it is a completed outcome, so it latches rather than running
// an identical failing command on every daemon poll.
func TestSSHReapAnsweredErrorLatches(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "answered"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/remote/answered",
		client:     &ssh.Client{},
	}
	var calls int
	p.reapRunCombined = func(_ time.Duration, _ string) ([]byte, error) {
		calls++
		return []byte("permission denied"), &ssh.ExitError{}
	}
	p.reapCloseClient = func() {}

	first := p.reap()
	if first == nil || errors.Is(first, ErrWorkspaceStateUnknown) || TeardownStateUnknown(first) {
		t.Fatalf("answered SSH error must stay known-state, got %v", first)
	}
	second := p.reap()
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("answered error did not latch: first=%v second=%v", first, second)
	}
	if calls != 1 {
		t.Fatalf("latched answered error re-ran remote cleanup %d times, want 1", calls)
	}
}

// A provisioning failure has no Instance record to carry a future reap retry.
// If its best-effort reap is unknown, that orphan risk must reach the caller
// with both causes instead of disappearing into the log.
func TestSSHProvisionFailureSurfacesUnknownReap(t *testing.T) {
	provisionErr := errors.New("git clone failed")
	p := &sshProvisioner{
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/partial.1234",
		client:     &ssh.Client{},
		reapRunCombined: func(time.Duration, string) ([]byte, error) {
			return nil, context.DeadlineExceeded
		},
		reapCloseClient: func() {},
	}

	err := p.reapProvisionFailure(provisionErr)
	if !errors.Is(err, provisionErr) || !errors.Is(err, ErrWorkspaceStateUnknown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("provision and unknown cleanup causes must all survive, got %v", err)
	}
	for _, detail := range []string{p.cfg.Host, p.sessionDir, "inspect it before retrying"} {
		if !strings.Contains(err.Error(), detail) {
			t.Fatalf("provision cleanup error omitted %q: %v", detail, err)
		}
	}
}

type countingCloser struct{ calls int }

func (c *countingCloser) Close() error {
	c.calls++
	return nil
}

func configSSHForReapTest() config.SSHConfig {
	return config.SSHConfig{Host: "cleanup.example.test", User: "remote"}
}

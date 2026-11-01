package daemon

import (
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
)

// acceptErrorInjector wraps the control socket's real net.Listener and returns
// canned Accept errors for the first len(errs) calls, then delegates to the
// underlying listener. It is the test seam that makes the accept loop's error
// classification reachable: startControlServer owns its listener and never
// returns it, so without this wrapper the only Accept error the loop ever
// observes in tests is the clean-shutdown close.
//
// injected closes once the loop has consumed every canned error (so a
// transient-retry test can wait for the loop to be past the errors and about
// to accept normally). closed closes once Close has run on the underlying
// listener (so a fatal-error test can wait for the loop to have unlinked the
// socket). Both are guarded against double-close.
type acceptErrorInjector struct {
	net.Listener
	mu           sync.Mutex
	errs         []error
	calls        int
	injected     chan struct{}
	injectedOnce sync.Once
	closed       chan struct{}
	closeOnce    sync.Once
}

func (l *acceptErrorInjector) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.calls < len(l.errs) {
		err := l.errs[l.calls]
		l.calls++
		if l.calls == len(l.errs) {
			l.injectedOnce.Do(func() { close(l.injected) })
		}
		l.mu.Unlock()
		return nil, err
	}
	l.mu.Unlock()
	return l.Listener.Accept()
}

func (l *acceptErrorInjector) Close() error {
	err := l.Listener.Close()
	l.closeOnce.Do(func() { close(l.closed) })
	return err
}

// installAcceptInjector installs testHookControlListener so the bound control
// socket is wrapped with an injector returning errs, and returns the injector
// for the test to observe. The hook is restored on cleanup. Tests in this
// package run sequentially, so the package-global hook is not subject to
// cross-test interference (same convention as testHookSpawnPingPassed).
func installAcceptInjector(t *testing.T, errs []error) *acceptErrorInjector {
	t.Helper()
	inj := &acceptErrorInjector{
		errs:     errs,
		injected: make(chan struct{}),
		closed:   make(chan struct{}),
	}
	prev := testHookControlListener
	t.Cleanup(func() { testHookControlListener = prev })
	testHookControlListener = func(l net.Listener) net.Listener {
		inj.Listener = l
		return inj
	}
	return inj
}

// requireControlPingAlive asserts the control plane answers a bounded Ping. A
// deadline-bounded RPC is the load-bearing detail: when the accept loop has
// died (the bug), the kernel still completes the dial into the backlog and a
// deadline-less Ping would hang the test forever; a bounded RPC surfaces that
// as a deadline error instead.
func requireControlPingAlive(t *testing.T, msg string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	done := make(chan error, 1)
	go func() { done <- pingDaemonUntil(deadline) }()
	select {
	case err := <-done:
		require.NoError(t, err, msg)
	case <-time.After(5 * time.Second):
		t.Fatalf("bounded Ping did not return within 5s: %s", msg)
	}
}

// requireControlPingRefused asserts the control plane is unreachable and that
// the unreachability is the RECOVERABLE refusal signature (the dial fails
// promptly), not the hang the bug produced (a successful dial into a
// dead-but-still-bound socket).
func requireControlPingRefused(t *testing.T, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	done := make(chan error, 1)
	go func() { done <- pingDaemonUntil(deadline) }()
	select {
	case err := <-done:
		require.Error(t, err, msg)
	case <-time.After(4 * time.Second):
		t.Fatalf("Ping against a closed control socket hung — the recoverable refusal signature was not produced: %s", msg)
	}
}

// TestControlServer_AcceptLoopRetriesTransientAcceptError is the regression for
// the control-plane silent-death bug. Pre-fix, a single transient Accept error
// (EMFILE/ENFILE under fd exhaustion) hit the bare `if err != nil { return }`
// and killed the accept goroutine permanently: the listener stayed bound, the
// socket file stayed on disk, clients dialed into the kernel backlog
// successfully, and then hung forever waiting for an accept that would never
// come — "alive and looping, but unreachable". Post-fix the loop classifies the
// error as transient, retries with backoff, and the control plane keeps
// serving.
func TestControlServer_AcceptLoopRetriesTransientAcceptError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	// Three fd-exhaustion errors model a brief fd-exhaustion spike; the loop
	// must retry through all of them and keep serving. Both EMFILE and ENFILE
	// are the transient class the report names; mixing them exercises both
	// errnos in the classification's `||`.
	inj := installAcceptInjector(t, []error{
		syscall.EMFILE, syscall.ENFILE, syscall.EMFILE,
	})

	closeServer, err := startControlServer(nil, nil, nil, nil)
	require.NoError(t, err, "startControlServer")
	t.Cleanup(func() { _ = closeServer() })

	select {
	case <-inj.injected:
	case <-time.After(3 * time.Second):
		t.Fatal("accept loop never consumed the injected transient errors; it exited on the first EMFILE instead of retrying")
	}

	// The loop must still be alive and serving — the core guarantee of the
	// fix. Pre-fix this Ping (bounded below) would hit its deadline.
	requireControlPingAlive(t, "control plane unreachable after a transient accept error; the loop died instead of retrying")

	// The socket file must survive: the retry path never closes the listener.
	socketPath, err := DaemonSocketPath()
	require.NoError(t, err)
	require.FileExists(t, socketPath, "transient accept error unlinked the socket; only a fatal error or shutdown should")

	// The loop must keep serving, not just catch one last connection. A
	// second Ping confirms the loop is stably accepting.
	requireControlPingAlive(t, "control plane did not keep serving after the transient retry")

	// Cleanup must drain cleanly and not hang the test.
	require.NoError(t, closeServer(), "closeServer errored or hung after a transient-recovered accept loop")
}

// TestControlServer_AcceptLoopFatalErrorClosesListenerAndUnlinksSocket covers
// the genuinely-non-recoverable branch: an Accept error that is neither the
// clean-shutdown close nor a transient EMFILE/ENFILE. Pre-fix the loop
// returned silently and left the socket bound, so clients dialed in and hung.
// Post-fix the loop logs, closes the listener (so net unlinks the socket
// file), and exits — converting the hang into the recoverable refusal
// signature (dial fails with "no such file"), which the existing dial-failure
// reclaim machinery can act on.
func TestControlServer_AcceptLoopFatalErrorClosesListenerAndUnlinksSocket(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	fatalErr := errors.New("simulated permanent listener failure")
	inj := installAcceptInjector(t, []error{fatalErr})

	closeServer, err := startControlServer(nil, nil, nil, nil)
	require.NoError(t, err, "startControlServer")
	t.Cleanup(func() { _ = closeServer() })

	select {
	case <-inj.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("accept loop did not close the listener on a fatal error; it left the socket dialable-but-dead")
	}

	// The socket file must be unlinked: net's unlink-on-close fired when the
	// loop closed the listener. A dialable-but-dead socket is exactly the bug.
	socketPath, err := DaemonSocketPath()
	require.NoError(t, err)
	if _, statErr := os.Stat(socketPath); !os.IsNotExist(statErr) {
		t.Fatalf("fatal accept error left the socket file on disk (stat err = %v); clients would dial it and hang", statErr)
	}

	// New clients must get the recoverable refusal signature (dial fails
	// promptly), not the hang the bug produced.
	requireControlPingRefused(t, "Ping succeeded or hung against a fatally-closed control socket")

	// Cleanup must not report a spurious drain failure (net.ErrClosed from the
	// already-closed listener is the loop's fatal-branch outcome, not a drain
	// error) and must not hang.
	require.NoError(t, closeServer(), "closeServer reported a spurious error or hung after the loop already closed the listener")
}

// TestControlServer_AcceptLoopExitsOnListenerClosed pins the clean-shutdown
// contract: net.ErrClosed is the SOLE non-fatal exit. The loop must return
// immediately on it (it is what the cleanup path produces by closing the
// listener) and must NOT treat it as a transient retry or a fatal close.
func TestControlServer_AcceptLoopExitsOnListenerClosed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	inj := installAcceptInjector(t, []error{net.ErrClosed})

	closeServer, err := startControlServer(nil, nil, nil, nil)
	require.NoError(t, err, "startControlServer")
	t.Cleanup(func() { _ = closeServer() })

	select {
	case <-inj.injected:
	case <-time.After(3 * time.Second):
		t.Fatal("accept loop did not consume the injected net.ErrClosed; it is not classifying the clean-shutdown signal")
	}

	// The clean-exit path must NOT unlink the socket itself: the cleanup owns
	// the unlink (and its #718/#767 race-safe close). The socket is still bound
	// until closeServer runs.
	socketPath, err := DaemonSocketPath()
	require.NoError(t, err)
	require.FileExists(t, socketPath, "net.ErrClosed path unlinked the socket; only the cleanup path should")

	// Cleanup drains cleanly: it closes the listener the loop already exited
	// on, joins the (now-done) serveWG, and returns without hanging.
	require.NoError(t, closeServer(), "closeServer errored or hung after a clean net.ErrClosed exit")
}

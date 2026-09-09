package daemon

import (
	"context"
	"errors"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
)

// bindHandoffRetryTestServer binds srv at the isolated control socket and
// serves connections until the listener is closed. It returns errors instead
// of calling testing.T methods so a hand-off transition can bind the next
// listener from the old server's RPC goroutine.
func bindHandoffRetryTestServer(srv *rpc.Server) (net.Listener, error) {
	socketPath, err := DaemonSocketPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go srv.ServeConn(conn)
		}
	}()
	return listener, nil
}

func closeHandoffRetryTestListener(listener net.Listener) {
	if listener != nil {
		_ = listener.Close()
	}
	if socketPath, err := DaemonSocketPath(); err == nil {
		_ = os.Remove(socketPath)
	}
}

type quiesceThenTransitionControl struct {
	ping         func() error
	transition   func() error
	mutationErr  error
	mutationOnce sync.Once
}

func (c *quiesceThenTransitionControl) Ping(_ PingRequest, _ *PingResponse) error {
	if c.ping != nil {
		return c.ping()
	}
	return nil
}

func (c *quiesceThenTransitionControl) Mutate(_ struct{}, _ *struct{}) error {
	var transitionErr error
	c.mutationOnce.Do(func() {
		if c.transition != nil {
			transitionErr = c.transition()
		}
	})
	if transitionErr != nil {
		return transitionErr
	}
	if c.mutationErr != nil {
		return c.mutationErr
	}
	return errDaemonQuiescing()
}

type acceptingHandoffRetryControl struct{}

func (acceptingHandoffRetryControl) Ping(_ PingRequest, _ *PingResponse) error {
	return nil
}

func (acceptingHandoffRetryControl) Mutate(_ struct{}, _ *struct{}) error {
	return nil
}

func acceptingHandoffRetryServer(t *testing.T) *rpc.Server {
	t.Helper()
	srv := rpc.NewServer()
	if err := srv.RegisterName(controlServiceName, acceptingHandoffRetryControl{}); err != nil {
		t.Fatalf("register accepting Control: %v", err)
	}
	return srv
}

func refuseRealDaemonLaunchForHandoffRetryTest(t *testing.T) {
	t.Helper()
	previous := launchDaemonProcessFn
	launchDaemonProcessFn = func() error {
		return errors.New("test fixture refuses to launch a real daemon")
	}
	t.Cleanup(func() { launchDaemonProcessFn = previous })
}

// TestCallDaemonRetriesPostQuiescingConnectionLoss covers the ordering where
// the client has already observed quiescing, then connects to the old daemon
// once more just as it exits. The accepted connection is closed without an RPC
// response, which net/rpc reports as io.ErrUnexpectedEOF. The candidate binds
// immediately afterward. The client must keep retrying and reach it.
func TestCallDaemonRetriesPostQuiescingConnectionLoss(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	withAutostartTestEnv(t, runtime.GOOS)
	refuseRealDaemonLaunchForHandoffRetryTest(t)

	oldSrv := rpc.NewServer()
	oldControl := &quiesceThenTransitionControl{}
	if err := oldSrv.RegisterName(controlServiceName, oldControl); err != nil {
		t.Fatalf("register old Control: %v", err)
	}
	oldListener, err := bindHandoffRetryTestServer(oldSrv)
	if err != nil {
		t.Fatalf("bind old Control: %v", err)
	}
	t.Cleanup(func() { closeHandoffRetryTestListener(oldListener) })

	var (
		candidateMu       sync.Mutex
		candidateListener net.Listener
	)
	candidateSrv := acceptingHandoffRetryServer(t)
	transitionDone := make(chan error, 1)
	oldControl.transition = func() error {
		closeHandoffRetryTestListener(oldListener)
		socketPath, pathErr := DaemonSocketPath()
		if pathErr != nil {
			return pathErr
		}
		abruptListener, listenErr := net.Listen("unix", socketPath)
		if listenErr != nil {
			return listenErr
		}
		go func() {
			conn, acceptErr := abruptListener.Accept()
			if acceptErr == nil {
				_ = conn.Close() // no response: net/rpc maps the EOF to UnexpectedEOF
			}
			closeHandoffRetryTestListener(abruptListener)
			candidate, candidateErr := bindHandoffRetryTestServer(candidateSrv)
			if candidateErr == nil {
				candidateMu.Lock()
				candidateListener = candidate
				candidateMu.Unlock()
			}
			if acceptErr != nil {
				transitionDone <- acceptErr
				return
			}
			transitionDone <- candidateErr
		}()
		return nil
	}
	t.Cleanup(func() {
		candidateMu.Lock()
		listener := candidateListener
		candidateMu.Unlock()
		closeHandoffRetryTestListener(listener)
	})

	callErr := callDaemon("Mutate", struct{}{}, &struct{}{})
	if transitionErr := <-transitionDone; transitionErr != nil {
		t.Fatalf("complete abrupt-close transition: %v", transitionErr)
	}
	if callErr != nil {
		t.Fatalf("callDaemon returned the post-quiescing connection loss instead of reaching the candidate: %v", callErr)
	}
}

// TestCallDaemonHandoffRetryHonorsAdmissionDeadline makes the upgrade-gate
// probe consume a seven-second internal allowance after the hand-off socket is
// gone. The client owns a five-second admission budget, so it must pass that
// earlier deadline into EnsureDaemon and return near five seconds, never wait
// for the gate's longer internal timeout and then enter launch/readiness work.
func TestCallDaemonHandoffRetryHonorsAdmissionDeadline(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	withAutostartTestEnv(t, runtime.GOOS)
	refuseRealDaemonLaunchForHandoffRetryTest(t)

	oldSrv := rpc.NewServer()
	oldControl := &quiesceThenTransitionControl{}
	if err := oldSrv.RegisterName(controlServiceName, oldControl); err != nil {
		t.Fatalf("register old Control: %v", err)
	}
	oldListener, err := bindHandoffRetryTestServer(oldSrv)
	if err != nil {
		t.Fatalf("bind old Control: %v", err)
	}
	oldControl.transition = func() error {
		closeHandoffRetryTestListener(oldListener)
		return nil
	}
	t.Cleanup(func() { closeHandoffRetryTestListener(oldListener) })

	previousTimeout := upgradeGateTimeout
	upgradeGateTimeout = daemonAdmissionRetryWait + 2*time.Second
	t.Cleanup(func() { upgradeGateTimeout = previousTimeout })
	stubEntrypointGate(t, func(ctx context.Context, _ string, _ bool) error {
		<-ctx.Done()
		return ctx.Err()
	})

	started := time.Now()
	_ = callDaemon("Mutate", struct{}{}, &struct{}{})
	elapsed := time.Since(started)
	maximum := daemonAdmissionRetryWait + time.Second
	if elapsed > maximum {
		t.Fatalf("callDaemon exceeded the %s admission retry budget: took %s (maximum with scheduling allowance %s)",
			daemonAdmissionRetryWait, elapsed, maximum)
	}
}

// TestCallDaemonDetectsGateAfterInitialPostEnsureAbsence covers the hand-off
// ordering where EnsureDaemon's Ping succeeds, but that Ping is the old
// daemon's last response. The first mutation dial therefore sees ENOENT before
// the client can observe a quiescing admission. A live upgrade gate is the
// positive proof that turns only this dead-window absence into a retry.
func TestCallDaemonDetectsGateAfterInitialPostEnsureAbsence(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	withAutostartTestEnv(t, runtime.GOOS)
	refuseRealDaemonLaunchForHandoffRetryTest(t)

	oldSrv := rpc.NewServer()
	oldControl := &quiesceThenTransitionControl{}
	if err := oldSrv.RegisterName(controlServiceName, oldControl); err != nil {
		t.Fatalf("register old Control: %v", err)
	}
	oldListener, err := bindHandoffRetryTestServer(oldSrv)
	if err != nil {
		t.Fatalf("bind old Control: %v", err)
	}
	oldControl.ping = func() error {
		closeHandoffRetryTestListener(oldListener)
		return nil
	}
	t.Cleanup(func() { closeHandoffRetryTestListener(oldListener) })

	candidateSrv := acceptingHandoffRetryServer(t)
	var (
		candidateOnce     sync.Once
		candidateListener net.Listener
		candidateErr      error
	)
	inProgress := &upgradetxn.UpgradeInProgressError{
		TransactionID: "txn-post-ensure",
		ToVersion:     "1.0.286",
		Phase:         upgradetxn.PhaseDaemonStopping,
		Deadline:      time.Now().Add(time.Minute),
	}
	stubEntrypointGate(t, func(context.Context, string, bool) error {
		candidateOnce.Do(func() {
			candidateListener, candidateErr = bindHandoffRetryTestServer(candidateSrv)
		})
		return inProgress
	})
	t.Cleanup(func() { closeHandoffRetryTestListener(candidateListener) })

	callErr := callDaemon("Mutate", struct{}{}, &struct{}{})
	if candidateErr != nil {
		t.Fatalf("bind candidate Control: %v", candidateErr)
	}
	if callErr != nil {
		t.Fatalf("callDaemon surfaced the first post-ensure absence instead of using the live gate and reaching the candidate: %v", callErr)
	}
}

type applicationErrorAfterQuiescingControl struct {
	mu    sync.Mutex
	calls int
}

func (c *applicationErrorAfterQuiescingControl) Ping(_ PingRequest, _ *PingResponse) error {
	return nil
}

func (c *applicationErrorAfterQuiescingControl) Mutate(_ struct{}, _ *struct{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == 1 {
		return errDaemonQuiescing()
	}
	return errors.New("application rejected the mutation")
}

// TestCallDaemonDoesNotRetryApplicationErrorAfterQuiescing is the load-bearing
// negative boundary: net/rpc application errors are rpc.ServerError values,
// not transport connection loss. Even after quiescing was observed, the client
// must return that handler result and must not replay the mutation.
func TestCallDaemonDoesNotRetryApplicationErrorAfterQuiescing(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	withAutostartTestEnv(t, runtime.GOOS)
	refuseRealDaemonLaunchForHandoffRetryTest(t)

	control := &applicationErrorAfterQuiescingControl{}
	srv := rpc.NewServer()
	if err := srv.RegisterName(controlServiceName, control); err != nil {
		t.Fatalf("register Control: %v", err)
	}
	listener, err := bindHandoffRetryTestServer(srv)
	if err != nil {
		t.Fatalf("bind Control: %v", err)
	}
	t.Cleanup(func() { closeHandoffRetryTestListener(listener) })

	callErr := callDaemon("Mutate", struct{}{}, &struct{}{})
	if callErr == nil || callErr.Error() != "application rejected the mutation" {
		t.Fatalf("callDaemon did not return the RPC application error verbatim: %v", callErr)
	}
	control.mu.Lock()
	calls := control.calls
	control.mu.Unlock()
	if calls != 2 {
		t.Fatalf("RPC application error was retried after quiescing: mutation called %d times, want 2", calls)
	}
}

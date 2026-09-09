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

type commitThenDropHandoffControl struct {
	mu          sync.Mutex
	currentConn net.Conn
	mutations   int
}

func (c *commitThenDropHandoffControl) setCurrentConn(conn net.Conn) {
	c.mu.Lock()
	c.currentConn = conn
	c.mu.Unlock()
}

func (c *commitThenDropHandoffControl) Ping(_ PingRequest, _ *PingResponse) error {
	return nil
}

func (c *commitThenDropHandoffControl) Mutate(_ struct{}, _ *struct{}) error {
	c.mu.Lock()
	c.mutations++
	mutation := c.mutations
	conn := c.currentConn
	c.mu.Unlock()

	if mutation == 1 {
		return errDaemonQuiescing()
	}
	if mutation == 2 {
		// The mutation has committed, but its response is lost. From the
		// client's side this is indistinguishable from the old quiescing daemon
		// disappearing before its refusal arrived.
		_ = conn.Close()
	}
	return nil
}

func bindCommitThenDropHandoffServer(control *commitThenDropHandoffControl) (net.Listener, error) {
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
	srv := rpc.NewServer()
	if err := srv.RegisterName(controlServiceName, control); err != nil {
		_ = listener.Close()
		return nil, err
	}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			control.setCurrentConn(conn)
			go srv.ServeConn(conn)
		}
	}()
	return listener, nil
}

// TestCallDaemonDoesNotReplayAmbiguousResponseLossAfterQuiescing covers an
// admitted candidate mutation that commits and then loses its response. The
// preceding quiescing refusal proves a hand-off, but not which daemon accepted
// this later call. Replaying it could execute a non-idempotent mutation twice,
// so every established-connection failure remains final.
func TestCallDaemonDoesNotReplayAmbiguousResponseLossAfterQuiescing(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	withAutostartTestEnv(t, runtime.GOOS)
	refuseRealDaemonLaunchForHandoffRetryTest(t)

	control := &commitThenDropHandoffControl{}
	listener, err := bindCommitThenDropHandoffServer(control)
	if err != nil {
		t.Fatalf("bind commit-then-drop Control: %v", err)
	}
	t.Cleanup(func() { closeHandoffRetryTestListener(listener) })

	callErr := callDaemon("Mutate", struct{}{}, &struct{}{})
	if callErr == nil || !isDaemonHandoffConnectionErr(callErr) {
		t.Fatalf("callDaemon replayed an ambiguously committed mutation instead of returning its response-loss error: %v", callErr)
	}
	control.mu.Lock()
	mutations := control.mutations
	control.mu.Unlock()
	if mutations != 2 {
		t.Fatalf("ambiguous response loss was replayed: mutation called %d times, want 2", mutations)
	}
}

type handoffRetryTimeoutError struct{}

func (handoffRetryTimeoutError) Error() string   { return "synthetic dial timeout" }
func (handoffRetryTimeoutError) Timeout() bool   { return true }
func (handoffRetryTimeoutError) Temporary() bool { return true }

// TestGenericTimeoutDoesNotEstablishHandoffProof pins the timeout-is-unknown
// boundary shared with daemon health probes. A generic timeout can mean a live
// listener with a saturated accept backlog; it cannot by itself authorize the
// retry loop to run the reclaiming EnsureDaemon path.
func TestGenericTimeoutDoesNotEstablishHandoffProof(t *testing.T) {
	if isDaemonHandoffConnectionErr(handoffRetryTimeoutError{}) {
		t.Fatal("generic timeout was classified as hand-off connection loss without quiescing or live-gate proof")
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

// TestCallDaemonRetriesInitialLiveGateRefusal covers a command that starts
// after the old daemon has unlinked its socket and before the candidate binds.
// The first EnsureDaemon sees the live upgrade gate; callDaemon must carry that
// proof into the same bounded retry phase instead of returning it immediately.
func TestCallDaemonRetriesInitialLiveGateRefusal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	withAutostartTestEnv(t, runtime.GOOS)
	refuseRealDaemonLaunchForHandoffRetryTest(t)

	candidateSrv := acceptingHandoffRetryServer(t)
	var (
		candidateOnce     sync.Once
		candidateListener net.Listener
	)
	candidateReady := make(chan error, 1)
	inProgress := &upgradetxn.UpgradeInProgressError{
		TransactionID: "txn-initial-gate",
		ToVersion:     "1.0.286",
		Phase:         upgradetxn.PhaseDaemonStopping,
		Deadline:      time.Now().Add(time.Minute),
	}
	stubEntrypointGate(t, func(context.Context, string, bool) error {
		candidateOnce.Do(func() {
			go func() {
				time.Sleep(150 * time.Millisecond)
				var bindErr error
				candidateListener, bindErr = bindHandoffRetryTestServer(candidateSrv)
				candidateReady <- bindErr
			}()
		})
		return inProgress
	})
	t.Cleanup(func() { closeHandoffRetryTestListener(candidateListener) })

	callErr := callDaemon("Mutate", struct{}{}, &struct{}{})
	if bindErr := <-candidateReady; bindErr != nil {
		t.Fatalf("bind candidate Control: %v", bindErr)
	}
	if callErr != nil {
		t.Fatalf("callDaemon returned the initial live upgrade-gate refusal instead of reaching the candidate: %v", callErr)
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

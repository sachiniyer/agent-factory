package daemon

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/rpc"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// stoppingLifecycle is a daemon's lifecycle stopping with the given intent, as
// drainPlanes leaves it once the Shutdown RPC or a stop signal named it.
func stoppingLifecycle(t *testing.T, restart bool) *daemonLifecycle {
	t.Helper()
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	stop := &stopClock{}
	manager.lifecycle.useStopClock(stop)
	stop.begin()
	stop.declareIntent(restart)
	manager.lifecycle.markStopping()
	return manager.lifecycle
}

// TestShutdownRPCCarriesTheStoppersRestartIntent: a Shutdown whose stopper
// starts the successor makes the stopping daemon refuse as a restart, which a
// client retries into that successor. One without it is a final stop.
func TestShutdownRPCCarriesTheStoppersRestartIntent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
	}{{"a restart", true}, {"a final stop", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
			manager.lifecycle.useStopClock(&stopClock{})
			cs := &controlServer{manager: manager, shutdownCh: make(chan struct{}), shutdownOnce: &sync.Once{}}

			require.NoError(t, cs.Shutdown(ShutdownRequest{Restart: tc.restart}, &ShutdownResponse{}))
			manager.lifecycle.markStopping()

			refusal := manager.lifecycle.mutationAdmissionError()
			require.Equal(t, tc.restart, IsDaemonRestartingErr(refusal), "refusal: %v", refusal)
			require.Equal(t, !tc.restart, IsDaemonStoppingErr(refusal), "refusal: %v", refusal)
		})
	}
}

// recordingShutdownControl is a daemon that records the Shutdown it was sent.
type recordingShutdownControl struct{ got chan ShutdownRequest }

func (recordingShutdownControl) Ping(_ PingRequest, _ *PingResponse) error { return nil }
func (c recordingShutdownControl) Shutdown(req ShutdownRequest, resp *ShutdownResponse) error {
	c.got <- req
	resp.OK = true
	return nil
}

// TestRequestRestartShutdownSaysRestartOnTheWire: the stopper that starts the
// successor (`af upgrade`, `af daemon restart`, the auto-update) puts the
// restart on the Shutdown it sends; a plain RequestShutdown does not.
func TestRequestRestartShutdownSaysRestartOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request func() (ShutdownResult, ShutdownTarget, error)
		restart bool
	}{{"restart", RequestRestartShutdown, true}, {"stop", RequestShutdown, false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			ctrl := recordingShutdownControl{got: make(chan ShutdownRequest, 1)}
			srv := rpc.NewServer()
			require.NoError(t, srv.RegisterName(controlServiceName, ctrl))
			_, cleanup := startFakeControlListener(t, srv)
			t.Cleanup(cleanup)

			result, _, err := tc.request()
			require.NoError(t, err)
			require.Equal(t, ShutdownViaRPC, result)
			require.Equal(t, tc.restart, (<-ctrl.got).Restart)
		})
	}
}

// TestSIGTERMAsksWhetherItIsARestartAfterStampingTheStop: a SIGTERM asks
// systemd whether it is a restart, but never ahead of the stop's stamp, so the
// stop's deadline is measured from the signal's receipt however long the
// question takes (#7436). A SIGINT is a final stop and asks nothing.
func TestSIGTERMAsksWhetherItIsARestartAfterStampingTheStop(t *testing.T) {
	asked := make(chan time.Time, 1)
	previous := stopSignalIsRestartFn
	stopSignalIsRestartFn = func(context.Context) bool {
		asked <- time.Now()
		time.Sleep(100 * time.Millisecond)
		return true
	}
	t.Cleanup(func() { stopSignalIsRestartFn = previous })

	relay := func(sig os.Signal) *stopClock {
		stop := &stopClock{}
		raw, out, done := make(chan os.Signal, 1), make(chan os.Signal, 1), make(chan struct{})
		t.Cleanup(func() { close(done) })
		go relayStopSignals(raw, out, stop, done)
		raw <- sig
		<-out
		return stop
	}

	stop := relay(syscall.SIGTERM)
	require.True(t, stop.restartIntended(), "systemd said restart")
	askedAt := <-asked
	require.False(t, stop.began().After(askedAt),
		"the stop was stamped at %v, after systemd was asked at %v: the question moved the deadline's anchor", stop.began(), askedAt)

	require.False(t, relay(syscall.SIGINT).restartIntended(), "a SIGINT is a final stop")
	select {
	case <-asked:
		t.Fatal("a SIGINT asked systemd whether it is a restart")
	default:
	}
}

// TestStopIntentFailsClosedAtItsBound: an answer that never comes (a wedged
// systemd) settles a final stop at stopIntentBound, and the refusal waiting on
// it is released then, rather than holding the stop.
func TestStopIntentFailsClosedAtItsBound(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stop := &stopClock{}
	stop.settleIntentAsync(func(context.Context) bool {
		<-release
		return true
	})

	answered := make(chan bool, 1)
	start := time.Now()
	go func() { answered <- stop.restartIntended() }()
	select {
	case restart := <-answered:
		require.False(t, restart, "an unanswered question is a final stop")
		require.Less(t, time.Since(start), stopIntentBound+time.Second)
	case <-time.After(stopIntentBound + 2*time.Second):
		t.Fatal("the stop's intent never settled: a wedged question held the stop")
	}
}

// TestRestartRefusalDecodesFromTheControlSocketByItsCode: net/rpc keeps only an
// error's text, so the restart refusal crosses the control socket with its code
// leading its text, and the client reads the code back into the typed refusal.
// The code classifies it, never the wording: a reworded refusal still decodes,
// and the wording alone does not. A wrapped refusal is not one: its handler ran.
func TestRestartRefusalDecodesFromTheControlSocketByItsCode(t *testing.T) {
	decoded := lifecycleRefusalFromRPCError(rpc.ServerError(errDaemonRestarting().Error()))
	require.True(t, IsDaemonRestartingErr(decoded))
	require.Equal(t, "agent-factory daemon is restarting; retry shortly", decoded.Error(),
		"the user reads the message, not its wire code")

	reworded := lifecycleRefusalFromRPCError(rpc.ServerError("[daemon_restarting] the daemon is cycling"))
	require.True(t, IsDaemonRestartingErr(reworded), "a reworded refusal keeps its code")

	for _, text := range []string{
		"agent-factory daemon is restarting; retry shortly",
		"failed to start task session: [daemon_restarting] agent-factory daemon is restarting; retry shortly",
	} {
		require.False(t, IsDaemonRestartingErr(lifecycleRefusalFromRPCError(rpc.ServerError(text))), "%q", text)
	}
}

// TestRestartingRefusalIsRetryable: a restart's refusal is an admission retry,
// on the daemon's side and the client's alike, and HTTP answers it with the
// 503 the other admission refusals get.
func TestRestartingRefusalIsRetryable(t *testing.T) {
	require.True(t, IsDaemonAdmissionRetryable(errDaemonRestarting()))
	require.True(t, IsDaemonAdmissionRetryable(&apiproto.LifecycleRefusal{Code: apiproto.ErrorCodeDaemonRestarting}))
	require.False(t, IsDaemonAdmissionRetryable(errDaemonStopping()))

	recorder := httptest.NewRecorder()
	rpcHandler(func(struct{}, *struct{}) error { return errDaemonRestarting() })(
		recorder, httptest.NewRequest(http.MethodPost, "/v1/tasks/trigger", nil))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

// restartingControl is a daemon whose stopper starts its successor: it refuses
// the mutation as a restart, then closes its socket.
type restartingControl struct {
	once    sync.Once
	close   func()
	refused atomic.Bool
}

func (*restartingControl) Ping(_ PingRequest, _ *PingResponse) error { return nil }
func (c *restartingControl) QuiescingMutation(_ struct{}, _ *struct{}) error {
	c.refused.Store(true)
	c.once.Do(func() { go c.close() })
	return errDaemonRestarting()
}

// TestCallDaemonRidesARestartIntoItsSuccessor: a client refused by a restarting
// daemon keeps retrying past the old socket's close, through a failed first
// ensure, into the successor, as it did before #7440. The restart is proof a
// successor is coming, as a hand-off is.
func TestCallDaemonRidesARestartIntoItsSuccessor(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	withAutostartTestEnv(t, runtime.GOOS)

	old := &restartingControl{}
	oldSrv := rpc.NewServer()
	require.NoError(t, oldSrv.RegisterName(controlServiceName, old))
	oldListener, oldCleanup := startFakeControlListener(t, oldSrv)
	t.Cleanup(oldCleanup)
	old.close = func() {
		time.Sleep(20 * time.Millisecond)
		_ = oldListener.Close()
		if socketPath, err := DaemonSocketPath(); err == nil {
			_ = os.Remove(socketPath)
		}
	}

	// The successor is not up at the first ensure, and is at the next.
	var (
		mu       sync.Mutex
		launches int
		bound    net.Listener
	)
	previous := launchDaemonProcessFn
	launchDaemonProcessFn = func() error {
		mu.Lock()
		defer mu.Unlock()
		launches++
		if launches == 1 {
			return errors.New("the successor is not up yet")
		}
		if bound != nil {
			return nil
		}
		srv := rpc.NewServer()
		if err := srv.RegisterName(controlServiceName, candidateAcceptControl{}); err != nil {
			return err
		}
		ln, err := startFakeControlListenerWithSrv(t, srv)
		bound = ln
		return err
	}
	t.Cleanup(func() {
		launchDaemonProcessFn = previous
		mu.Lock()
		defer mu.Unlock()
		if bound != nil {
			_ = bound.Close()
		}
	})

	require.NoError(t, callDaemon("QuiescingMutation", struct{}{}, &struct{}{}),
		"a restart's refusal must be retried into the successor")
	require.True(t, old.refused.Load(), "the restarting daemon never refused: the scenario did not run")
}

// TestHTTPRefusesAClosedAdmissionAsItsLifecycleState: once the HTTP drain has
// closed admission, its refusal is the lifecycle's own, as on the control
// socket: a final stop as a stop, a restart as a restart (typed, by its code),
// and a hand-off as a hand-off. All three are 503s.
func TestHTTPRefusesAClosedAdmissionAsItsLifecycleState(t *testing.T) {
	refusal := func(t *testing.T, lifecycle *daemonLifecycle) apiproto.EnvelopeError {
		t.Helper()
		drain := newHTTPRequestDrain()
		cs := &controlServer{manager: &Manager{lifecycle: lifecycle}, httpRequests: drain}
		drain.closeAdmission()
		recorder := httptest.NewRecorder()
		cs.trackHTTPRPC(func(http.ResponseWriter, *http.Request) {
			t.Error("a closed admission ran the handler")
		})(recorder, httptest.NewRequest(http.MethodPost, "/v1/tasks/trigger", nil))
		require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		var env struct{ Error apiproto.EnvelopeError }
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &env))
		return env.Error
	}

	t.Run("a final stop", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		got := refusal(t, stoppingLifecycle(t, false))
		require.Equal(t, "agent-factory daemon is shutting down; retry after it restarts", got.Message)
	})
	t.Run("a restart", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		got := refusal(t, stoppingLifecycle(t, true))
		require.Equal(t, "agent-factory daemon is restarting; retry shortly", got.Message)
	})
	t.Run("an upgrade hand-off", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
		manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
		manager.lifecycle.markQuiescing()
		require.True(t, IsDaemonQuiescingErr(errors.New(refusal(t, manager.lifecycle).Message)))
	})
}

// TestHTTPCarriesTheRestartRefusalsCode: an RPC refused as a restart answers
// with the refusal's code in the envelope and its message without the code the
// control socket's text leads with. A wrapped refusal carries no code: its
// handler ran, so it is not a refusal a client may retry.
func TestHTTPCarriesTheRestartRefusalsCode(t *testing.T) {
	answer := func(err error) apiproto.EnvelopeError {
		recorder := httptest.NewRecorder()
		rpcHandler(func(struct{}, *struct{}) error { return err })(
			recorder, httptest.NewRequest(http.MethodPost, "/v1/tasks/trigger", nil))
		var env struct{ Error apiproto.EnvelopeError }
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &env))
		return env.Error
	}

	got := answer(errDaemonRestarting())
	require.Equal(t, apiproto.ErrorCodeDaemonRestarting, got.Code)
	require.Equal(t, "agent-factory daemon is restarting; retry shortly", got.Message)

	wrapped := answer(errors.Join(errors.New("failed to start task session"), errDaemonRestarting()))
	require.Empty(t, wrapped.Code, "a wrapped refusal is not a refusal to retry")
}

// TestARestartShutdownDecodesInAnOlderDaemon: a newer `af daemon restart`, run
// after installing a new binary by hand, sends its Shutdown to the daemon still
// running the old one, whose request has no fields. gob drops the field it does
// not know, so the old daemon still stops (as a final stop, as it always has).
func TestARestartShutdownDecodesInAnOlderDaemon(t *testing.T) {
	var wire bytes.Buffer
	require.NoError(t, gob.NewEncoder(&wire).Encode(ShutdownRequest{Restart: true}))
	var older struct{}
	require.NoError(t, gob.NewDecoder(&wire).Decode(&older))
}

// blockingJobRead is a systemd job read that answers only when its context
// ends, and reports how it ended.
func blockingJobRead() (resolve func(context.Context) bool, ended <-chan error) {
	done := make(chan error, 1)
	return func(ctx context.Context) bool {
		<-ctx.Done()
		done <- ctx.Err()
		return false
	}, done
}

// lengthenStopIntentBound makes the stop's own bound on its job read long
// enough that only the stop ending the read can end it.
func lengthenStopIntentBound(t *testing.T) {
	t.Helper()
	previous := stopIntentBound
	stopIntentBound = 30 * time.Second
	t.Cleanup(func() { stopIntentBound = previous })
}

// TestTheStopEndsItsJobRead: ending the stop's job read cancels it, which kills
// its systemctl, and waits for it, so the read never outlives the daemon
// (#7480).
func TestTheStopEndsItsJobRead(t *testing.T) {
	lengthenStopIntentBound(t)
	stop := &stopClock{}
	resolve, ended := blockingJobRead()
	stop.settleIntentAsync(resolve)

	start := time.Now()
	stop.endIntentRead()
	select {
	case err := <-ended:
		require.ErrorIs(t, err, context.Canceled, "the stop must cancel its read, not wait out its bound")
	default:
		t.Fatal("endIntentRead returned with the read still running")
	}
	require.Less(t, time.Since(start), 5*time.Second)
	require.False(t, stop.restartIntended())
}

// TestAJobReadAskedForAfterTheStopEndedItsReadsNeverStarts: a SIGTERM that
// arrives once the stop has ended its job reads starts no systemctl that could
// outlive the daemon, and is a final stop (#7480).
func TestAJobReadAskedForAfterTheStopEndedItsReadsNeverStarts(t *testing.T) {
	stop := &stopClock{}
	stop.endIntentRead()
	var started atomic.Bool
	stop.settleIntentAsync(func(context.Context) bool { started.Store(true); return true })
	require.False(t, stop.restartIntended(), "a read asked for after the stop ended its reads is a final stop")
	require.False(t, started.Load(), "a read asked for after the stop ended its reads started")
}

// TestRunDaemonEndsItsStopsJobRead: the daemon's stop ends a job read still
// running, before RunDaemon returns, rather than leaving its systemctl to
// outlive the process.
func TestRunDaemonEndsItsStopsJobRead(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	stubContainerSweep(t)
	lengthenStopIntentBound(t)
	managers := captureManager(t)

	runExited := startInProcessDaemon(t, nil)
	manager := <-managers
	waitForDaemonPhase(t, DaemonPhaseReady)
	resolve, ended := blockingJobRead()
	manager.lifecycle.stopClock().settleIntentAsync(resolve)
	requestShutdown(t, runExited)

	select {
	case err := <-ended:
		require.ErrorIs(t, err, context.Canceled)
	default:
		t.Fatal("RunDaemon returned with the stop's job read still running")
	}
}

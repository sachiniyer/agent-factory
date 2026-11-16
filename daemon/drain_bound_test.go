package daemon

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/rpc"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #7393: on 11-15 a stop hung 90s in drainDaemon's handler joins, before the
// bounded join, until systemd SIGKILLed the daemon. Every step before the save
// now shares drainBudget; a handler that never returns is abandoned, named,
// and costs the save — never the stop.

func shortenDrainTotal(t *testing.T, d time.Duration) {
	t.Helper()
	prev := drainBudget
	drainBudget = d
	t.Cleanup(func() { drainBudget = prev })
}

// neverReturns is a plane close that joins a handler which never finishes,
// until the test ends.
func neverReturns(t *testing.T) func() error {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func() error {
		<-release
		return nil
	}
}

// runDrain runs drainDaemon and fails if it outlives limit — on master, a plane
// whose handler never returns holds it forever.
func runDrain(t *testing.T, limit time.Duration, manager *Manager, closeHTTP, closeControl func() error, workers *sync.WaitGroup) (joined bool, elapsed time.Duration) {
	t.Helper()
	httpClosed, controlClosed := false, false
	done := make(chan bool, 1)
	start := time.Now()
	go func() {
		done <- drainDaemon(manager, nil, closeHTTP, closeControl, &httpClosed, &controlClosed, make(chan struct{}), workers)
	}()
	select {
	case joined = <-done:
		return joined, time.Since(start)
	case <-time.After(limit):
		t.Fatalf("drainDaemon was still running after %s: a handler that never returns held the stop", limit)
		return false, 0
	}
}

func TestDrainAbandonsAControlHandlerThatNeverReturns(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	warn := captureWarnings(t)
	shortenDrainTotal(t, 300*time.Millisecond)

	// The handler the control-socket close is joining: dispatched, and never
	// returning, as a method waiting on a wedged git would be.
	defer manager.lifecycle.inFlight().begin("control RPC Daemon.ArchiveSession")()
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	before := listFiles(t, home)

	var workers sync.WaitGroup
	joined, elapsed := runDrain(t, 5*time.Second, manager, nil, neverReturns(t), &workers)

	require.False(t, joined, "an abandoned handler must cost the final save, as a timed-out join does")
	require.Less(t, elapsed, 2*time.Second, "the drain must end near its budget, not wait out the handler")
	got := warn.String()
	require.Contains(t, got, "drain daemon control server did not finish within the 300ms drain budget")
	require.Contains(t, got, "control RPC Daemon.ArchiveSession (running", "the log must name the handler that held the stop")
	require.Equal(t, before, listFiles(t, home), "a drain that abandoned a handler must not write anything")
}

func TestDrainAbandonsAnHTTPHandlerThatNeverReturns(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	warn := captureWarnings(t)
	shortenDrainTotal(t, 300*time.Millisecond)
	defer manager.lifecycle.inFlight().begin("HTTP POST /v1/KillSession")()

	var workers sync.WaitGroup
	joined, elapsed := runDrain(t, 5*time.Second, manager, neverReturns(t), func() error { return nil }, &workers)

	require.False(t, joined)
	require.Less(t, elapsed, 2*time.Second)
	got := warn.String()
	require.Contains(t, got, "drain daemon HTTP server did not finish")
	require.Contains(t, got, "HTTP POST /v1/KillSession (running")
	// The control close still ran: an instant step after a spent budget gets
	// the floor, and is not misreported as abandoned.
	require.NotContains(t, got, "drain daemon control server did not finish", "an instant step was reported as abandoned")
}

// TestDrainSharesOneDeadlineAcrossItsSteps: the steps draw on one deadline, so a
// slow HTTP drain, a wedged control drain and a wedged background writer
// together still end near drainBudget — not at the sum of separate bounds
// (which for the join alone is drainJoinBudget, 3s).
func TestDrainSharesOneDeadlineAcrossItsSteps(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	shortenDrainTotal(t, 400*time.Millisecond)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-release
	}()
	slowHTTP := func() error {
		time.Sleep(250 * time.Millisecond)
		return nil
	}

	joined, elapsed := runDrain(t, 10*time.Second, manager, slowHTTP, neverReturns(t), &workers)
	require.False(t, joined)
	require.Less(t, elapsed, 1500*time.Millisecond, "the drain's steps must share one deadline")
}

// blockingRPC is a control-socket method that runs until released.
type blockingRPC struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingRPC) Wait(_ int, reply *int) error {
	close(b.entered)
	<-b.release
	*reply = 1
	return nil
}

// TestControlCodecNamesEachCallUntilItsResponse: through real net/rpc
// dispatch, a method is on record from its request header until its response,
// so a drain that gives up on it can name it.
func TestControlCodecNamesEachCallUntilItsResponse(t *testing.T) {
	calls := &inflightCalls{}
	svc := &blockingRPC{entered: make(chan struct{}), release: make(chan struct{})}
	server := rpc.NewServer()
	require.NoError(t, server.RegisterName("Blocker", svc))
	serverConn, clientConn := net.Pipe()
	go server.ServeCodec(newGobServerCodec(serverConn, nil, calls))
	client := rpc.NewClient(clientConn)
	t.Cleanup(func() { _ = client.Close() })

	var reply int
	call := client.Go("Blocker.Wait", 7, &reply, nil)
	select {
	case <-svc.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the method was never dispatched")
	}
	require.Contains(t, calls.describe(time.Now()), "control RPC Blocker.Wait (running", "a running method must be named")

	close(svc.release)
	select {
	case <-call.Done:
		require.NoError(t, call.Error)
	case <-time.After(5 * time.Second):
		t.Fatal("the call never completed")
	}
	require.Equal(t, "no handler is recorded as in flight", calls.describe(time.Now()), "an answered call must leave the record")
}

// TestTrackHTTPRPCNamesTheRouteWhileItRuns: the HTTP plane's admitted handlers
// are named by method and path while they run, and released when they return.
func TestTrackHTTPRPCNamesTheRouteWhileItRuns(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	cs := &controlServer{manager: manager, httpRequests: newHTTPRequestDrain()}
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := cs.trackHTTPRPC(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/ArchiveSession", nil))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}
	calls := manager.lifecycle.inFlight()
	require.Contains(t, calls.describe(time.Now()), "HTTP POST /v1/ArchiveSession (running")

	close(release)
	<-done
	require.Equal(t, "no handler is recorded as in flight", calls.describe(time.Now()))
}

func TestInflightCallsDescribeOldestFirstAndCapped(t *testing.T) {
	calls := &inflightCalls{}
	var dones []func()
	for i := 0; i < inflightDescribeMax+2; i++ {
		dones = append(dones, calls.begin(fmt.Sprintf("call-%02d", i)))
		time.Sleep(2 * time.Millisecond) // distinct start times, so the order is the begin order
	}
	got := calls.describe(time.Now())
	require.True(t, strings.HasPrefix(got, "call-00 (running"), "oldest first: %s", got)
	require.Contains(t, got, "and 2 more")
	require.NotContains(t, got, fmt.Sprintf("call-%02d", inflightDescribeMax))

	for _, done := range dones {
		done()
		done() // idempotent
	}
	require.Equal(t, "no handler is recorded as in flight", calls.describe(time.Now()))

	var none *inflightCalls
	none.begin("ignored")()
	require.Equal(t, "no handler is recorded as in flight", none.describe(time.Now()))
}

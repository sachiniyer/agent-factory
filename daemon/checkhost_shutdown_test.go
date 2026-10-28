package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// #5839: an in-flight host probe must never keep the daemon from reaching its
// final checkpoint inside the stopper's SIGKILL grace. Shutdown cancels the
// advisory scope first, which kills the probe's ssh child, and the refresh loop
// is not joined by the drain waitgroup at all.

func hungProbe(t *testing.T, entered chan<- struct{}) {
	t.Helper()
	prev := probeHostsFn
	var once sync.Once
	probeHostsFn = func(ctx context.Context, _ string, _ map[string]config.HostConfig, _ string, _ time.Duration) (session.HostCheck, error) {
		once.Do(func() { close(entered) })
		select {
		case <-ctx.Done(): // what the real probe does when its ssh process group is killed
			return session.HostCheck{Error: "abandoned at shutdown"}, nil
		case <-time.After(30 * time.Second): // session.HostCheckTimeout
			return session.HostCheck{}, nil
		}
	}
	t.Cleanup(func() { probeHostsFn = prev })
}

func TestShutdownCancelsInFlightHostProbesBeforeJoining(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_, err := config.SetGlobalConfigValue("hosts", `{"box1":{"host":"box1.example"}}`)
	require.NoError(t, err)
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())
	server := &controlServer{manager: manager}

	entered := make(chan struct{})
	hungProbe(t, entered)

	// One probe in flight through the RPC (what closeControl waits for) ...
	rpcDone := make(chan struct{})
	go func() {
		defer close(rpcDone)
		var resp CheckHostResponse
		_ = server.CheckHost(CheckHostRequest{Host: "box1"}, &resp)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe never started")
	}

	// ... and the shutdown path that must still reach the checkpoint quickly.
	stopCh := make(chan struct{})
	var workers sync.WaitGroup
	httpClosed, controlClosed := false, false
	drained := make(chan struct{})
	go func() {
		drainDaemon(manager, nil, func() error {
			// The control drain waits for dispatched handlers: with the probe
			// uncancelled this is where the 30s went.
			<-rpcDone
			return nil
		}, &httpClosed, &controlClosed, stopCh, &workers)
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown waited on an in-flight host probe; the final checkpoint would run past the SIGKILL grace")
	}
}

func TestHostRefreshIsNotJoinedByTheDrainWaitgroup(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_, err := config.SetGlobalConfigValue("hosts", `{"box1":{"host":"box1.example"}}`)
	require.NoError(t, err)
	manager, _ := newManagerCapturingWarnings(t, config.DefaultConfig())

	// A probe that ignores cancellation entirely (a wedged ssh that survived the
	// kill): the drain must still not wait for it.
	entered := make(chan struct{})
	release := make(chan struct{})
	prev, prevInterval := probeHostsFn, hostRefreshInterval
	var once sync.Once
	probeHostsFn = func(context.Context, string, map[string]config.HostConfig, string, time.Duration) (session.HostCheck, error) {
		once.Do(func() { close(entered) })
		<-release
		return session.HostCheck{}, nil
	}
	hostRefreshInterval = 10 * time.Millisecond

	stopCh := make(chan struct{})
	loopDone := startHostReadinessRefresh(manager, stopCh)
	// The loop is deliberately not joined by the drain, so it outlives this test:
	// stop it and wait for it to exit BEFORE restoring the seams it was started
	// with (it holds its own copies, but waiting keeps the test self-contained).
	t.Cleanup(func() {
		close(release)
		manager.advisory.Cancel()
		select {
		case <-loopDone:
		case <-time.After(5 * time.Second):
			t.Error("the refresh loop did not exit after its advisory scope was cancelled")
		}
		probeHostsFn, hostRefreshInterval = prev, prevInterval
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh never probed")
	}

	var workers sync.WaitGroup
	httpClosed, controlClosed := false, false
	drained := make(chan struct{})
	go func() {
		drainDaemon(manager, nil, func() error { return nil }, &httpClosed, &controlClosed, stopCh, &workers)
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("the drain joined the host-readiness refresh")
	}
}

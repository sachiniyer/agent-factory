package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestRunDaemonSweepsOrphanSandboxTunnelsBeforeReady pins the tunnel sweep's
// position inside the same pre-admission window as the container sweep
// (#5564): the sweep must run while the readiness barrier is still closed, so
// every marker-stamped transport it finds is provably a previous generation's
// leftover and not this daemon's live tunnel.
func TestRunDaemonSweepsOrphanSandboxTunnelsBeforeReady(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	sweepStarted := make(chan string, 1)
	sweepGate := make(chan struct{})
	var releaseOnce sync.Once
	releaseSweep := func() { releaseOnce.Do(func() { close(sweepGate) }) }
	t.Cleanup(releaseSweep)

	previousSweep := sweepOrphanSandboxTunnels
	sweepOrphanSandboxTunnels = func(homeID string) {
		sweepStarted <- homeID
		<-sweepGate
	}
	t.Cleanup(func() { sweepOrphanSandboxTunnels = previousSweep })

	runDone := make(chan error, 1)
	// runExited closes when the RunDaemon goroutine returns. runDone is
	// single-consumption — the body below receives the error — so the cleanup
	// join needs its own multi-reader signal for "the goroutine finished".
	runExited := make(chan struct{})
	go func() {
		runDone <- RunDaemon(config.DefaultConfig())
		close(runExited)
	}()
	// Every goroutine this test starts is joined before it returns: an
	// unjoined RunDaemon keeps the home's control socket and races the NEXT
	// test's RunDaemon. releaseSweep runs first because a daemon parked in
	// the gated sweep can never answer the Shutdown RPC.
	t.Cleanup(func() {
		releaseSweep()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var resp ShutdownResponse
			_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
			select {
			case <-runExited:
				return
			case <-time.After(50 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Error("in-process daemon did not exit during cleanup")
				return
			}
		}
	})

	var sweptHome string
	select {
	case sweptHome = <-sweepStarted:
	case err := <-runDone:
		t.Fatalf("RunDaemon exited before the orphan tunnel sweep: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunDaemon did not start the orphan tunnel sweep")
	}

	// The seam is handed the CANONICAL home — exactly what startTunnel stamps
	// into AF_SANDBOX_TUNNEL_HOME — never the raw config spelling.
	want, err := canonicalDir(home)
	require.NoError(t, err)
	assert.Equal(t, want, sweptHome, "the sweep must scope to the canonicalized AF home")

	var resp SendPromptResponse
	err = callDaemonNoEnsure("SendPrompt", SendPromptRequest{Title: "created-during-tunnel-sweep", Prompt: "hi"}, &resp)
	require.Error(t, err)
	assert.True(t, IsDaemonStartingErr(err),
		"a state RPC reached the ready manager while the tunnel sweep was still running: %v", err)

	releaseSweep()
	result, _, err := RequestShutdown()
	require.NoError(t, err)
	assert.Equal(t, ShutdownViaRPC, result)
	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunDaemon did not exit after shutdown")
	}
}

// TestRunDaemonStopsSandboxTunnelsOnExit pins the deferred shutdown stop
// (#5564): a clean daemon exit must stop the sandbox tunnels it spawned, the
// same way it stops its other daemon-owned children.
func TestRunDaemonStopsSandboxTunnelsOnExit(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	stopped := make(chan struct{}, 1)
	previousStop := stopSandboxTunnels
	stopSandboxTunnels = func() { stopped <- struct{}{} }
	t.Cleanup(func() { stopSandboxTunnels = previousStop })

	// Keep the real sweeps cheap and side-effect free: the container sweep is
	// stubbed, the tunnel sweep runs for real against a home nothing is
	// stamped for.
	previousSweep := sweepOrphanContainers
	sweepOrphanContainers = func(string, map[string]bool) session.OrphanSweepResult {
		return session.OrphanSweepResult{}
	}
	t.Cleanup(func() { sweepOrphanContainers = previousSweep })

	runDone := make(chan error, 1)
	runExited := make(chan struct{})
	go func() {
		runDone <- RunDaemon(config.DefaultConfig())
		close(runExited)
	}()
	// Same join rule as the sweep test above: the daemon goroutine must not
	// outlive this test. runExited — not runDone — is the join signal: the
	// body below receives the error, so the channel is already drained.
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for {
			var resp ShutdownResponse
			_ = callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp)
			select {
			case <-runExited:
				return
			case <-time.After(50 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Error("in-process daemon did not exit during cleanup")
				return
			}
		}
	})

	// The PID file is written before the control socket binds (#5188), so the
	// socket — not the file — is what readiness waits on.
	waitForReady(t, "daemon serving the control socket", func() bool {
		return pingDaemon() == nil
	})
	result, _, err := RequestShutdown()
	require.NoError(t, err)
	assert.Equal(t, ShutdownViaRPC, result)

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the deferred sandbox-tunnel stop did not run on daemon exit")
	}
	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunDaemon did not exit after shutdown")
	}
}

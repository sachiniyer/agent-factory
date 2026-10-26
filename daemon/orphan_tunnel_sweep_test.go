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
	go func() { runDone <- RunDaemon(config.DefaultConfig()) }()

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
	go func() { runDone <- RunDaemon(config.DefaultConfig()) }()

	require.NoError(t, pingDaemonUntil(time.Now().Add(10*time.Second)))
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

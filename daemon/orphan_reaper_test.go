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

// TestRunDaemonReachesReadyWhileOrphanSweepIsBlocked pins #5815: the Docker
// orphan pass is housekeeping, so a pass that never returns (an unavailable
// engine: 30s per Docker call) must not hold the daemon in "starting (restoring
// sessions)". The pass is armed at startup and runs after readiness on a tracked
// worker; createSweepMu, not the readiness barrier, keeps a concurrent create out
// of its protected-slug window (#2632).
func TestRunDaemonReachesReadyWhileOrphanSweepIsBlocked(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	sweepStarted := make(chan struct{})
	sweepGate := make(chan struct{})
	var releaseOnce sync.Once
	releaseSweep := func() { releaseOnce.Do(func() { close(sweepGate) }) }
	t.Cleanup(releaseSweep)

	var startedOnce sync.Once
	previousSweep := sweepOrphanContainers
	sweepOrphanContainers = func(string, map[string]bool) session.OrphanSweepResult {
		startedOnce.Do(func() { close(sweepStarted) })
		<-sweepGate
		return session.OrphanSweepResult{}
	}
	t.Cleanup(func() { sweepOrphanContainers = previousSweep })

	runDone := make(chan error, 1)
	go func() { runDone <- RunDaemon(config.DefaultConfig()) }()

	// Ready with the sweep still blocked (or not yet started).
	waitForDaemonPhase(t, DaemonPhaseReady)

	select {
	case <-sweepStarted:
	case err := <-runDone:
		t.Fatalf("RunDaemon exited before the orphan sweep: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunDaemon did not run the armed orphan sweep after readiness")
	}
	assert.Equal(t, DaemonPhaseReady, Health().Phase, "the daemon stays ready while the sweep is blocked")

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

// TestDockerReapProtectedSlugs: the orphan sweep's protected set covers live
// instances AND still-provisioning pending creates (the #2549 window where a
// container exists but no Instance does yet), each as its af.session title slug —
// so the sweep can never reap a container whose session is alive or mid-create.
func TestDockerReapProtectedSlugs(t *testing.T) {
	repoID := "repo1"
	m := &Manager{
		instances:      make(map[string]*session.Instance),
		pendingCreates: make(map[string]session.InstanceData),
	}
	m.instances[daemonInstanceKey(repoID, "live one")] = &session.Instance{Title: "live one"}
	m.pendingCreates[daemonInstanceKey(repoID, "mid create")] = session.InstanceData{Title: "mid create"}

	got := m.dockerReapProtectedSlugs()

	assert.True(t, got[session.Slugify("live one")], "a live instance's slug must be protected")
	assert.True(t, got[session.Slugify("mid create")], "a mid-create pending session's slug must be protected (#2549)")
	assert.Len(t, got, 2)
}

// TestDockerReapProtectedSlugsSettledPendingNotDoubleCounted: a pending row that
// has already settled into a real instance is not processed twice.
func TestDockerReapProtectedSlugsSettledPendingNotDoubleCounted(t *testing.T) {
	key := daemonInstanceKey("repo1", "worker")
	m := &Manager{
		instances:      map[string]*session.Instance{key: {Title: "worker"}},
		pendingCreates: map[string]session.InstanceData{key: {Title: "worker"}},
	}
	got := m.dockerReapProtectedSlugs()
	assert.True(t, got[session.Slugify("worker")])
	assert.Len(t, got, 1, "a settled pending create must not be counted twice")
}

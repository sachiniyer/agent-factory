package daemon

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// stagingSweepRecorder captures every call to the stubbed
// sweepOrphanedStagingDirs — same shape as sweepRecorder for the container
// sweep.
type stagingSweepRecorder struct {
	mu    sync.Mutex
	calls []stagingSweepCall
}

type stagingSweepCall struct {
	homeID       string
	claimedNames map[string]bool
	slugs        map[string]bool
}

func (r *stagingSweepRecorder) record(homeID string, claimedNames, slugs map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, stagingSweepCall{homeID: homeID, claimedNames: claimedNames, slugs: slugs})
}

func (r *stagingSweepRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *stagingSweepRecorder) lastCall() (stagingSweepCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return stagingSweepCall{}, false
	}
	return r.calls[len(r.calls)-1], true
}

// TestDeferredSweep_StagingDirsRunsInSamePass pins the #7590 wiring: the
// orphaned-staging-dir sweep runs inside the same deferred pass as the
// container sweep, with the staging-dir keep set built in the same breath as
// the protected slugs — a record-claimed <slug>-<hex> name reaches the sweep,
// a bare instance contributes no name, and every live/pending slug is
// protected. It also pins the #7616 R3 wiring: the sweep runs AFTER the
// createSweepMu admission barrier is released — its candidacy proof is each
// candidate's own generation lock, not the snapshot the barrier guards, so
// its filesystem reads and removals never hold create admission.
func TestDeferredSweep_StagingDirsRunsInSamePass(t *testing.T) {
	stubConfigDirForReap(t, "/test/home")
	stubSweepOrphanContainers(t)
	// The deferred pass also runs the af-setup payload sweep — stub it so the
	// test never touches a real <home>/af-setup on the box it runs on.
	prevPayload := sweepStaleSetupPayloads
	sweepStaleSetupPayloads = func(map[string]bool) {}
	t.Cleanup(func() { sweepStaleSetupPayloads = prevPayload })

	m := newBareManagerForSweep()
	m.deferredOrphanSweepArmed = true

	rec := &stagingSweepRecorder{}
	var sawUnbarriered atomic.Bool
	prev := sweepOrphanedStagingDirs
	sweepOrphanedStagingDirs = func(homeID string, claimedNames, slugs map[string]bool) session.StagingSweepResult {
		// TryLock only succeeds when the barrier is NOT held — the sweep
		// must run after it is released.
		if m.createSweepMu.TryLock() {
			sawUnbarriered.Store(true)
			m.createSweepMu.Unlock()
		}
		rec.record(homeID, claimedNames, slugs)
		return session.StagingSweepResult{}
	}
	t.Cleanup(func() { sweepOrphanedStagingDirs = prev })

	// A record-bearing docker instance claims its staging dir by name; the
	// claim is what keeps the sweep from reclaiming a tombstoned row's dir.
	claimed, err := session.FromInstanceData(session.InstanceData{
		ID:          "id-claimed",
		Title:       "claimed-sess",
		BackendType: "docker",
		Liveness:    session.LiveRunning,
		RuntimeCleanup: &session.RuntimeCleanupData{Docker: &session.DockerRuntimeCleanupData{
			ContainerID: "cid", EngineID: "eng", StagingDir: "claimed-sess-1a2b3c4d",
		}},
	})
	require.NoError(t, err)
	m.instances[daemonInstanceKey("repo1", "claimed-sess")] = claimed
	// A bare (local) instance protects its slug but claims no staging name.
	m.instances[daemonInstanceKey("repo1", "bare-sess")] = &session.Instance{Title: "bare-sess"}
	// A mid-create pending session: no record yet, protected by slug alone.
	m.pendingCreates[daemonInstanceKey("repo1", "pending-sess")] = session.InstanceData{ID: "id-pending", Title: "pending-sess"}

	runDeferredOrphanSweepIfReady(m)

	assert.True(t, sawUnbarriered.Load(), "the staging sweep must run with createSweepMu already released")
	require.Equal(t, 1, rec.count(), "the staging sweep must run once inside the deferred pass")
	call, ok := rec.lastCall()
	require.True(t, ok)
	assert.Equal(t, "/test/home", call.homeID)
	assert.True(t, call.claimedNames["claimed-sess-1a2b3c4d"],
		"the record-claimed staging name must reach the keep set")
	assert.Len(t, call.claimedNames, 1, "a backend with no claim contributes no name")
	for _, title := range []string{"claimed-sess", "bare-sess", "pending-sess"} {
		assert.True(t, call.slugs[session.Slugify(title)], "slug %q must be protected", title)
	}
}

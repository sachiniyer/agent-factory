package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// seedLiveSandboxRow writes a NON-tombstoned sandbox row whose RuntimeCleanup
// handle IS persisted — the on-disk shape a current-binary LIVE sandbox
// session produces after the KilledWithoutLiveRuntime orphan-guard fix, so a
// subsequent daemon restart can rebuild the off-box teardown handle and a
// later kill reaps for real. Mirrors seedInertSandboxTombstone's
// "write directly to instances.json then refreshLocked reloads it" pattern,
// but for the row that was LIVE before the restart instead of a kill tombstone.
func seedLiveSandboxRow(t *testing.T, repoID, id, title, repoPath string, cleanup *session.RuntimeCleanupData) {
	t.Helper()
	row := session.InstanceData{
		ID:             id,
		Title:          title,
		Path:           repoPath,
		BackendType:    "docker",
		Liveness:       session.LiveRunning,
		RuntimeCleanup: cleanup,
	}
	encoded, err := json.Marshal([]session.InstanceData{row})
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, encoded))
}

// liveDockerCleanupHandle returns the persisted RuntimeCleanup shape a current-
// binary docker session writes on its ordinary snapshot after the orphan-guard
// fix: the engine id + container id a restart needs to rebuild the docker reap.
func liveDockerCleanupHandle(title string) *session.RuntimeCleanupData {
	return &session.RuntimeCleanupData{Docker: &session.DockerRuntimeCleanupData{
		ContainerID: "sha256:" + title,
		EngineID:    "engine-" + title,
	}}
}

// TestRefreshLocked_RebuiltLiveDockerHandleReapsForReal is the daemon-level E2E
// for the orphan-guard fix: a sandbox row whose off-box runtime SURVIVED the
// daemon restart (RuntimeCleanup is persisted on the ordinary disk record)
// reloads via the production refresh path with a REAL rebuilt teardown handle
// — not the nil an inert-only reload produced pre-fix. With the rebuilt
// handle, a later kill reaps the still-live container for real instead of
// staging an unearned "nothing owed" marker from the inert backend's nil
// runtimeTeardown alone and running a no-op that orphans it.
//
// The kill's reap is mocked through session.SetDockerExecForTest (the same seam
// session.TestRestoredDockerCleanupReapsMatchingEngine uses), proving the
// rebuilt teardown IS the docker reap and runs end-to-end via inst.Kill.
//
// PRE-FIX this test fails: the live snapshot scrubbed RuntimeCleanup, the
// reload had no handle to rebuild, runtimeTeardown came back nil, and Kill
// returned nil for a nil teardown WITHOUT EVER CALLING docker — the silent
// orphan the bug is about.
func TestRefreshLocked_RebuiltLiveDockerHandleReapsForReal(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	seedLiveSandboxRow(t, repoID, "live-restart-id", "live-restart", repoPath, liveDockerCleanupHandle("live-restart"))

	// Simulate the daemon restart: refreshLocked re-reads the on-disk record
	// via the production fromInstanceDataForRefresh path and materializes the
	// row in m.instances.
	require.NoError(t, manager.refreshLocked(), "refresh should materialize the seeded live row")
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repoID, "live-restart")]
	manager.mu.Unlock()
	require.NotNil(t, inst, "the seeded live sandbox row must materialize on reload")

	// End-to-end: kill the reloaded row and confirm the rebuilt teardown runs
	// the REAL docker reap (mocked) instead of the no-op inert path pre-fix
	// returned for a nil teardown.
	var reapCalls [][]string
	restoreExec := session.SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		reapCalls = append(reapCalls, append([]string(nil), args...))
		// Two calls expected: the engine-identity probe, then the rm itself.
		if len(args) > 0 && args[0] == "info" {
			return []byte("engine-live-restart\n"), nil // matches the recorded EngineID
		}
		return []byte("sha256:live-restart\n"), nil // rm success
	})
	t.Cleanup(restoreExec)

	killErr := inst.Kill()
	assert.NoError(t, killErr,
		"a reloaded live sandbox row's Kill must run the rebuilt docker reap and return its (here mocked-success) outcome — "+
			"a no-op nil would mean the kill took the inert pre-fix path instead of reaping for real")
	// The reap actually ran: the rebuilt teardown IS the docker reap. Pre-fix
	// this slice was empty because runtimeTeardown was nil and Kill returned
	// nil without ever reaching the reap.
	if !assert.Len(t, reapCalls, 2, "the rebuilt docker reap must have run its two-step identity probe + rm") {
		return
	}
	if len(reapCalls[0]) == 0 || reapCalls[0][0] != "info" {
		t.Fatalf("first reap call was not the engine-identity probe: %v", reapCalls[0])
	}
	if len(reapCalls[1]) == 0 || reapCalls[1][0] != "rm" {
		t.Fatalf("second reap call was not the container rm: %v", reapCalls[1])
	}
}

// TestRefreshLocked_LiveDockerReapUncertaintyRetainsRecord pins the safe-failure
// side of the orphan guard at the daemon level: when the rebuilt docker reap
// LOSES certainty mid-teardown (the engine identity changed under it), Kill
// returns ErrWorkspaceStateUnknown — the unknown-state sentinel
// deleteSessionRecord refuses to delete on — so the row is RETAINED, not
// orphaned. Pre-fix the kill returned nil (no-op reap) and the record deleted,
// leaving the container alive with no remaining pointer in af.
func TestRefreshLocked_LiveDockerReapUncertaintyRetainsRecord(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	seedLiveSandboxRow(t, repoID, "live-uncertain-id", "live-uncertain", repoPath, liveDockerCleanupHandle("live-uncertain"))

	require.NoError(t, manager.refreshLocked(), "refresh should materialize the seeded live row")
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repoID, "live-uncertain")]
	manager.mu.Unlock()
	require.NotNil(t, inst, "the seeded live sandbox row must materialize on reload")

	restoreExec := session.SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			// A different engine identity than the one recorded → the reap
			// returns ErrWorkspaceStateUnknown, the unknown-state sentinel
			// deleteSessionRecord refuses to delete on.
			return []byte("engine-someone-else\n"), nil
		}
		return nil, assert.AnError
	})
	t.Cleanup(restoreExec)

	killErr := inst.Kill()
	assert.True(t, errors.Is(killErr, session.ErrWorkspaceStateUnknown),
		"a reloaded live sandbox row whose reap cannot prove the engine must return ErrWorkspaceStateUnknown so the record is retained, got: %v", killErr)
}

// TestRefreshLockedLegacyLiveRowStillAdmitsInertAtKillMarker pins the
// continuation of the f58ca97f wedge fix the orphan guard must NOT regress: a
// legacy LIVE sandbox row whose ordinary snapshot did NOT yet persist
// RuntimeCleanup (the pre-fix ForStorage scrubbed it) still reloads as inert,
// and a kill committed against it stages the nothing-owed marker from that
// inert shape and the pending record delete can still proceed. The bug report
// names this as the rollforward boundary — the orphan guard solves the bug
// for rows persisted after the fix; legacy rows continue through the marker
// path until they cycle out.
func TestRefreshLockedLegacyLiveRowStillAdmitsInertAtKillMarker(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// Seed a legacy row with NO RuntimeCleanup (pre-fix ordinary-snapshot shape).
	seedLiveSandboxRow(t, repoID, "legacy-live-id", "legacy-live", repoPath, nil)

	require.NoError(t, manager.refreshLocked(), "refresh should materialize the seeded legacy row")
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repoID, "legacy-live")]
	manager.mu.Unlock()
	require.NotNil(t, inst, "the legacy row must materialize on reload")

	// Legacy row has nothing to rebuild from: RuntimeCleanup is nil, the
	// orphan-guard's rebuild path does not fire, and the row reloaded INERT —
	// exactly as pre-fix. The wedge-fix marker path still admits this shape,
	// so a kill committed against it completes the no-op reap and the pending
	// record delete can still proceed; no regression for rows the fix did not
	// reach. The orphan guard narrows the marker to earned evidence but does
	// not synthesize a handle that was never stored.
	inst.MarkUserKilled()
	stored := inst.ToInstanceData().ForStorage()
	assert.True(t, stored.KilledWithoutLiveRuntime,
		"a legacy always-inert reload must still stage the nothing-owed marker at kill time; the f58ca97f wedge-fix admission is preserved")
	assert.Nil(t, stored.RuntimeCleanup,
		"a legacy always-inert reload must not synthesize a cleanup handle; the orphan guard does not invent a reap target")
	if err := inst.Kill(); err != nil {
		t.Fatalf("a legacy always-inert reload's Kill must complete the no-op reap (the marker admits nothing owed): %v", err)
	}
}

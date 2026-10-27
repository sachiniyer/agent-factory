package daemon

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// seedInertSandboxTombstone writes a single sandbox-type row to disk shaped
// exactly like the tombstone persistKillTombstone commits when a kill runs
// against an inert-loaded sandbox session: UserKilled + a nil RuntimeCleanup
// (the inert backend owed nothing to reap). withMarker controls whether the
// KilledWithoutLiveRuntime admission marker is set — the durable token the fix
// reads at load time to admit that "nothing owed" shape.
func seedInertSandboxTombstone(t *testing.T, repoID, id, title, repoPath string, withMarker bool) {
	t.Helper()
	row := session.InstanceData{
		ID:          id,
		Title:       title,
		Path:        repoPath,
		BackendType: "docker",
		UserKilled:  true,
		Liveness:    session.LiveLost,
	}
	if withMarker {
		row.KilledWithoutLiveRuntime = true
	}
	seeded, err := json.Marshal([]session.InstanceData{row})
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, seeded))
}

// TestFinishUserKill_ConvergesOnInertAtKillTombstoneAcrossRestart is the daemon-
// level E2E for the inert-at-kill fix (G12). It reproduces the exact wedge window
// the bug report names: a kill committed against an inert-loaded sandbox row
// persists a handle-less tombstone; if the record delete then fails (or the
// daemon dies before it), the surviving tombstone is reloaded on the next daemon
// start. The fix writes a KilledWithoutLiveRuntime marker at commit time so the
// reload admits the "nothing owed" shape (nil runtimeTeardown) and the poll's
// finishUserKill retry converges — instead of installing a permanent fail-closed
// teardown that loops forever on ErrWorkspaceStateUnknown.
//
// Asserted through the production refresh path (refreshLocked →
// fromInstanceDataForRefresh) and the poll's finish-kill pass (RefreshStatuses →
// finishUserKill), exactly as a daemon restart + next poll would. The observable
// is the one that matters: the record is deleted (converges), not retained
// forever.
func TestFinishUserKill_ConvergesOnInertAtKillTombstoneAcrossRestart(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	seedInertSandboxTombstone(t, repoID, "inert-kill-id", "inert-kill", repoPath, true)

	// Simulate the daemon restart: refreshLocked reloads the on-disk record via
	// fromInstanceDataForRefresh (the production refresh path) and materializes
	// the tombstoned row in m.instances.
	require.NoError(t, manager.refreshLocked(), "refresh should materialize the seeded tombstone")
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repoID, "inert-kill")]
	manager.mu.Unlock()
	require.NotNil(t, inst, "the seeded inert-at-kill tombstone must materialize on reload")

	// The marker's load-time admission, asserted through the exported Kill the
	// poll's finishUserKill drives: a nil teardown ⇒ no-op success (nil error),
	// not the permanent ErrWorkspaceStateUnknown the pre-fix code returned on a
	// handle-less tombstone. Calling Kill directly here (before RefreshStatuses)
	// is safe: the instance is registered but no op-lock is held and no kill is
	// in flight yet.
	killErr := inst.Kill()
	assert.NoError(t, killErr,
		"inert-at-kill tombstone Kill must complete the no-op reap (nil teardown admitted by the marker), "+
			"got %v — a permanent fail-closed teardown means the record can never be deleted", killErr)

	// Drive the poll's finish-kill pass. With a nil teardown, Kill ran the no-op
	// success path above, so deleteSessionRecord proceeds and the record is
	// deleted — the convergence the wedge defeated. (Re-run Kill through the
	// manager path to exercise deleteSessionRecord end-to-end.)
	manager.RefreshStatuses()

	if rec := recordFor(t, repoID, "inert-kill"); rec != nil {
		t.Fatalf("inert-at-kill tombstone must converge (record deleted) after restart, still present: %+v", rec)
	}
	manager.mu.Lock()
	_, tracked := manager.instances[daemonInstanceKey(repoID, "inert-kill")]
	manager.mu.Unlock()
	assert.False(t, tracked, "finished kill must drop the instance from the manager's map")
}

// TestFinishUserKill_WedgesOnHandlelessTombstoneWithoutMarker is the pre-fix
// regression shape: the SAME handle-less sandbox tombstone WITHOUT the
// KilledWithoutLiveRuntime marker — the on-disk shape a binary predating this
// fix writes — must still fail closed. finishUserKill's Kill runs the permanent
// unavailableRuntimeCleanup and returns ErrWorkspaceStateUnknown, so the record
// is retained (the retry does NOT converge). This proves the fix admits ONLY
// the current-binary inert-at-kill shape the marker vouches for.
func TestFinishUserKill_WedgesOnHandlelessTombstoneWithoutMarker(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	seedInertSandboxTombstone(t, repoID, "legacy-inert-id", "legacy-inert", repoPath, false)

	require.NoError(t, manager.refreshLocked(), "refresh should materialize the seeded legacy tombstone")
	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repoID, "legacy-inert")]
	manager.mu.Unlock()
	require.NotNil(t, inst, "the legacy tombstone must materialize on reload")

	// No marker ⇒ fail closed: a permanent teardown that returns
	// ErrWorkspaceStateUnknown, the exact unhealable error the bug report names.
	killErr := inst.Kill()
	assert.True(t, errors.Is(killErr, session.ErrWorkspaceStateUnknown),
		"legacy handle-less tombstone without the marker must fail closed with ErrWorkspaceStateUnknown, got: %v", killErr)

	// finishUserKill must NOT delete the record: the retry does not converge —
	// the deterministic error means a later attempt is the same attempt.
	manager.RefreshStatuses()
	if rec := recordFor(t, repoID, "legacy-inert"); rec == nil {
		t.Fatal("legacy handle-less tombstone without the marker must NOT converge — the record must be retained (fail closed), but it was deleted")
	}
}

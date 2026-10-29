package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// This file pins the downgrade regression for a preserved unpushed archived
// workspace (#5867): a binary rolled back to before #5886 (the release that
// began publishing the preserved row's runtime-cleanup handle) clears
// RuntimeCleanup on every archived checkpoint, so a single checkpoint on the
// rolled-back binary erases the only identity that names the preserved
// directory af made. The naive fence (projecting RuntimeCleanupStateUnknown,
// as the ordinary-sandbox branch does) cannot protect the row: the previous
// release's ForStorage case lv == LiveArchived runs first and clears both the
// handle and the flag unconditionally, and a fence that survives the rolled-back
// checkpoint would have to project the row's liveness away from LiveArchived —
// a change whose blast radius across the many raw disk readers of liveness
// (RecordedLiveness/IsArchivedData callers across api, daemon, commands, app)
// is out of scope for this fix.
//
// The gap is contained on the load side instead: FromInstanceData treats a
// preserved archive whose handle is gone as a row that still owes a reap and
// fail-closes it, so a kill returns ErrWorkspaceStateUnknown and the destructive
// choke point retains the only record pointing at the preserved directory
// rather than laundering the missing identity into a no-op delete that orphans
// it — the exact #5867 orphan the handle publication was meant to prevent. The
// durable archived_workspace field survives the rolled-back checkpoint, so an
// operator can still reap the directory by hand or restore the session. These
// tests would FAIL on the pre-fix tree (the erased row loaded inert with a nil
// teardown, kill ran the no-op success path, and the record was deleted without
// reaping the directory) and PASS now.

// simulatePreviousReleaseArchivedCheckpoint models what a binary rolled back to
// before #5886 writes when it shuts down on a preserved archived row: it loads
// the row (a JSON round-trip, which preserves every field the previous release's
// struct already knows — b9a3589e added no new fields), and its ForStorage
// case lv == LiveArchived clears RuntimeCleanup, RuntimeCleanupStateUnknown and
// KilledWithoutLiveRuntime unconditionally. The result is the on-disk row a roll
// forward to HEAD must not orphan.
func simulatePreviousReleaseArchivedCheckpoint(t *testing.T, stored InstanceData) InstanceData {
	t.Helper()
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	var reloaded InstanceData
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	// The pre-#5886 ForStorage LiveArchived branch, verbatim in effect:
	//   d.RuntimeCleanupStateUnknown = false
	//   d.RuntimeCleanup = nil
	//   d.KilledWithoutLiveRuntime = false
	reloaded.RuntimeCleanupStateUnknown = false
	reloaded.RuntimeCleanup = nil
	reloaded.KilledWithoutLiveRuntime = false
	return reloaded
}

// TestPreservedArchiveHandleErasedByDowngradeFailsClosed is the core regression:
// HEAD publishes the preserved row's reap handle, a rolled-back binary erases it
// on its next archived checkpoint, and rolling forward to HEAD must not let a
// kill delete the only record pointing at the preserved directory. The row loads
// fail-closed: a sentinel teardown makes Kill return ErrWorkspaceStateUnknown so
// the record is retained, the nothing-owed marker is refused, and the durable
// archived_workspace field survives so the operator can reap by hand.
func TestPreservedArchiveHandleErasedByDowngradeFailsClosed(t *testing.T) {
	inst, _, _ := reattachFixtureStops(t, "", false)
	inst.ID, inst.Path, inst.started, inst.liveness = "s1", t.TempDir(), true, LiveReady
	attachStoppableAgent(t, inst, false)
	loc, err := inst.ArchiveSandboxPreserving()
	require.NoError(t, err)
	inst.mu.Lock()
	inst.liveness = LiveArchived
	inst.mu.Unlock()

	// HEAD publish: the handle is written for a preserved workspace (#5867).
	stored := inst.ToInstanceData().ForStorage()
	require.NotNil(t, stored.RuntimeCleanup, "HEAD publishes the preserved row's reap handle")
	require.NotNil(t, stored.RuntimeCleanup.Sandbox)
	require.Equal(t, loc.Dir, stored.RuntimeCleanup.Sandbox.SessionDir)
	require.False(t, stored.RuntimeCleanupStateUnknown, "a preserved archive's cleanup state is known")
	require.True(t, stored.keepsPreservedWorkspaceHandle(), "the row owes a preserved-workspace reap")

	// Simulate the downgrade: the rolled-back binary's next archived checkpoint
	// erases the handle the previous release's ForStorage clears unconditionally.
	erased := simulatePreviousReleaseArchivedCheckpoint(t, stored)
	require.Nil(t, erased.RuntimeCleanup, "the downgrade erased the only handle naming the preserved directory")
	require.True(t, erased.keepsPreservedWorkspaceHandle(), "archived_workspace survives the rolled-back checkpoint")
	require.Equal(t, LiveArchived, livenessFromData(erased), "the row is still archived")

	// Roll forward to HEAD. The fail-closed regression: the row does NOT load
	// inert with a nil teardown (the pre-fix shape a no-op kill would delete
	// through). It installs the unknown-state sentinel so Kill retains it.
	restored, err := FromInstanceData(erased)
	require.NoError(t, err)
	require.NotNil(t, restored.runtimeTeardown, "an erased preserved archive must not load with a nil teardown; the no-op kill path would delete the only record pointing at the preserved directory")
	require.ErrorIs(t, restored.runtimeTeardown(), ErrWorkspaceStateUnknown, "the sentinel must surface the unknown state the destructive choke point retains on")
	require.True(t, restored.runtimeCleanupStateUnknown, "the cleanup state is unknown so the nothing-owed marker gate refuses to admit a no-op delete")
	require.Equal(t, LiveArchived, restored.liveness, "the row is still archived and inert")
	require.NotNil(t, restored.ArchivedWorkspace(), "the durable archived_workspace survives so the operator can reap or restore by hand")
	require.Equal(t, loc.Dir, restored.ArchivedWorkspace().Dir)
	require.True(t, restored.ToInstanceData().keepsPreservedWorkspaceHandle())

	// A kill committed against the erased row retains the record instead of
	// running the no-op success path a nil teardown would. The marker must not
	// be staged: the row owes a reap, not "nothing owed".
	restored.MarkUserKilled()
	tombstone := restored.ToInstanceData().ForStorage()
	require.True(t, tombstone.UserKilled, "the kill tombstone carries the kill-intent flag")
	require.False(t, tombstone.KilledWithoutLiveRuntime, "an erased preserved archive must not stage the nothing-owed marker; the marker would launder the missing handle into a no-op delete that orphans the directory")
	killErr := restored.Kill()
	require.ErrorIs(t, killErr, ErrWorkspaceStateUnknown, "kill must retain the erased preserved archive (ErrWorkspaceStateUnknown) instead of the no-op success path that deletes the record without reaping the directory")
}

// TestPreservedArchiveFailClosedSurvivesRestart pins that the fail-closed state
// is durable across a daemon restart: the row re-checkpoints (still archived,
// still owing the reap, still carrying archived_workspace but no handle) and the
// next load re-installs the unknown-state sentinel rather than drifting inert.
// Without this, the fail-closed would be a one-load reprieve and the second
// restart would orphan the directory through the nil-teardown no-op path.
func TestPreservedArchiveFailClosedSurvivesRestart(t *testing.T) {
	inst, _, _ := reattachFixtureStops(t, "", false)
	inst.ID, inst.Path, inst.started, inst.liveness = "s1", t.TempDir(), true, LiveReady
	attachStoppableAgent(t, inst, false)
	_, err := inst.ArchiveSandboxPreserving()
	require.NoError(t, err)
	inst.mu.Lock()
	inst.liveness = LiveArchived
	inst.mu.Unlock()
	stored := inst.ToInstanceData().ForStorage()
	require.NotNil(t, stored.RuntimeCleanup)

	erased := simulatePreviousReleaseArchivedCheckpoint(t, stored)
	restored, err := FromInstanceData(erased)
	require.NoError(t, err)
	require.NotNil(t, restored.runtimeTeardown)
	require.ErrorIs(t, restored.runtimeTeardown(), ErrWorkspaceStateUnknown)

	// The fail-closed row re-checkpoints through the production LiveArchived
	// branch: RuntimeCleanupStateUnknown is cleared on disk and RuntimeCleanup
	// stays nil, but keepsPreservedWorkspaceHandle() stays true, so the gate
	// re-fires on the next load.
	recheck := restored.ToInstanceData().ForStorage()
	require.Nil(t, recheck.RuntimeCleanup, "the re-checkpointed row carries no handle to rebuild")
	require.False(t, recheck.RuntimeCleanupStateUnknown, "the LiveArchived branch clears the unknown flag on disk")
	require.True(t, recheck.keepsPreservedWorkspaceHandle(), "the reap obligation is durable")

	raw, err := json.Marshal(recheck)
	require.NoError(t, err)
	var reloaded InstanceData
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	again, err := FromInstanceData(reloaded)
	require.NoError(t, err)
	require.NotNil(t, again.runtimeTeardown, "the second restart re-installed the sentinel; without it the row would load inert and a kill would orphan the directory")
	require.ErrorIs(t, again.runtimeTeardown(), ErrWorkspaceStateUnknown)
	require.True(t, again.runtimeCleanupStateUnknown, "the unknown flag is re-armed in memory so the marker gate keeps refusing the no-op delete")
	require.NotNil(t, again.ArchivedWorkspace(), "the durable handle for a manual reap survives the restart")
}

// TestPreservedArchiveCurrentBinaryRestartStillReaps is the no-regression
// counterfactual: a preserved archive that did NOT pass through a downgrade
// (the handle survived) still rebuilds a REAL teardown a kill reaps through, so
// the fail-closed is isolated to the erased-handle shape. Contrast with
// TestPreservedArchiveKeepsItsCleanupIdentityAcrossARestart, which this mirrors
// against the absence of the downgrade step.
func TestPreservedArchiveCurrentBinaryRestartStillReaps(t *testing.T) {
	inst, _, _ := reattachFixtureStops(t, "", false)
	inst.ID, inst.Path, inst.started, inst.liveness = "s1", t.TempDir(), true, LiveReady
	attachStoppableAgent(t, inst, false)
	loc, err := inst.ArchiveSandboxPreserving()
	require.NoError(t, err)
	inst.mu.Lock()
	inst.liveness = LiveArchived
	inst.mu.Unlock()
	stored := inst.ToInstanceData().ForStorage()
	require.NotNil(t, stored.RuntimeCleanup)

	// A plain JSON round-trip — no downgrade step — rebuilds the real teardown.
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	var back InstanceData
	require.NoError(t, json.Unmarshal(raw, &back))
	reloaded, err := FromInstanceData(back)
	require.NoError(t, err)
	require.NotNil(t, reloaded.runtimeTeardown, "a current-binary restart rebuilds the teardown a kill reaps through")
	// The reload is the known-state real rebuild, not the fail-closed sentinel:
	// the cleanup state is known (the sentinel arms runtimeCleanupStateUnknown),
	// and the backend carries the real cleanup identity the reap reuses.
	require.False(t, reloaded.runtimeCleanupStateUnknown, "a preserved archive whose handle survived reloads with a known cleanup state, in contrast to the fail-closed sentinel the erased row installs")
	sandbox, ok := reloaded.backend.(*sandboxBackend)
	require.True(t, ok, "the rebuilt backend is the sandbox backend the reap runs through")
	require.NotNil(t, sandbox.cleanup, "the rebuilt backend carries the real cleanup identity; the fail-closed reload leaves an inert backend with none")
	require.Equal(t, loc.Dir, reloaded.preservedSessionDir())
}

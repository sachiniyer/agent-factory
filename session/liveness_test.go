package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStatusShimRoundTrips is the #1195 Phase 1b inertness guard: SetStatus →
// GetStatus must be a faithful round-trip for every legacy Status value, so the
// two-axis decomposition/composition is invisible to existing callers.
func TestStatusShimRoundTrips(t *testing.T) {
	for _, s := range []Status{Running, Ready, Loading, Deleting, Dead, Lost, Archived} {
		i := &Instance{}
		i.SetStatusForTest(s)
		require.Equal(t, s, i.GetStatus(), "SetStatus(%v) must round-trip through GetStatus", s)
	}
}

// TestStatusAxesDecomposition documents how each legacy value lands on the two
// axes: transient values overlay the liveness (op set, liveness untouched);
// settled values set the liveness and clear the op.
func TestStatusAxesDecomposition(t *testing.T) {
	cases := []struct {
		status   Status
		liveness Liveness
		op       InFlightOp
	}{
		{Running, LiveRunning, OpNone},
		{Ready, LiveReady, OpNone},
		{Lost, LiveLost, OpNone},
		{Dead, LiveDead, OpNone},
		{Archived, LiveArchived, OpNone},
	}
	for _, c := range cases {
		i := &Instance{}
		i.SetStatusForTest(c.status)
		assert.Equal(t, c.liveness, i.liveness, "%v liveness", c.status)
		assert.Equal(t, c.op, i.inFlightOp, "%v op", c.status)
	}

	// Transient values set the op and leave the underlying liveness intact.
	i := &Instance{}
	i.SetStatusForTest(Running) // underlying liveness
	i.SetStatusForTest(Deleting)
	assert.Equal(t, LiveRunning, i.liveness, "Deleting must overlay, not overwrite, liveness")
	assert.Equal(t, OpKilling, i.inFlightOp)
	assert.Equal(t, Deleting, i.GetStatus())

	i2 := &Instance{}
	i2.SetStatusForTest(Ready)
	i2.SetStatusForTest(Loading)
	assert.Equal(t, LiveReady, i2.liveness, "Loading must overlay, not overwrite, liveness")
	assert.Equal(t, OpCreating, i2.inFlightOp)
}

// TestLivenessPersistenceRollforward guards the migration format: new records
// carry the `liveness` key; records written before #1195 (no `liveness`) decode
// to LivenessUnset so FromInstanceData falls back to the legacy `status` int.
func TestLivenessPersistenceRollforward(t *testing.T) {
	// New record: ToInstanceData writes both axes; the `liveness` key is present.
	i := &Instance{}
	i.SetStatusForTest(Lost)
	data := i.ToInstanceData()
	require.Equal(t, LiveLost, data.Liveness)
	require.Equal(t, Lost, data.Status, "legacy status still written for rollback")

	raw, err := json.Marshal(data)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"liveness":`, "new records persist the liveness axis")

	var back InstanceData
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, LiveLost, back.Liveness, "liveness survives a JSON round-trip")

	// Legacy record: only `status` on disk, no `liveness` key. It must decode to
	// LivenessUnset — the signal FromInstanceData uses to fall back to `status`.
	var legacy InstanceData
	require.NoError(t, json.Unmarshal([]byte(`{"title":"old","status":5}`), &legacy))
	assert.Equal(t, LivenessUnset, legacy.Liveness, "a pre-#1195 record has no liveness key")
	assert.Equal(t, Lost, legacy.Status, "the legacy status int is still readable")
	assert.Equal(t, LiveLost, LivenessForStatus(legacy.Status),
		"the fallback maps the legacy status onto the liveness axis")
}

// TestSnapshotInFlightOpRoundTrips guards #1436: a daemon Snapshot must carry
// the transient operation axis explicitly. The legacy Status value is lossy
// (OpArchiving and OpKilling both compose to Deleting; OpRestoring composes to
// Lost), so secondary TUIs must not reconstruct the op from Status alone.
func TestSnapshotInFlightOpRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name   string
		op     InFlightOp
		status Status
	}{
		{name: "archiving", op: OpArchiving, status: Deleting},
		{name: "restoring", op: OpRestoring, status: Lost},
		{name: "replacing", op: OpReplacing, status: Loading},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := &Instance{}
			i.SetStatusForTest(Running)
			i.SetInFlightOpForTest(tc.op)

			data := i.ToInstanceData()
			require.Equal(t, tc.status, data.Status, "legacy status still carries the composed value")
			require.Equal(t, tc.op, data.InFlightOp, "snapshot data must preserve the non-round-trippable op")

			raw, err := json.Marshal(data)
			require.NoError(t, err)
			assert.Contains(t, string(raw), `"in_flight_op":`, "snapshots encode the op axis")

			var back InstanceData
			require.NoError(t, json.Unmarshal(raw, &back))
			require.Equal(t, tc.op, inFlightOpFromData(back))
		})
	}

	legacy := InstanceData{Status: Deleting}
	require.Equal(t, OpKilling, inFlightOpFromData(legacy),
		"legacy data without in_flight_op keeps the old Deleting fallback")
}

// TestLifecycleActionIsSharedAcrossInstanceAndProjection is #2234's parity
// contract. The TUI reads Instance.LifecycleAction while the web reads the value
// serialized by ToInstanceData; both must be the same domain decision, including
// the two non-actionable rows that triggered the regression.
func TestLifecycleActionIsSharedAcrossInstanceAndProjection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     string
		live   Liveness
		op     InFlightOp
		killed bool
		want   LifecycleAction
	}{
		{name: "ready archives", id: "ready-id", live: LiveReady, want: LifecycleActionArchive},
		{name: "running archives", id: "running-id", live: LiveRunning, want: LifecycleActionArchive},
		{name: "lost restores", id: "lost-id", live: LiveLost, want: LifecycleActionRestore},
		{name: "dead restores", id: "dead-id", live: LiveDead, want: LifecycleActionRestore},
		{name: "archived restores", id: "archived-id", live: LiveArchived, want: LifecycleActionRestore},
		{name: "creating has no lifecycle action", id: "pending-id", live: LiveReady, op: OpCreating, want: LifecycleActionNone},
		{name: "replacing admits no competing lifecycle action", id: "handoff-id", live: LiveReady, op: OpReplacing, want: LifecycleActionNone},
		{name: "tombstone admits no lifecycle action", id: "killed-id", live: LiveLost, killed: true, want: LifecycleActionNone},
		{name: "id-less has no lifecycle action", live: LiveReady, want: LifecycleActionNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &Instance{ID: tc.id, liveness: tc.live, inFlightOp: tc.op, userKilled: tc.killed}
			require.Equal(t, tc.want, inst.LifecycleAction(), "TUI domain decision")
			require.Equal(t, tc.want, inst.ToInstanceData().LifecycleAction, "web projection decision")
		})
	}
}

func TestLifecycleActionIsProjectionOnly(t *testing.T) {
	data := (&Instance{ID: "ready-id", liveness: LiveReady}).ToInstanceData()
	require.Equal(t, LifecycleActionArchive, data.LifecycleAction)
	require.True(t, data.CanKill)

	stored := data.ForStorage()
	require.Equal(t, LifecycleActionNone, stored.LifecycleAction)
	require.False(t, stored.CanKill)
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "lifecycle_action",
		"instances.json must not persist a UI capability derived from live state")
	assert.NotContains(t, string(raw), "can_kill",
		"instances.json must not persist a UI capability derived from live state")
}

// TestIsRootIsSharedAcrossInstanceAndProjection pins #2513's cross-surface
// contract: the reserved-root decision the web consumes (InstanceData.IsRoot) is
// exactly the daemon's own session.IsReservedTitle, so the browser never
// re-derives the reserved-title rule (and cannot drift from its case-insensitive,
// trimmed spelling). It is projection-only — scrubbed before disk like CanKill.
func TestIsRootIsSharedAcrossInstanceAndProjection(t *testing.T) {
	for _, title := range []string{"root", "Root", "  root  ", "worker", ""} {
		data := (&Instance{ID: "id", Title: title, liveness: LiveReady}).ToInstanceData()
		require.Equal(t, IsReservedTitle(title), data.IsRoot,
			"projected IsRoot must equal session.IsReservedTitle for %q", title)
	}

	rootData := (&Instance{ID: "root-id", Title: "root", liveness: LiveReady}).ToInstanceData()
	require.True(t, rootData.IsRoot)

	stored := rootData.ForStorage()
	require.False(t, stored.IsRoot, "IsRoot is derived live, never persisted")
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "is_root",
		"instances.json must not persist a UI decision derived from the live title")
}

func TestKillAddressabilityIsSharedAcrossInstanceAndProjection(t *testing.T) {
	for _, tc := range []struct {
		name           string
		id             string
		op             InFlightOp
		startupUnknown bool
		want           bool
	}{
		{name: "settled stable row", id: "ready-id", want: true},
		{name: "startup unknown keeps teardown handle", id: "unknown-id", startupUnknown: true, want: true},
		{name: "creating has no teardown target", id: "pending-id", op: OpCreating},
		{name: "replacing already owns the teardown fence", id: "handoff-id", op: OpReplacing},
		{name: "restoring cannot be killed mid-restore", id: "restore-id", op: OpRestoring},
		{name: "id-less cannot address teardown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &Instance{ID: tc.id, liveness: LiveReady, inFlightOp: tc.op, startupStateUnknown: tc.startupUnknown}
			require.Equal(t, tc.want, inst.CanKill(), "TUI domain decision")
			require.Equal(t, tc.want, inst.ToInstanceData().CanKill, "web projection decision")
		})
	}
}

// TestRestoringRowOffersNoKill mirrors TestRespawningRowOffersNeitherArchiveNorKill
// (respawn_fence_test.go) for the restore fence. The daemon takes the per-session
// operation lock for the WHOLE restore (claimRestoreOperation sets killsInFlight in
// daemon/restore.go), so a Kill pressed during OpRestoring is rejected at the
// admission gate — before BeginKill is ever reached — with "kill already in
// progress for session X" (daemon/manager_sessions.go). That error does not match
// the user's mental model: they started a restore, not a kill, yet the message
// implies a prior kill. Kill never gets to supersede the op, so advertising it
// promises an action that fails immediately and confuses. canKillFor hides it for
// the same "don't show actions that cannot succeed" reason it already hides
// OpRespawning. The Restore verb has its OWN TUI guard for this state
// (handleRestore shows an "already being restored" notice rather than
// dispatching); Kill had no such guard and fell through to the daemon, which is
// why the exclusion belongs at the shared canKillFor gate. Both surfaces (TUI
// Instance.CanKill and the web InstanceData.CanKill projection) are asserted, and
// the teardown handle comes back when the restore completes.
func TestRestoringRowOffersNoKill(t *testing.T) {
	i := &Instance{ID: "restore-fence-id", Title: "restoring", liveness: LiveArchived}
	require.Equal(t, LifecycleActionRestore, i.LifecycleAction(),
		"an archived row offers Restore before the restore starts")
	require.True(t, i.CanKill(),
		"an archived row keeps its explicit teardown handle before the restore starts")

	// BeginRestore is the daemon edge that raises the restore fence (Archived →
	// Lost + OpRestoring) — the same edge after which claimRestoreOperation takes
	// killsInFlight, the lock KillSession would have to traverse to reach BeginKill.
	require.NoError(t, i.Transition(BeginRestore()))
	require.Equal(t, OpRestoring, i.GetInFlightOp(), "the restore fence is up")

	require.False(t, i.CanKill(),
		"Kill cannot supersede a restore — KillSession is rejected at the killsInFlight "+
			"admission gate and reports 'kill already in progress', contradicting a user who "+
			"started a restore, so advertising it promises an action it cannot perform")
	projection := i.ToInstanceData()
	require.False(t, projection.CanKill, "the web projection must hide Kill during a restore too")

	// ConfirmLive is the restore-completion edge (Lost + OpRestoring → live + None):
	// the daemon settles the restored session and the fence drops. The handle comes
	// back on both surfaces, exactly as it does when the respawn fence is lowered.
	require.NoError(t, i.Transition(ConfirmLive()))
	require.Equal(t, OpNone, i.GetInFlightOp(), "the restore fence is down")
	require.True(t, i.CanKill(), "and the teardown handle comes back")
	require.True(t, i.ToInstanceData().CanKill, "as does the web projection")
}

// TestMarkUserKilledClearsStaleOperationAndKeepsTeardownAddressable pins the
// daemon commit point: the per-session operation lock proves a carried handoff
// marker has no live owner by the time kill intent becomes durable. Leaving the
// marker would hide the only retry action on a retained unknown-state teardown.
func TestMarkUserKilledClearsStaleOperationAndKeepsTeardownAddressable(t *testing.T) {
	inst := &Instance{ID: "killed-handoff-id", liveness: LiveRunning, inFlightOp: OpReplacing}

	inst.MarkUserKilled()

	require.Equal(t, OpNone, inst.GetInFlightOp(), "durable kill intent must discard the stale handoff fence")
	require.True(t, inst.CanKill(), "a retained tombstone must keep its explicit teardown handle")
	data := inst.ToInstanceData()
	require.True(t, data.UserKilled)
	require.True(t, data.CanKill, "daemon and web projections must expose the same teardown handle")
}

func TestInFlightOpStrippedFromStorageRecords(t *testing.T) {
	data := InstanceData{Status: Deleting, Liveness: LiveRunning, InFlightOp: OpArchiving}
	stored := data.ForStorage()
	require.Equal(t, OpNone, stored.InFlightOp)
	require.Equal(t, Running, stored.Status,
		"storage must persist the settled liveness status, not a transient overlay")

	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "in_flight_op",
		"instances.json must not persist transient operations")
}

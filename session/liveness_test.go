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

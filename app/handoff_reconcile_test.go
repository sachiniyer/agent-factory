package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// A secondary TUI can observe a daemon-owned handoff entirely through
// snapshots. It must adopt OpReplacing while the runtime/mission fence is held
// and clear it when the daemon settles; otherwise panes and tab actions remain
// available during the destructive swap window.
func TestReconcileSnapshotOp_AdoptsAndSettlesHandoffFence(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "claude",
	})
	require.NoError(t, err)
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveRunning)))

	m := &home{adoptedSnapshotOps: adoptedOps{}}

	require.True(t, m.reconcileSnapshotOp(inst, session.OpReplacing, session.LiveRunning))
	require.Equal(t, session.OpReplacing, inst.GetInFlightOp())
	require.True(t, m.adoptedSnapshotOps.owns(inst),
		"an op mirrored from the daemon snapshot must be recorded as adopted (#3005)")

	require.True(t, m.reconcileSnapshotOp(inst, session.OpNone, session.LiveRunning))
	require.Equal(t, session.OpNone, inst.GetInFlightOp())
	require.False(t, m.adoptedSnapshotOps.owns(inst),
		"releasing the fence must release its provenance with it")
}

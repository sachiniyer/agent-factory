package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// adoptArchiveColdStart drives the documented cold-start entry: a secondary TUI
// launches mid-archive, so addInstanceFromSnapshot builds a projection carrying
// the snapshot's OpArchiving and records it as daemon-owned. The builder preserves
// identity (ID/CreatedAt) so the later post-cancel reconcile resolves to the same
// row rather than a swap.
func adoptArchiveColdStart(t *testing.T, h *home, title string) session.InstanceData {
	t.Helper()
	daemonInst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)
	daemonInst.SetBackend(session.NewFakeBackend())
	daemonInst.SetStartedForTest(true)
	daemonInst.SetStatusForTest(session.Running)
	daemonInst.SetInFlightOpForTest(session.OpArchiving)

	mid := daemonInst.ToInstanceData()
	require.Equal(t, session.OpArchiving, mid.InFlightOp, "precondition: snapshot carries the archive fence")
	require.Equal(t, session.LiveRunning, mid.Liveness, "precondition: liveness stays non-terminal mid-archive")

	restore := SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		inst, err := session.NewInstance(session.InstanceOptions{
			Title:   d.Title,
			Path:    t.TempDir(),
			Program: "test",
		})
		require.NoError(t, err)
		inst.ID = d.ID
		inst.CreatedAt = d.CreatedAt
		inst.SetBackend(session.NewFakeBackend())
		inst.SetStartedForTest(true)
		_ = inst.Transition(session.ObserveLiveness(snapshotLiveness(inst.GetLiveness(), d)))
		inst.SetInFlightOpForTest(d.InFlightOp)
		return inst, nil
	})
	t.Cleanup(restore)

	h.reconcileSnapshot([]session.InstanceData{mid})
	projection := h.store.GetInstanceByTitle(title)
	require.NotNil(t, projection)
	require.Equal(t, session.OpArchiving, projection.GetInFlightOp(),
		"cold-started projection adopts the daemon's exact archive op")
	require.True(t, h.adoptedSnapshotOps.owns(projection),
		"the adopted op is recorded as daemon-owned — owns(inst) must be true so the new clear branch can release it")
	return mid
}

// TestReconcile_CanceledDaemonArchiveStrandsAdoptedOpArchiving is the documented
// repro for the CancelArchive strand. The daemon raises OpArchiving over a live
// row, a TUI adopts it (here via the cold-start materializeSnapshot path), and the
// daemon then cancels the archive pre-teardown — CancelArchive reverts to OpNone
// while PRESERVING the prior non-terminal liveness. Before the fix the adopted
// OpArchiving was never released by the snapshot-reconcile path (the dead branch
// only cleared on terminal liveness), so the row stayed stuck rendering the
// archiving/Deleting overlay across every later poll, with its lifecycle actions
// fenced off. The fix clears a daemon-OWNED OpArchiving when the snapshot no
// longer carries it; this test pins the fixed behavior — the row releases to
// OpNone, becomes interactable, and stays clear across subsequent polls.
func TestReconcile_CanceledDaemonArchiveStrandsAdoptedOpArchiving(t *testing.T) {
	h := newTestHome(t)
	mid := adoptArchiveColdStart(t, h, "worker")
	projection := h.store.GetInstanceByTitle("worker")

	// Mid-archive the row is fenced: teardown in flight, no lifecycle verb, no kill.
	require.True(t, projection.IsTearingDown(), "precondition: archiving row is fenced as tearing down")
	require.Equal(t, session.LifecycleActionNone, projection.LifecycleAction(),
		"precondition: no archive/restore verb while the fence is held")
	require.False(t, projection.CanKill(), "precondition: kill is fenced off during archive")

	// The daemon cancels the archive pre-teardown: the snapshot reports OpNone with
	// the SAME non-terminal liveness. The row must release the adopted op.
	postCancel := mid
	postCancel.Status = session.Running
	postCancel.Liveness = session.LiveRunning
	postCancel.InFlightOp = session.OpNone
	h.reconcileSnapshot([]session.InstanceData{postCancel})

	require.Equal(t, session.OpNone, projection.GetInFlightOp(),
		"the reconciled snapshot must release the adopted OpArchiving the daemon no longer carries")
	require.Same(t, projection, h.store.GetInstanceByTitle("worker"),
		"the released row stays in the store — CancelArchive keeps the session")
	require.False(t, h.adoptedSnapshotOps.owns(projection),
		"releasing the op must release its provenance too")
	require.False(t, projection.IsTearingDown(), "the row is no longer fenced as tearing down")
	require.True(t, projection.CanKill(), "kill is available again once the fence releases")
	require.Equal(t, session.LifecycleActionArchive, projection.LifecycleAction(),
		"the live row offers its archive verb again — interactable, not stuck Deleting")

	// The stuck state used to survive every later poll because the dead branch was
	// hit on each one. After the fix a second OpNone poll is a stable no-op: the row
	// stays released and interactable.
	h.reconcileSnapshot([]session.InstanceData{postCancel})
	require.Equal(t, session.OpNone, projection.GetInFlightOp(),
		"a second OpNone poll must not re-strand the row")
	require.Equal(t, session.Running, projection.GetStatus(),
		"the released live row renders Running, not Deleting")
}

// TestReconcile_CanceledDaemonArchive_RunningPollReleasesAdoptedArchive covers the
// other reachability entry from the bug report: a TUI that is ALREADY open adopts
// the fence through the running-poll path (adoptSnapshotOp onto an existing row),
// then a later poll after the cancel clears it. Both entries converge on the same
// reconcileSnapshotOp branch; this guards the existing-row variant.
func TestReconcile_CanceledDaemonArchive_RunningPollReleasesAdoptedArchive(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "worker") // started, Running, in the store
	h.store.AddInstance(inst)

	// Poll 1 lands mid-fence: the daemon reports OpArchiving over the live row. The
	// reconcile adopts it onto the existing row (adoptSnapshotOp -> BeginArchive) and
	// records it as daemon-owned.
	mid := inst.ToInstanceData()
	mid.Liveness = session.LiveRunning
	mid.InFlightOp = session.OpArchiving
	h.reconcileSnapshot([]session.InstanceData{mid})
	require.Equal(t, session.OpArchiving, inst.GetInFlightOp(), "mid-fence poll adopts the archive op")
	require.True(t, h.adoptedSnapshotOps.owns(inst), "the adopted op is recorded as daemon-owned")

	// Poll 2 lands after the cancel: OpNone, same non-terminal liveness.
	postCancel := mid
	postCancel.Status = session.Running
	postCancel.Liveness = session.LiveRunning
	postCancel.InFlightOp = session.OpNone
	h.reconcileSnapshot([]session.InstanceData{postCancel})

	require.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"the post-cancel snapshot must release an ADOPTED OpArchiving on non-terminal liveness")
	require.False(t, h.adoptedSnapshotOps.owns(inst), "provenance is forgotten with the op")
	require.False(t, inst.IsTearingDown(), "the row is interactable again")
	require.True(t, inst.CanKill(), "kill is available again once the fence releases")
}

// TestReconcile_CanceledDaemonArchive_NonTerminalLivenessVariants covers every
// non-terminal liveness CancelArchive preserves (Running/Ready/LimitReached). The
// dead branch only matched terminal liveness, so each of these stranded before
// the fix; each must release now — the fix is not gated on a particular liveness.
func TestReconcile_CanceledDaemonArchive_NonTerminalLivenessVariants(t *testing.T) {
	cases := []struct {
		name     string
		liveness session.Liveness
		status   session.Status
	}{
		{"running", session.LiveRunning, session.Running},
		{"ready", session.LiveReady, session.Ready},
		// composeStatus maps LiveLimitReached to the legacy Ready value — there is no
		// LimitReached Status enum, so the snapshot carries Ready alongside it.
		{"limitReached", session.LiveLimitReached, session.Ready},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			inst := instanceWithFakeBackend(t, "worker")
			inst.SetStatusForTest(session.Running)
			h.store.AddInstance(inst)

			// Adopt the daemon's archive fence over the live row.
			mid := inst.ToInstanceData()
			mid.Liveness = tc.liveness
			mid.InFlightOp = session.OpArchiving
			h.reconcileSnapshot([]session.InstanceData{mid})
			require.Equal(t, session.OpArchiving, inst.GetInFlightOp(),
				"adopt the daemon's archive op over %s", tc.name)
			require.True(t, h.adoptedSnapshotOps.owns(inst))

			// The daemon cancels: OpNone, same non-terminal liveness.
			postCancel := mid
			postCancel.Status = tc.status
			postCancel.Liveness = tc.liveness
			postCancel.InFlightOp = session.OpNone
			h.reconcileSnapshot([]session.InstanceData{postCancel})

			require.Equal(t, session.OpNone, inst.GetInFlightOp(),
				"%s: an adopted OpArchiving must release when the snapshot reports OpNone", tc.name)
			require.False(t, h.adoptedSnapshotOps.owns(inst))
		})
	}
}

// TestReconcile_CanceledDaemonArchive_LocalArchivingPreservedOnNonTerminal guards
// the optimistic archive UX, the case the fix's owns(inst) gate exists to
// protect. A LOCAL OpArchiving (raised by this TUI's own `a` archive action, not
// adopted from a snapshot) has its own completion handler (instanceArchivedMsg)
// that owns the clear. owns(inst) returns false for it, so a non-terminal OpNone
// snapshot must leave it untouched — symmetric with the kill UX guarded by
// TestReconcile_OptimisticKillPreservedForNonTerminal.
func TestReconcile_CanceledDaemonArchive_LocalArchivingPreservedOnNonTerminal(t *testing.T) {
	h := newTestHome(t)
	inst := storeArchivingInstance(t, h, "worker") // LOCAL optimistic OpArchiving, no provenance
	require.False(t, h.adoptedSnapshotOps.owns(inst),
		"precondition: a locally raised OpArchiving is NOT daemon-owned")

	// The daemon reports OpNone over a still-live liveness (e.g. it never saw the
	// archive, or a version-skew poll). The local optimistic op must survive.
	data := inst.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveRunning
	data.InFlightOp = session.OpNone
	h.reconcileSnapshot([]session.InstanceData{data})

	require.Equal(t, session.OpArchiving, inst.GetInFlightOp(),
		"a LOCAL optimistic OpArchiving is owned by its completion handler and must not be cleared by an OpNone snapshot")
	require.True(t, inst.IsTearingDown(),
		"the local archive fence stays up — instanceArchivedMsg owns its clear")
	require.False(t, inst.CanKill(), "the local archive fence still fences kill")
}

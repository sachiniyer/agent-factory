package app

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// storeArchivingInstance adds a started, mock-backed instance to the store and
// raises the optimistic OpArchiving op over a live (Running) liveness — the exact
// local state the TUI `a` archive action leaves behind while the daemon RPC runs.
func storeArchivingInstance(t *testing.T, h *home, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatusForTest(session.Running)
	inst.SetInFlightOpForTest(session.OpArchiving)
	h.store.AddInstance(inst)
	return inst
}

// TestReconcile_ArchivingRowReachesArchived is the #1187-IMPOSSIBILITY test — the
// #1195 regression gate. The old bug: a locally-archiving row was skipped by
// isTransientStatus on every reconcile, so a poll that caught the archive fence
// stranded it on "Tearing down session…" forever. Under the two-axis model the
// daemon liveness is applied UNCONDITIONALLY and the op is reconciled on its own
// axis, so the row can never be stranded: a mid-archive snapshot leaves it
// archiving, and the terminal Archived always lands and clears the op. This test
// makes the failure structurally impossible to reproduce.
func TestReconcile_ArchivingRowReachesArchived(t *testing.T) {
	h := newTestHome(t)
	inst := storeArchivingInstance(t, h, "worker")
	require.Equal(t, session.Deleting, inst.GetStatus(), "an archiving row renders as Deleting")

	// A poll lands INSIDE the archive fence: the daemon still reports the live
	// liveness and the archive op. The row must keep showing archiving — applied
	// liveness, op preserved — never stranded.
	mid := inst.ToInstanceData()
	mid.Liveness = session.LiveRunning
	h.reconcileSnapshot([]session.InstanceData{mid})
	require.Equal(t, session.OpArchiving, inst.GetInFlightOp(), "mid-archive: op preserved")
	require.Equal(t, session.Deleting, inst.GetStatus(), "mid-archive: still visibly archiving")

	// The daemon completes the archive and reports the terminal liveness. The row
	// MUST reach Archived and clear its op — the strand the old code allowed.
	done := inst.ToInstanceData()
	done.Status = session.Archived
	done.Liveness = session.LiveArchived
	done.InFlightOp = session.OpNone
	h.reconcileSnapshot([]session.InstanceData{done})
	require.Equal(t, session.OpNone, inst.GetInFlightOp(), "the settled Archived liveness clears the op")
	require.Equal(t, session.Archived, inst.GetStatus(),
		"a daemon-confirmed Archived must always land — never stranded on Tearing down")
}

// TestReconcile_SecondaryColdStartMidArchiveReachesArchived is the #1436
// secondary-TUI regression. A non-primary TUI can cold-start while another
// client/CLI has the daemon mid-archive. The snapshot must carry OpArchiving
// explicitly; reconstructing it from Status=Deleting turns it into OpKilling,
// and the later LiveArchived snapshot never clears the stale Deleting overlay.
func TestReconcile_SecondaryColdStartMidArchiveReachesArchived(t *testing.T) {
	h := newTestHome(t)

	daemonInst, err := session.NewInstance(session.InstanceOptions{
		Title:   "worker",
		Path:    t.TempDir(),
		Program: "test",
	})
	require.NoError(t, err)
	daemonInst.SetBackend(session.NewFakeBackend())
	daemonInst.SetStartedForTest(true)
	daemonInst.SetStatusForTest(session.Running)
	daemonInst.SetInFlightOpForTest(session.OpArchiving)

	mid := daemonInst.ToInstanceData()
	require.Equal(t, session.Deleting, mid.Status, "archive fence still composes to the legacy Deleting value")
	require.Equal(t, session.LiveRunning, mid.Liveness)
	require.Equal(t, session.OpArchiving, mid.InFlightOp,
		"the daemon snapshot must preserve archiving, not only legacy Deleting")

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
	defer restore()

	h.reconcileSnapshot([]session.InstanceData{mid})
	projection := h.store.GetInstanceByTitle("worker")
	require.NotNil(t, projection)
	require.Equal(t, session.OpArchiving, projection.GetInFlightOp(),
		"cold-started projection must adopt the daemon's exact archive op")
	require.Equal(t, session.Deleting, projection.GetStatus())

	done := mid
	done.Status = session.Archived
	done.Liveness = session.LiveArchived
	done.InFlightOp = session.OpNone
	h.reconcileSnapshot([]session.InstanceData{done})

	require.Same(t, projection, h.store.GetInstanceByTitle("worker"),
		"same-session archive completion updates in place")
	require.Equal(t, session.OpNone, projection.GetInFlightOp(),
		"the settled Archived liveness clears the archive op")
	require.Equal(t, session.Archived, projection.GetStatus(),
		"a secondary TUI must converge to Archived instead of staying Deleting")
}

// TestReconcile_StaleDeletingProjectionClearsOnArchivedSnapshot covers the
// already-stranded half of #1436: a secondary TUI that reconstructed
// Status=Deleting as OpKilling must still converge once the daemon reports the
// terminal Archived liveness.
func TestReconcile_StaleDeletingProjectionClearsOnArchivedSnapshot(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "worker")
	inst.SetInFlightOpForTest(session.OpKilling)
	h.store.AddInstance(inst)
	require.Equal(t, session.Deleting, inst.GetStatus(), "precondition: stale projection is stuck as Deleting")

	data := inst.ToInstanceData()
	data.Status = session.Archived
	data.Liveness = session.LiveArchived
	data.InFlightOp = session.OpNone

	h.reconcileSnapshot([]session.InstanceData{data})

	require.Equal(t, session.OpNone, inst.GetInFlightOp())
	require.Equal(t, session.Archived, inst.GetStatus(),
		"terminal daemon Archived must clear a stale Deleting overlay")
}

// TestReconcile_StaleRestoringProjectionClearsOnLiveSnapshot is the restore-side
// #1436 convergence check. A secondary row that is visibly Lost only because of
// OpRestoring must clear that overlay when the daemon reports the restored live
// state.
func TestReconcile_StaleRestoringProjectionClearsOnLiveSnapshot(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "worker")
	inst.SetStatusForTest(session.Lost)
	inst.SetInFlightOpForTest(session.OpRestoring)
	h.store.AddInstance(inst)
	require.Equal(t, session.Lost, inst.GetStatus(), "precondition: restoring overlay composes to Lost")

	data := inst.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveRunning
	data.InFlightOp = session.OpNone

	h.reconcileSnapshot([]session.InstanceData{data})

	require.Equal(t, session.OpNone, inst.GetInFlightOp())
	require.Equal(t, session.Running, inst.GetStatus(),
		"terminal daemon Running must clear a stale restore overlay")
}

// TestReconcile_OptimisticKillPreservedForNonTerminal guards the kill UX: a
// user-initiated optimistic kill (OpKilling) stays visibly Deleting when the
// daemon still reports a live liveness (the kill hasn't completed). The op is a
// local overlay the liveness write can't touch; the row is removed only when the
// daemon drops the record (instanceKilledMsg), never mid-flight.
func TestReconcile_OptimisticKillPreservedForNonTerminal(t *testing.T) {
	cases := []struct {
		name     string
		liveness session.Liveness
	}{
		{"running", session.LiveRunning},
		{"ready", session.LiveReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			inst, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "test"})
			require.NoError(t, err)
			inst.SetBackend(session.NewFakeBackend())
			inst.SetStatusForTest(session.Running)
			inst.SetInFlightOpForTest(session.OpKilling)
			h.store.AddInstance(inst)

			data := inst.ToInstanceData()
			data.Liveness = tc.liveness
			data.InFlightOp = session.OpNone
			h.reconcileSnapshot([]session.InstanceData{data})

			require.Equal(t, session.OpKilling, inst.GetInFlightOp())
			require.Equal(t, session.Deleting, inst.GetStatus(),
				"a non-terminal daemon liveness must NOT clear an optimistic kill op (kill UX)")
		})
	}
}

// TestReconcile_ArchivedToLiveRebuildsStarted is the #1195 restore-regression
// gate (root's #1203 play-test caught this): a row that went inert on archive
// (SetArchived → started=false) MUST come back started when the daemon reports it
// live again — restored in-session or out-of-band via the CLI, both surface as a
// snapshot liveness flip through this one reconcile path. An in-place liveness
// write would leave it "live but not started" — unattachable (attach/send-keys/
// Enter/preview all fail the !started guard). The reconcile REBUILDS it (re-Start
// in place), so started + the agent-tmux binding return without a TUI relaunch.
func TestReconcile_ArchivedToLiveRebuildsStarted(t *testing.T) {
	h := newTestHome(t)

	archived, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	archived.SetBackend(session.NewFakeBackend())
	archived.SetArchived() // the inert shape: started=false, liveness=Archived
	require.False(t, archived.Started())
	h.store.AddInstance(archived)

	built := 0
	restore := SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		built++
		ri, err := session.NewInstance(session.InstanceOptions{Title: d.Title, Path: t.TempDir(), Program: "test"})
		require.NoError(t, err)
		ri.SetBackend(session.NewFakeBackend())
		ri.SetStartedForTest(true)
		ri.SetStatusForTest(session.Running)
		ri.ID = d.ID
		return ri, nil
	})
	defer restore()

	// The daemon reports the session restored (worktree back, agent re-spawned).
	data := archived.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveRunning
	data.InFlightOp = session.OpNone
	h.reconcileSnapshot([]session.InstanceData{data})

	require.Equal(t, 1, built, "an archived→live transition must rebuild (re-Start), not update in place")
	got := h.store.GetInstanceByTitle("worker")
	require.NotNil(t, got)
	require.NotSame(t, archived, got, "the inert corpse is replaced by the started rebuild")
	require.True(t, got.Started(), "the restored row must be started — attachable in-place")
	require.Equal(t, session.Running, got.GetStatus())
}

// TestReconcile_LostToLiveStaysInPlace documents the audit-adjacent coverage
// (#1203): a Lost row is started=true (FromInstanceData Starts every non-archived
// liveness), so a lost→recover transition is a plain in-place liveness update —
// no rebuild, the pointer + started persist and the by-name tmux binding
// reconnects. Only Archived→live needs the rebuild.
func TestReconcile_LostToLiveStaysInPlace(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "worker") // started=true
	inst.SetStatusForTest(session.Lost)
	h.store.AddInstance(inst)

	restore := SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return nil, fmt.Errorf("lost→live recover must not rebuild")
	})
	defer restore()

	data := inst.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveRunning
	data.InFlightOp = session.OpNone
	h.reconcileSnapshot([]session.InstanceData{data})

	require.Same(t, inst, h.store.GetInstanceByTitle("worker"), "same pointer preserved (in-place update)")
	require.True(t, inst.Started())
	require.Equal(t, session.Running, inst.GetStatus())
}

// TestReconcile_LegacyTransientSnapshotKeepsLiveness guards the mixed-version
// upgrade window (#1195 Greptile): a snapshot from a pre-#1195 daemon has no
// `liveness` field, only the old composed `status`. A legacy transient
// (Loading/Deleting) carries no real liveness in the two-axis model — it is
// version-skew noise — so it must NOT be mapped to Ready (which would flip a
// mid-kill row live→Ready). The reconcile keeps the row's current liveness until
// the upgraded daemon reports a real one.
func TestReconcile_LegacyTransientSnapshotKeepsLiveness(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "worker") // started, LiveRunning
	h.store.AddInstance(inst)

	// A mixed-version snapshot: NO liveness field (LivenessUnset), only the legacy
	// composed status Deleting — the shape an old daemon sends for a mid-kill row.
	data := inst.ToInstanceData()
	data.Liveness = session.LivenessUnset
	data.Status = session.Deleting

	h.reconcileSnapshot([]session.InstanceData{data})

	require.Equal(t, session.LiveRunning, inst.GetLiveness(),
		"a legacy Deleting snapshot (no liveness field) must not flip the row to Ready")
}

// TestHandleInstanceArchived_FinalizesRowImmediately: on a successful archive the
// local row is flipped inert Archived immediately (belt-and-suspenders with the
// reconcile clear-on-settle), so it partitions into the Archived folder without
// waiting for the next snapshot poll.
func TestHandleInstanceArchived_FinalizesRowImmediately(t *testing.T) {
	h := newTestHome(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatusForTest(session.Running)
	inst.SetInFlightOpForTest(session.OpArchiving) // as left by the optimistic archive action
	h.store.AddInstance(inst)

	h.handleInstanceArchived(instanceArchivedMsg{target: captureSessionActionTarget(inst, h.repoID)})

	require.Equal(t, session.OpNone, inst.GetInFlightOp(), "the op is cleared on finalize")
	require.Equal(t, session.Archived, inst.GetStatus(),
		"a completed archive must finalize the local row to Archived at once")
}

// TestRestore_EagerRehomeDoesNotBypassRebuild is the #1210 fix + its #1203
// non-regression, proven together in one flow (Greptile's landmine): the restore
// re-homes the row into the live Instances section AT ONCE, yet the snapshot
// reconcile still runs its Archived→live rebuild so the restored row comes back
// started==true and attachable — never "live but not started".
func TestRestore_EagerRehomeDoesNotBypassRebuild(t *testing.T) {
	h := newTestHome(t)
	archived, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	archived.SetBackend(session.NewFakeBackend())
	archived.SetArchived() // inert precondition: started=false, liveness=Archived
	h.store.AddInstance(archived)
	require.True(t, archived.ShownArchived(), "precondition: row sits in the Archived section")

	// Restore dispatched: handleRestore raises OpRestoring. The row must re-home
	// into the live Instances section IMMEDIATELY (a) — but its liveness must stay
	// Archived so the reconcile can still see the Archived→live transition.
	archived.SetInFlightOpForTest(session.OpRestoring)
	require.False(t, archived.ShownArchived(),
		"(a) an OpRestoring row must render in the live Instances section at once")
	require.Equal(t, session.LiveArchived, archived.GetLiveness(),
		"the eager re-home must NOT flip liveness — that would bypass the #1203 rebuild")

	// A builder standing in for the reconcile's rebuild (re-Start), yielding a
	// started, live instance — exactly what buildInstanceFromSnapshot does.
	built := 0
	restoreBuilder := SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		built++
		ri, err := session.NewInstance(session.InstanceOptions{Title: d.Title, Path: t.TempDir(), Program: "test"})
		require.NoError(t, err)
		ri.SetBackend(session.NewFakeBackend())
		ri.SetStartedForTest(true)
		ri.SetStatusForTest(session.Running)
		ri.ID = d.ID
		return ri, nil
	})
	defer restoreBuilder()

	// The daemon now reports the session restored (worktree back, agent re-spawned).
	data := archived.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveRunning
	data.InFlightOp = session.OpNone
	h.reconcileSnapshot([]session.InstanceData{data})

	require.Equal(t, 1, built,
		"the Archived→live transition MUST still trigger the rebuild — the eager re-home cannot bypass it (#1203)")
	got := h.store.GetInstanceByTitle("worker")
	require.NotNil(t, got)
	require.True(t, got.Started(),
		"(b) the restored row must be started==true — attachable, not 'live but not started'")
	require.Equal(t, session.Running, got.GetStatus())
	require.False(t, got.ShownArchived(),
		"(a) the restored row stays in the live Instances section")
	require.Equal(t, session.OpNone, got.GetInFlightOp(),
		"the rebuild clears the OpRestoring overlay by replacing the row")
}

// TestHandleInstanceRestored_FailureDropsBackToArchived: a failed restore clears
// the optimistic OpRestoring overlay so the row falls back into the Archived
// section (its worktree is still shelved) instead of stranding in Instances.
func TestHandleInstanceRestored_FailureDropsBackToArchived(t *testing.T) {
	h := newTestHome(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetArchived()
	inst.SetInFlightOpForTest(session.OpRestoring) // as the dispatched restore left it
	h.store.AddInstance(inst)
	require.False(t, inst.ShownArchived(), "precondition: eagerly re-homed to Instances")

	target := captureSessionActionTarget(inst, h.repoID)
	h.handleInstanceRestored(instanceRestoredMsg{target: target, err: fmt.Errorf("origin repo gone")})

	require.Equal(t, session.OpNone, inst.GetInFlightOp(), "a failed restore clears the overlay")
	require.True(t, inst.ShownArchived(),
		"a failed restore drops the row back into the Archived section")
}

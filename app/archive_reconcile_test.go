package app

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// storeArchivingInstance adds a started, mock-backed instance to the store and
// raises the optimistic OpArchiving op over a live (Running) liveness — the exact
// local state the TUI `A` archive action leaves behind while the daemon RPC runs.
func storeArchivingInstance(t *testing.T, h *home, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatus(session.Running)
	inst.SetInFlightOp(session.OpArchiving)
	h.store.AddInstance(inst)
	return inst
}

// TestReconcile_ArchivingRowReachesArchived is the #1187-IMPOSSIBILITY test — the
// #1195 regression gate. The old bug: a locally-archiving row was skipped by
// isTransientStatus on every reconcile, so a poll that caught the archive fence
// stranded it on "Tearing down session…" forever. Under the two-axis model the
// daemon liveness is applied UNCONDITIONALLY (it never carries the op), so the row
// can never be stranded: a mid-archive snapshot leaves it archiving, and the
// terminal Archived always lands and clears the op. This test makes the failure
// structurally impossible to reproduce.
func TestReconcile_ArchivingRowReachesArchived(t *testing.T) {
	h := newTestHome(t)
	inst := storeArchivingInstance(t, h, "worker")
	require.Equal(t, session.Deleting, inst.GetStatus(), "an archiving row renders as Deleting")

	// A poll lands INSIDE the archive fence: the daemon still reports the live
	// liveness (its OpArchiving fence is not serialized). The row must keep
	// showing archiving — applied liveness, op preserved — never stranded.
	mid := inst.ToInstanceData()
	mid.Liveness = session.LiveRunning
	h.reconcileSnapshot([]session.InstanceData{mid})
	require.Equal(t, session.OpArchiving, inst.GetInFlightOp(), "mid-archive: op preserved")
	require.Equal(t, session.Deleting, inst.GetStatus(), "mid-archive: still visibly archiving")

	// The daemon completes the archive and reports the terminal liveness. The row
	// MUST reach Archived and clear its op — the strand the old code allowed.
	done := inst.ToInstanceData()
	done.Liveness = session.LiveArchived
	h.reconcileSnapshot([]session.InstanceData{done})
	require.Equal(t, session.OpNone, inst.GetInFlightOp(), "the settled Archived liveness clears the op")
	require.Equal(t, session.Archived, inst.GetStatus(),
		"a daemon-confirmed Archived must always land — never stranded on Tearing down")
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
			inst.SetStatus(session.Running)
			inst.SetInFlightOp(session.OpKilling)
			h.store.AddInstance(inst)

			data := inst.ToInstanceData()
			data.Liveness = tc.liveness
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
		ri.SetStatus(session.Running)
		ri.ID = d.ID
		return ri, nil
	})
	defer restore()

	// The daemon reports the session restored (worktree back, agent re-spawned).
	data := archived.ToInstanceData()
	data.Liveness = session.LiveRunning
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
	inst.SetStatus(session.Lost)
	h.store.AddInstance(inst)

	restore := SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return nil, fmt.Errorf("lost→live recover must not rebuild")
	})
	defer restore()

	data := inst.ToInstanceData()
	data.Liveness = session.LiveRunning
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
	inst.SetStatus(session.Running)
	inst.SetInFlightOp(session.OpArchiving) // as left by the optimistic archive action
	h.store.AddInstance(inst)

	h.handleInstanceArchived(instanceArchivedMsg{title: "worker"})

	require.Equal(t, session.OpNone, inst.GetInFlightOp(), "the op is cleared on finalize")
	require.Equal(t, session.Archived, inst.GetStatus(),
		"a completed archive must finalize the local row to Archived at once")
}

// TestHandleInstanceRestored_FinalizesRowImmediately is the #1210 regression:
// on a successful restore the local row must flip back off Archived immediately
// (mirroring the archive finalize), so it re-homes into the live Instances
// section without lingering in the Archived folder until the next snapshot poll.
func TestHandleInstanceRestored_FinalizesRowImmediately(t *testing.T) {
	h := newTestHome(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetArchived() // the archived precondition: started=false, liveness=Archived
	h.store.AddInstance(inst)
	require.Equal(t, session.LiveArchived, inst.GetLiveness(), "precondition: row is archived")

	h.handleInstanceRestored(instanceRestoredMsg{title: "worker"})

	require.NotEqual(t, session.LiveArchived, inst.GetLiveness(),
		"a completed restore must move the row out of the Archived partition at once")
	require.Equal(t, session.LiveRunning, inst.GetLiveness(),
		"restore flips liveness back to a live state")
	require.Equal(t, session.OpNone, inst.GetInFlightOp(), "no op strands after restore")
	require.True(t, inst.Started(),
		"restore restores the started flag, symmetric with SetArchived (#1203)")
}

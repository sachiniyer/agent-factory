package app

import (
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

package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// storeDeletingInstance adds a started, mock-backed instance to the store and
// marks it Deleting — the exact local state a snapshot poll leaves behind when
// it lands inside the archive teardown fence (#1028). Returns the instance.
func storeDeletingInstance(t *testing.T, h *home, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatus(session.Deleting)
	h.store.AddInstance(inst)
	return inst
}

// TestReconcile_TerminalStatusOverridesLocalDeleting is the play-test regression
// (#1028): a row locally mirrored to the transient Deleting (a snapshot caught
// the archive teardown fence) must be updated when the daemon's NEXT snapshot
// reports a TERMINAL status. Without the override, isTransientStatus skips the
// row forever and it strands on "Tearing down session…", never reaching the
// Archived folder. Covers all three terminal states.
func TestReconcile_TerminalStatusOverridesLocalDeleting(t *testing.T) {
	cases := []struct {
		name   string
		status session.Status
	}{
		{"archived", session.Archived},
		{"lost", session.Lost},
		{"dead", session.Dead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			inst := storeDeletingInstance(t, h, "worker")

			// The daemon (single writer) now reports the settled terminal status
			// for the same session (matching CreatedAt — not a kill+recreate swap).
			h.reconcileSnapshot([]session.InstanceData{
				{Title: "worker", CreatedAt: inst.CreatedAt, Status: tc.status},
			})

			require.Equal(t, tc.status, inst.GetStatus(),
				"a daemon terminal status must override a stale local Deleting row")
		})
	}
}

// TestReconcile_OptimisticDeletingPreservedForNonTerminal guards the kill UX:
// a user-initiated optimistic Deleting row must STILL be left alone when the
// daemon reports a non-terminal status (the kill hasn't completed yet), so the
// "Tearing down…" feedback shows until the daemon confirms. Only a terminal
// status may override it.
func TestReconcile_OptimisticDeletingPreservedForNonTerminal(t *testing.T) {
	cases := []struct {
		name   string
		status session.Status
	}{
		{"running", session.Running},
		{"ready", session.Ready},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			inst := storeDeletingInstance(t, h, "worker")

			h.reconcileSnapshot([]session.InstanceData{
				{Title: "worker", CreatedAt: inst.CreatedAt, Status: tc.status},
			})

			require.Equal(t, session.Deleting, inst.GetStatus(),
				"a non-terminal daemon status must NOT clobber an optimistic Deleting row (kill UX)")
		})
	}
}

// TestHandleInstanceArchived_FinalizesRowImmediately: on a successful archive the
// local row is flipped to Archived immediately (belt-and-suspenders with the
// reconcile override), so it partitions into the Archived folder without waiting
// for the next snapshot poll.
func TestHandleInstanceArchived_FinalizesRowImmediately(t *testing.T) {
	h := newTestHome(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatus(session.Deleting) // as left by the archive fence
	h.store.AddInstance(inst)

	h.handleInstanceArchived(instanceArchivedMsg{title: "worker"})

	require.Equal(t, session.Archived, inst.GetStatus(),
		"a completed archive must finalize the local row to Archived at once")
}

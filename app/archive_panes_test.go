package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// TestArchive_ClosesOpenPanesViaFinalize (#1028 P1): archiving a session that has
// an open pane must close that pane — an archived session's tmux/worktree is
// torn down, so a live pane would dangle (the archived-row "no live panes"
// contract). The finalize path (TUI `A` → handleInstanceArchived) closes it via
// the synchronous pane prune in selectionChanged.
func TestArchive_ClosesOpenPanesViaFinalize(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "worker")
	inst.SetStatus(session.Running)
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	_, _ = h.openOrFocusPane(inst, 0) // open the agent pane
	require.Equal(t, 1, h.store.NumOpenPanes(), "precondition: a pane is open on the session")

	h.handleInstanceArchived(instanceArchivedMsg{title: inst.Title})

	require.Equal(t, session.Archived, inst.GetStatus())
	require.True(t, h.store.ContainsInstance(inst),
		"an archived instance stays in the projection (it moves to the Archived folder)")
	require.Equal(t, 0, h.store.NumOpenPanes(),
		"archiving must close panes bound to the session — an archived row has no live panes")
}

// TestArchive_ReconcileClosesOpenPanes (#1028 P1): an out-of-band archive (`af
// sessions archive` while the TUI runs) mirrored by the snapshot reconcile must
// ALSO close the session's panes, not just the TUI `A` path.
func TestArchive_ReconcileClosesOpenPanes(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "worker")
	inst.SetStatus(session.Running)
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	_, _ = h.openOrFocusPane(inst, 0)
	require.Equal(t, 1, h.store.NumOpenPanes())

	// The daemon reports the session archived (same session — matching id).
	data := inst.ToInstanceData()
	data.Liveness = session.LiveArchived
	h.reconcileSnapshot([]session.InstanceData{data})

	require.Equal(t, session.Archived, inst.GetStatus())
	require.Equal(t, 0, h.store.NumOpenPanes(),
		"an out-of-band archive must close the session's panes via the reconcile prune")
}

// TestRestore_LeavesNoStalePaneBinding (#1028 P1 audit): archive closes the
// session's panes, so restore never needs to touch panes — and it must never
// leave a pane bound to a dead/archived session. Any pane present after the
// round trip (e.g. a fresh preview auto-opened for the now-live selected row)
// must bind to the LIVE restored instance, not a stale one.
func TestRestore_LeavesNoStalePaneBinding(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "worker")
	inst.SetStatus(session.Running)
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	_, _ = h.openOrFocusPane(inst, 0)
	require.Equal(t, 1, h.store.NumOpenPanes())

	// Archive closes the pane bound to the (about-to-be) archived session.
	h.handleInstanceArchived(instanceArchivedMsg{title: inst.Title})
	require.Equal(t, 0, h.store.NumOpenPanes(),
		"archive closes the session's panes — no live pane on an archived row")

	// Restore flips it back to Running via the reconcile.
	data := inst.ToInstanceData()
	data.Liveness = session.LiveRunning
	h.reconcileSnapshot([]session.InstanceData{data})
	require.Equal(t, session.Running, inst.GetStatus())

	// Whatever panes exist now must bind ONLY to the live restored instance —
	// never a stale/archived binding.
	for _, p := range h.store.OpenPanes() {
		require.True(t, h.store.ContainsInstance(p.Instance()),
			"no pane may be bound to a session that left the projection")
		require.NotEqual(t, session.Archived, p.Instance().GetStatus(),
			"no pane may be bound to an archived session after restore")
	}
}

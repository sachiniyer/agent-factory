package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pane bindings across a tab-set change: the shared close/rebind/rename
// semantics of reconcilePanesForTabs, driven through the REAL entry points —
// the TUI `w` kill (handleCloseTab) and the daemon snapshot reconcile
// (reconcileSnapshot). Both must agree on what happened to a tab, because the
// daemon is the source of truth and a tab can change with no local action at
// all (#960). A pane is part of its tab: it closes only when its tab is really
// gone, follows the tab across a reorder, and survives a rename untouched
// (#1088, #1813, #1905).

// TestPane_CloseTabRebindsPanes: `t` opens the fresh tab as a pane, and
// killing a middle tab (w) hides its pane while shifting higher-slot panes'
// bindings down so they keep showing the same tab.
func TestPane_CloseTabRebindsPanes(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "rebind")
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	restore := SetTabCreatorForTest(func(daemon.CreateTabRequest) (string, string, error) {
		return spawnDaemonTab(inst)
	})
	defer restore()
	_, _ = h.handleNewTab() // agent + shell + shell-2
	require.Equal(t, 3, inst.TabCount())
	require.Equal(t, 1, h.store.NumOpenPanes(), "t opens the fresh tab as a pane")
	require.Equal(t, 2, h.store.OpenPanes()[0].Tab(), "bound to the new last slot")

	// Also open the slot-1 shell pane.
	_, _ = h.openOrFocusPane(inst, 1)
	require.Equal(t, 2, h.store.NumOpenPanes())

	// Kill tab 1: its pane hides; the slot-2 pane re-binds to slot 1.
	h.store.SetActiveTab(1)
	restoreClose := SetTabCloserForTest(func(daemon.CloseTabRequest) error { return nil })
	defer restoreClose()
	_, _ = h.handleCloseTab()

	require.Equal(t, 2, inst.TabCount())
	require.Equal(t, 1, h.store.NumOpenPanes(), "the killed tab's pane leaves the workspace")
	assert.Equal(t, 1, h.store.OpenPanes()[0].Tab(), "the surviving pane re-binds to the shifted slot")
}

// TestPane_SnapshotTabRemovalRebindsPanes is the daemon-driven twin of
// TestPane_CloseTabRebindsPanes (Greptile on PR #1099): a tab that disappears
// from the SNAPSHOT out-of-band — another client, `af sessions tab-delete`,
// a daemon-side removal — must apply the SAME pane close/rebind semantics as
// the TUI `w` kill (shared reconcilePanesForTabs): the vanished tab's pane
// closes, higher-slot panes re-bind to their shifted slot, and the focus
// ring + layout are consistent the moment the reconcile returns. This is the
// #960 contract: the daemon is the source of truth, tabs change with no
// local action.
func TestPane_SnapshotTabRemovalRebindsPanes(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "snaprebind")
	inst.SetStatusForTest(session.Running)
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	restore := SetTabCreatorForTest(func(daemon.CreateTabRequest) (string, string, error) {
		return spawnDaemonTab(inst)
	})
	defer restore()
	_, _ = h.handleNewTab() // agent + shell + shell-2; opens the slot-2 pane
	require.Equal(t, 3, inst.TabCount())
	_, _ = h.openOrFocusPane(inst, 1) // and the slot-1 ("shell") pane, focused
	require.Equal(t, 2, h.store.NumOpenPanes())

	// The daemon reports "shell" gone: the snapshot carries agent + shell-2.
	data := inst.ToInstanceData()
	require.Equal(t, "shell", data.Tabs[1].Name)
	data.Tabs = append(data.Tabs[:1], data.Tabs[2:]...)

	require.True(t, h.reconcileSnapshot([]session.InstanceData{data}))

	require.Equal(t, 2, inst.TabCount(), "the snapshot's tab set is mirrored")
	require.Equal(t, 1, h.store.NumOpenPanes(), "the vanished tab's pane closes")
	p := h.store.OpenPanes()[0]
	assert.Equal(t, 1, p.Tab(), "the shell-2 pane re-binds to its shifted slot")
	assert.Equal(t, "shell-2", inst.GetTabs()[p.Tab()].Name,
		"no pane may be left showing a shifted/stale tab")
	assert.Equal(t, 1, h.lastLayout.PaneCount(), "the layout re-solves in the same reconcile")
	assert.Equal(t, layout.PaneRegion(p.ID()), h.ring.Active(),
		"focus lands cleanly on the surviving selected pane after the focused pane closes")
}

// TestPane_SnapshotTabRemovalKeepsUnaffectedPaneBinding: removing a HIGHER
// slot out-of-band closes only that pane — a pane on a lower slot keeps its
// binding untouched (no spurious rebind).
func TestPane_SnapshotTabRemovalKeepsUnaffectedPaneBinding(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "snapkeep")
	inst.SetStatusForTest(session.Running)
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	restore := SetTabCreatorForTest(func(daemon.CreateTabRequest) (string, string, error) {
		return spawnDaemonTab(inst)
	})
	defer restore()
	_, _ = h.handleNewTab() // agent + shell + shell-2; opens the slot-2 pane
	require.Equal(t, 3, inst.TabCount())
	_, _ = h.openOrFocusPane(inst, 1)
	require.Equal(t, 2, h.store.NumOpenPanes())

	// The daemon reports "shell-2" (the last slot) gone.
	data := inst.ToInstanceData()
	require.Equal(t, "shell-2", data.Tabs[2].Name)
	data.Tabs = data.Tabs[:2]

	require.True(t, h.reconcileSnapshot([]session.InstanceData{data}))

	require.Equal(t, 1, h.store.NumOpenPanes(), "only the vanished tab's pane closes")
	p := h.store.OpenPanes()[0]
	assert.Equal(t, 1, p.Tab(), "the unaffected pane keeps its slot")
	assert.Equal(t, "shell", inst.GetTabs()[p.Tab()].Name)
}

// TestPane_SnapshotTabRenameKeepsPaneOpen (#1905): renaming a tab from another
// surface (the web UI, `af sessions tab-rename`) must not disturb the tab — and
// a pane showing it is part of the tab. Keyed on the tab NAME, the pane
// reconcile read a rename as "the tab I was showing is gone" and CLOSED the
// user's pane; only the stable id (#1738/#1805) survives a rename, which is why
// the pane binding must resolve through it.
func TestPane_SnapshotTabRenameKeepsPaneOpen(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "snaprename")
	inst.SetStatusForTest(session.Running)
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	_, err := inst.AddWebTab("http://localhost:3000", "dash")
	require.NoError(t, err)
	require.Equal(t, 2, inst.TabCount())
	_, _ = h.openOrFocusPane(inst, 1)
	require.Equal(t, 1, h.store.NumOpenPanes())

	// The daemon reports the tab renamed: same stable id, new name.
	data := inst.ToInstanceData()
	require.Equal(t, "dash", data.Tabs[1].Name)
	require.NotEmpty(t, data.Tabs[1].ID, "the rename case is only expressible with a stable id")
	data.Tabs[1].Name = "metrics"

	require.True(t, h.reconcileSnapshot([]session.InstanceData{data}))

	require.Equal(t, 2, inst.TabCount(), "a rename never changes the tab SET")
	require.Equal(t, 1, h.store.NumOpenPanes(), "a rename must not close the tab's open pane")
	p := h.store.OpenPanes()[0]
	assert.Equal(t, 1, p.Tab(), "the pane stays bound to its tab's slot")
	assert.Equal(t, "metrics", inst.GetTabs()[p.Tab()].Name, "the pane's label repaints to the new name")
}

// TestPane_SnapshotTabReorderFollowsPaneToNewSlot: a reorder moves a tab to a
// new slot, and its open pane must FOLLOW it rather than keep showing whatever
// slid into the old slot. This is the stable-id guarantee stated as a pane
// binding (#1813 feature 4).
func TestPane_SnapshotTabReorderFollowsPaneToNewSlot(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "snaporder")
	inst.SetStatusForTest(session.Running)
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	_, err := inst.AddWebTab("http://localhost:3000", "a")
	require.NoError(t, err)
	_, err = inst.AddWebTab("http://localhost:3001", "b")
	require.NoError(t, err)
	_, _ = h.openOrFocusPane(inst, 1) // the "a" pane
	require.Equal(t, 1, h.store.NumOpenPanes())

	// The daemon reports the roster reordered to [agent, b, a].
	data := inst.ToInstanceData()
	require.Equal(t, []string{"a", "b"}, []string{data.Tabs[1].Name, data.Tabs[2].Name})
	data.Tabs[1], data.Tabs[2] = data.Tabs[2], data.Tabs[1]

	require.True(t, h.reconcileSnapshot([]session.InstanceData{data}))

	require.Equal(t, 1, h.store.NumOpenPanes(), "a reorder must not close a pane")
	p := h.store.OpenPanes()[0]
	assert.Equal(t, 2, p.Tab(), "the pane re-binds to its tab's new slot")
	assert.Equal(t, "a", inst.GetTabs()[p.Tab()].Name, "the pane still shows the SAME tab it was showing")
}

// TestPane_SnapshotTabIDAdoptionKeepsPaneOpen guards the id-less rollforward at
// the pane level. A tab can be live locally with NO id — AttachShellTab leaves an
// optimistic tab's id empty on purpose, and its pane is opened on it immediately
// — and the next snapshot then ADOPTS the daemon's id for it. Adoption changes
// the tab's identity from "no id" to a real one while the tab itself does not
// change at all, so a binding resolved by id ALONE reads it as a vanished tab and
// closes a pane the user is looking at. Only the name carries the binding across
// an adoption.
//
// The adoption must land in the same snapshot as an unrelated change — here, a
// second tab appearing — because id adoption on its own is deliberately not a
// visible change (ReconcileTabsFromData), so it never reaches this reconcile by
// itself. Co-occurring is the common case, not a contrived one: the snapshot that
// first carries a new tab's id is routinely the one carrying the next tab too.
func TestPane_SnapshotTabIDAdoptionKeepsPaneOpen(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "snapadopt")
	inst.SetStatusForTest(session.Running)
	selectInstance(h, inst)
	resizeHome(h, 200, 40)

	_, err := inst.AddWebTab("http://localhost:3000", "dash")
	require.NoError(t, err)
	data := inst.ToInstanceData() // captured WITH the daemon's id...
	require.NotEmpty(t, data.Tabs[1].ID)
	inst.GetTabs()[1].ID = "" // ...while the local tab has none yet.
	_, _ = h.openOrFocusPane(inst, 1)
	require.Equal(t, 1, h.store.NumOpenPanes())

	// The same snapshot adopts "dash"'s id AND adds a second tab.
	data.Tabs = append(data.Tabs, session.TabData{
		ID: "extra-id", Name: "extra", Kind: session.TabKindWeb, URL: "http://localhost:3001",
	})
	require.True(t, h.reconcileSnapshot([]session.InstanceData{data}))

	require.Equal(t, 3, inst.TabCount())
	require.Equal(t, 1, h.store.NumOpenPanes(), "adopting an id must not close the tab's open pane")
	p := h.store.OpenPanes()[0]
	assert.Equal(t, 1, p.Tab(), "the pane keeps its tab")
	assert.Equal(t, "dash", inst.GetTabs()[p.Tab()].Name)
	assert.Equal(t, data.Tabs[1].ID, inst.GetTabs()[1].ID, "the daemon's id is adopted")
}

// TestPane_SnapshotInstanceRemovalPrunesPanes is the whole-instance variant:
// an instance that disappears from the snapshot (killed out-of-band) takes
// its open panes with it in the SAME reconcile — ring reshaped, surviving
// panes re-fit — rather than waiting for a later tick.

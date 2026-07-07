package app

import (
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSnapshotTestInstance builds an unstarted, non-transient sidebar instance.
// Unstarted is fine for the reconcile-orchestration tests: ReconcileTabsFromData
// no-ops on a not-started instance, so these tests stay hermetic (no tmux), and
// the tab-reconnect path is covered in the session package
// (TestReconcileTabsFromData_AddsOutOfBandTab).
func newSnapshotTestInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	inst.SetStatusForTest(session.Running)
	return inst
}

// TestReconcileSnapshot_AddsAndRemovesSessions: whole sessions appearing in /
// disappearing from the daemon snapshot are added to / removed from the sidebar.
func TestReconcileSnapshot_AddsAndRemovesSessions(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	keep := newSnapshotTestInstance(t, "keep")
	h.store.AddInstance(keep)

	// Snapshot adds "new" alongside the existing "keep" (matched by CreatedAt).
	added := h.reconcileSnapshot([]session.InstanceData{
		keep.ToInstanceData(),
		{Title: "new", CreatedAt: time.Now()},
	})
	assert.True(t, added, "a new session in the snapshot is a change")
	require.NotNil(t, findSidebarInstance(h, "keep"))
	require.NotNil(t, findSidebarInstance(h, "new"), "out-of-band session must appear in the sidebar")

	// Snapshot drops "keep"; only "new" remains.
	newInst := findSidebarInstance(h, "new")
	removed := h.reconcileSnapshot([]session.InstanceData{newInst.ToInstanceData()})
	assert.True(t, removed, "a session gone from the snapshot is a change")
	assert.Nil(t, findSidebarInstance(h, "keep"), "session gone from the snapshot must leave the sidebar")
	require.NotNil(t, findSidebarInstance(h, "new"))
}

// TestReconcileSnapshot_PreservesSelectionAndActiveTab: reconciling (including a
// removal that shifts indices) must not drift the user's selected instance or
// the active tab index (#684) — local-only view state survives a snapshot.
func TestReconcileSnapshot_PreservesSelectionAndActiveTab(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	a := newSnapshotTestInstance(t, "a")
	b := newSnapshotTestInstance(t, "b")
	c := newSnapshotTestInstance(t, "c")
	h.store.AddInstance(a)
	h.store.AddInstance(b)
	h.store.AddInstance(c)
	h.sidebar.SelectInstance(b)
	require.Same(t, b, h.sidebar.GetSelectedInstance())

	h.store.SetActiveTab(1)
	require.Equal(t, 1, h.store.ActiveTab(), "set a non-zero active tab")

	// Drop "a" (precedes the selection, so its removal shifts indices) and add
	// "d"; keep "b" and "c".
	changed := h.reconcileSnapshot([]session.InstanceData{
		b.ToInstanceData(),
		c.ToInstanceData(),
		{Title: "d", CreatedAt: time.Now()},
	})
	assert.True(t, changed)

	require.Same(t, b, h.sidebar.GetSelectedInstance(),
		"selection must stay pinned to b even though removing a preceding row shifted indices")
	assert.Equal(t, 1, h.store.ActiveTab(),
		"the active tab index must be preserved across a reconcile (reconcileSnapshot never touches it)")
	assert.Nil(t, findSidebarInstance(h, "a"))
	require.NotNil(t, findSidebarInstance(h, "d"))
}

// TestReconcileSnapshot_SelectionDriftAfterSwapAndRemoval: when the selected
// instance is swapped (kill+recreate with same title) AND another preceding
// instance is removed in the same reconcile cycle, the selection must stay on
// the recreated instance, not drift to the wrong row (#969). The pre-fix re-pin
// used pointer equality, which missed the rebuilt instance after the swap.
func TestReconcileSnapshot_SelectionDriftAfterSwapAndRemoval(t *testing.T) {
	h := newTestHome(t)

	var rebuilt *session.Instance
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		inst := newSnapshotTestInstance(t, d.Title)
		rebuilt = inst
		return inst, nil
	}))

	a := newSnapshotTestInstance(t, "a")
	b := newSnapshotTestInstance(t, "b")
	c := newSnapshotTestInstance(t, "c")
	h.store.AddInstance(a)
	h.store.AddInstance(b)
	h.store.AddInstance(c)
	h.sidebar.SelectInstance(b)
	require.Same(t, b, h.sidebar.GetSelectedInstance(), "initial selection must be on b")

	// Snapshot: b was recreated (same title, NEW session id — a kill+recreate
	// mints a fresh stable id, #1195), a is gone, c unchanged.
	bData := b.ToInstanceData()
	bData.ID = "recreated-b"
	bData.CreatedAt = time.Now().Add(time.Hour)
	cData := c.ToInstanceData()

	changed := h.reconcileSnapshot([]session.InstanceData{bData, cData})
	assert.True(t, changed)

	// Selection must follow the rebuilt "b" instance, not drift to c.
	selected := h.sidebar.GetSelectedInstance()
	require.NotNil(t, selected, "selection must not be nil")
	assert.Same(t, rebuilt, selected,
		"selection must stay on the recreated instance (b'), not drift to c")
}

// TestReconcileSnapshot_NoChangeWhenUnchanged: an identical snapshot reports no
// change (so the caller skips the repaint) and updates rows IN PLACE rather than
// rebuilding them.
func TestReconcileSnapshot_NoChangeWhenUnchanged(t *testing.T) {
	h := newTestHome(t)
	a := newSnapshotTestInstance(t, "a")
	h.store.AddInstance(a)

	changed := h.reconcileSnapshot([]session.InstanceData{a.ToInstanceData()})
	assert.False(t, changed, "an unchanged snapshot must not report a change (no repaint/flicker)")
	require.Same(t, a, findSidebarInstance(h, "a"), "an existing row is updated in place, not rebuilt")
}

// TestReconcileSnapshot_SwapsKillRecreatedSameTitle: a same-title row whose
// identity (CreatedAt) differs from the snapshot is a kill+recreate (#765); the
// stale row is swapped for the recreated one rather than mutated in place.
func TestReconcileSnapshot_SwapsKillRecreatedSameTitle(t *testing.T) {
	h := newTestHome(t)

	stale := newSnapshotTestInstance(t, "dup")
	h.store.AddInstance(stale)

	recreated := newSnapshotTestInstance(t, "dup") // distinct pointer + CreatedAt
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return recreated, nil
	}))

	changed := h.reconcileSnapshot([]session.InstanceData{recreated.ToInstanceData()})
	assert.True(t, changed)

	var matches []*session.Instance
	for _, inst := range h.store.GetInstances() {
		if inst.Title == "dup" {
			matches = append(matches, inst)
		}
	}
	require.Len(t, matches, 1, "exactly one row for the reused title")
	require.Same(t, recreated, matches[0], "the recreated instance must replace the stale corpse")
}

// TestFetchSnapshotCmd_UsesFetcherSeam exercises the full off-loop fetch →
// on-loop reconcile path through the per-home snapshotFetcher field: the fetch
// command is scoped to the home's repo, and its result reconciles into the
// sidebar — the #959 live-display flow with no real daemon. The fetcher is a home
// field (not a package global), so this never races a sibling test's swap.
func TestFetchSnapshotCmd_UsesFetcherSeam(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	called := false
	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		called = true
		require.Equal(t, h.repoID, repoID, "the snapshot fetch must be scoped to this repo")
		return daemon.SnapshotResponse{Instances: []session.InstanceData{{Title: "viaseam", CreatedAt: time.Now()}}}, nil
	}

	msg := h.fetchSnapshotCmd()()
	require.True(t, called, "fetchSnapshotCmd must call the snapshot fetcher seam")
	snap, ok := msg.(snapshotFetchedMsg)
	require.True(t, ok, "fetchSnapshotCmd must return a snapshotFetchedMsg")
	require.NoError(t, snap.err)

	require.True(t, h.handleSnapshot(snap), "the fetched session is a change")
	require.NotNil(t, findSidebarInstance(h, "viaseam"),
		"the snapshot fetched through the seam must reconcile into the sidebar")
}

// TestHandleSnapshot_WarmingDaemonLeavesSidebarIntact: a warming-daemon fetch
// error is retryable, not a reason to drop the cold-start sidebar (#829).
func TestHandleSnapshot_WarmingDaemonLeavesSidebarIntact(t *testing.T) {
	h := newTestHome(t)
	a := newSnapshotTestInstance(t, "a")
	h.store.AddInstance(a)

	changed := h.handleSnapshot(snapshotFetchedMsg{
		err: errors.New("agent-factory daemon is starting (restoring sessions); retry shortly"),
	})
	assert.False(t, changed)
	require.Same(t, a, findSidebarInstance(h, "a"),
		"a warming-daemon snapshot fetch must leave the sidebar intact for a retry")
}

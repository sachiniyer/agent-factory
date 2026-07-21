package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// instanceWithFakeBackend builds an instance backed by FakeBackend, marked
// Started and Running. Used by metadata-tick tests to exercise the loop body
// without spinning up real tmux sessions.
func instanceWithFakeBackend(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	return inst
}

// TestReconcileSnapshot_MirrorsStatusOntoExistingRow is the TUI half of the
// #960 PR 5 move: the TUI no longer computes status (runMetadataTick is gone) —
// it renders the daemon's authoritative status straight from the Snapshot. A row
// whose live status differs from the snapshot's must be updated in place to the
// snapshot value, with no tmux probe of its own.
func TestReconcileSnapshot_MirrorsStatusOntoExistingRow(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a") // starts Running, started=true
	h.store.AddInstance(inst)

	// The daemon's snapshot reports this session is now Lost. The TUI must adopt
	// the daemon's liveness verbatim — it does not re-derive it from a local probe.
	data := inst.ToInstanceData()
	data.Liveness = session.LiveLost

	changed := h.reconcileSnapshot([]session.InstanceData{data})

	assert.True(t, changed, "mirroring a new liveness must report a change so the sidebar repaints")
	assert.Equal(t, session.Lost, inst.GetStatus(),
		"the TUI must render the daemon's snapshot liveness, not compute its own")
}

// TestReconcileSnapshot_LeavesTransientOpAlone guards the #1195 structural
// property: a row the TUI owns mid-kill (local OpKilling) keeps its op through a
// reconcile even though the daemon liveness is applied. A terminal snapshot
// clears kill by removing the row; a non-terminal liveness write can't touch the
// separate local op axis, so the composed status stays Deleting — replacing the
// old isTransientStatus skip.
func TestReconcileSnapshot_LeavesTransientOpAlone(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a")
	inst.SetInFlightOpForTest(session.OpKilling)
	h.store.AddInstance(inst)

	data := inst.ToInstanceData()
	data.Liveness = session.LiveReady // daemon doesn't know about the in-flight kill
	data.InFlightOp = session.OpNone

	h.reconcileSnapshot([]session.InstanceData{data})

	assert.Equal(t, session.OpKilling, inst.GetInFlightOp(),
		"a reconcile liveness write must not touch the local kill op")
	assert.Equal(t, session.Deleting, inst.GetStatus(),
		"a mid-teardown row must keep its Deleting marker through a reconcile")
}

// TestReconcileSnapshot_IdentityUsesStableID guards the #1195 identity fix: the
// reconcile decides "same session" vs "title reused" (#765) by the stable
// per-session ID, not CreatedAt equality (the audit's identity-by-circumstance
// gotcha). It also verifies the legacy fallback: records without an ID still use
// CreatedAt, exactly as before, so mixed old/new records reconcile correctly.
func TestReconcileSnapshot_IdentityUsesStableID(t *testing.T) {
	t1 := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 4, 11, 0, 0, 0, time.UTC)

	// stubBuilder records swap builds and returns a fresh fake-backed instance
	// carrying the snapshot record's identity (no real tmux/worktree — the #961
	// reattach-flake lesson).
	stubBuilder := func(t *testing.T, built *[]session.InstanceData) func() {
		return SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
			*built = append(*built, d)
			inst, err := session.NewInstance(session.InstanceOptions{Title: d.Title, Path: t.TempDir(), Program: "test"})
			require.NoError(t, err)
			inst.SetBackend(session.NewFakeBackend())
			inst.SetStartedForTest(true)
			inst.ID = d.ID
			inst.CreatedAt = d.CreatedAt
			return inst, nil
		})
	}
	makeStored := func(t *testing.T, h *home, id, title string, created time.Time) *session.Instance {
		inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "test"})
		require.NoError(t, err)
		inst.SetBackend(session.NewFakeBackend())
		inst.SetStartedForTest(true)
		inst.SetStatusForTest(session.Running)
		inst.ID = id
		inst.CreatedAt = created
		h.store.AddInstance(inst)
		return inst
	}

	cases := []struct {
		name        string
		storedID    string
		storedAt    time.Time
		snapID      string
		snapAt      time.Time
		wantSwapped bool
	}{
		// ID authoritative: different ID is a title reuse even when CreatedAt matches.
		{"different id -> swap despite equal CreatedAt", "id-A", t1, "id-B", t1, true},
		// ID authoritative: same ID is the same session even when CreatedAt drifted.
		{"same id -> no swap despite differing CreatedAt", "id-A", t1, "id-A", t2, false},
		// Legacy fallback: no IDs on either side -> CreatedAt decides (prior behavior).
		{"legacy no ids, differing CreatedAt -> swap", "", t1, "", t2, true},
		{"legacy no ids, equal CreatedAt -> no swap", "", t1, "", t1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			var built []session.InstanceData
			restore := stubBuilder(t, &built)
			defer restore()

			stored := makeStored(t, h, tc.storedID, "worker", tc.storedAt)
			h.reconcileSnapshot([]session.InstanceData{
				{ID: tc.snapID, Title: "worker", CreatedAt: tc.snapAt, Status: session.Ready},
			})

			got := h.store.GetInstanceByTitle("worker")
			require.NotNil(t, got)
			if tc.wantSwapped {
				require.Len(t, built, 1, "a title reuse must rebuild (swap) the row")
				require.NotSame(t, stored, got, "swap must replace the stale pointer")
			} else {
				require.Empty(t, built, "same session must update in place, not swap")
				require.Same(t, stored, got, "same session must keep its pointer")
			}
		})
	}
}

// TestImportRemoteHookSessionsAddsListCmdSessions removed — the remote-hook
// enumeration/import path (list_cmd, SetRemoteImporterForTest,
// ListRemoteHookInstanceData, importRemoteHookSessions, RemoteMeta) was deleted
// in the provision-and-expose migration. // #1592 Phase 4 PR7

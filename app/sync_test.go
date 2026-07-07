package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionAutoYesAuthoritative is a regression test for issue #326.
//
// Previously the TUI loops only set instance.AutoYes = true when the
// session-level autoYes was true and never cleared it, so a prior
// `--auto-yes` run that persisted AutoYes=true would silently keep
// auto-accepting prompts in subsequent TUI runs without the flag.
//
// The fix synchronizes instance.AutoYes with the session-level autoYes
// in all TUI paths (loading instances, starting instances, and the
// snapshot reconcile that adds daemon-owned sessions). This test guards
// the load-instances path: it verifies that a persisted AutoYes=true is
// cleared when the session autoYes is false.
func TestSessionAutoYesAuthoritative(t *testing.T) {
	cases := []struct {
		name           string
		persistedValue bool
		sessionAutoYes bool
		want           bool
	}{
		{
			name:           "persisted true, session false -> false (issue #326)",
			persistedValue: true,
			sessionAutoYes: false,
			want:           false,
		},
		{
			name:           "persisted false, session true -> true",
			persistedValue: false,
			sessionAutoYes: true,
			want:           true,
		},
		{
			name:           "persisted true, session true -> true",
			persistedValue: true,
			sessionAutoYes: true,
			want:           true,
		},
		{
			name:           "persisted false, session false -> false",
			persistedValue: false,
			sessionAutoYes: false,
			want:           false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Mirror the load-instances loop in app.go: the session-level
			// autoYes must be authoritative over the persisted value.
			instances := []*session.Instance{{Title: "t", AutoYes: tc.persistedValue}}
			autoYes := tc.sessionAutoYes
			for _, instance := range instances {
				instance.SetAutoYes(autoYes)
			}
			if instances[0].AutoYes != tc.want {
				t.Fatalf("instance.AutoYes = %v; want %v", instances[0].AutoYes, tc.want)
			}
		})
	}
}

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
// reconcile even though the daemon liveness is applied. The daemon snapshot never
// carries the op, and a liveness write can't touch the separate op axis, so the
// composed status stays Deleting — replacing the old isTransientStatus skip.
func TestReconcileSnapshot_LeavesTransientOpAlone(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a")
	inst.SetInFlightOpForTest(session.OpKilling)
	h.store.AddInstance(inst)

	data := inst.ToInstanceData()
	data.Liveness = session.LiveReady // daemon doesn't know about the in-flight kill

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

func TestImportRemoteHookSessionsAddsListCmdSessions(t *testing.T) {
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)

	h := newTestHome(t)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID

	scriptDir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(scriptDir, name)
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0755))
		return path
	}

	listCmd := write("list.sh", `echo '[{"name": "remote-one", "status": "running", "host": "h1"}, {"name": "remote-two", "status": "stopped"}]'`)
	attachCmd := write("attach.sh", `echo "attached $1"`)
	noopCmd := write("noop.sh", `echo '{"ok": true}'`)
	require.NoError(t, config.SaveRepoConfig(repo.ID, &config.RepoConfig{
		RemoteHooks: &config.RemoteHooks{
			LaunchCmd: noopCmd,
			ListCmd:   listCmd,
			AttachCmd: attachCmd,
			DeleteCmd: noopCmd,
		},
	}))

	restoreImporter := SetRemoteImporterForTest(func(repoPath string) ([]session.InstanceData, error) {
		listed, err := session.ListRemoteHookInstanceData(repoPath, config.RemoteHooks{ListCmd: listCmd}, time.Now())
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(listed)
		if err != nil {
			return nil, err
		}
		if err := config.SaveRepoInstances(repo.ID, raw); err != nil {
			return nil, err
		}
		return listed, nil
	})
	t.Cleanup(restoreImporter)

	imported := h.importRemoteHookSessions()
	require.Equal(t, 1, imported)
	require.Equal(t, 1, h.store.NumInstances())

	inst := h.store.GetInstances()[0]
	require.True(t, inst.IsRemote())
	require.Equal(t, "remote-one", inst.Title)

	stored, err := config.LoadRepoInstances(repo.ID)
	require.NoError(t, err)
	var data []session.InstanceData
	require.NoError(t, json.Unmarshal(stored, &data))
	require.Len(t, data, 1)
	require.Equal(t, "remote-one", data[0].Title)
	require.Equal(t, "remote-one", data[0].RemoteMeta["name"])
	require.Equal(t, "h1", data[0].RemoteMeta["host"])
}

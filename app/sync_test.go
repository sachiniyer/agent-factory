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
	inst.SetStatus(session.Running)
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
	h.sidebar.AddInstance(inst)

	// The daemon's snapshot reports this session is now Dead. The TUI must adopt
	// it verbatim — it does not re-derive Ready/Dead from a local probe.
	data := inst.ToInstanceData()
	data.Status = session.Dead

	changed := h.reconcileSnapshot([]session.InstanceData{data})

	assert.True(t, changed, "mirroring a new status must report a change so the sidebar repaints")
	assert.Equal(t, session.Dead, inst.GetStatus(),
		"the TUI must render the daemon's snapshot status, not compute its own")
}

// TestReconcileSnapshot_LeavesTransientRowStatusAlone guards that a row the TUI
// owns mid-operation (Loading creation #808, Deleting kill #844) is not
// clobbered by the status mirror: the reconcile skips transient rows entirely.
func TestReconcileSnapshot_LeavesTransientRowStatusAlone(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a")
	inst.SetStatus(session.Deleting)
	h.sidebar.AddInstance(inst)

	data := inst.ToInstanceData()
	data.Status = session.Ready // daemon doesn't know about the in-flight kill

	h.reconcileSnapshot([]session.InstanceData{data})

	assert.Equal(t, session.Deleting, inst.GetStatus(),
		"a mid-teardown row must keep its Deleting marker through a reconcile")
}

func TestImportRemoteHookSessionsAddsListCmdSessions(t *testing.T) {
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)

	h := newTestHome(t)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	h.repoID = repo.ID
	h.storage, err = session.NewStorage(config.DefaultState(), repo.ID)
	require.NoError(t, err)

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
	require.Equal(t, 1, h.sidebar.NumInstances())

	inst := h.sidebar.GetInstances()[0]
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

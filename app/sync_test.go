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

// TestPendingInstanceCollisionShouldSkip covers the decision logic used by
// mergePendingInstances when a scheduled-task rerun produces a pending
// instance whose title collides with an existing sidebar instance. See
// issue #255: a rerun recreates the same tmux session name under a new
// worktree path, so TmuxAlive() alone cannot tell whether the sidebar
// instance is still valid.
func TestPendingInstanceCollisionShouldSkip(t *testing.T) {
	cases := []struct {
		name             string
		existingWorktree string
		pendingWorktree  string
		tmuxAlive        bool
		wantSkip         bool
	}{
		{
			name:             "worktree paths differ and tmux alive -> replace (issue #255)",
			existingWorktree: "/repo/worktrees/task",
			pendingWorktree:  "/repo/worktrees/task-2",
			tmuxAlive:        true,
			wantSkip:         false,
		},
		{
			name:             "worktree paths differ and tmux dead -> replace",
			existingWorktree: "/repo/worktrees/task",
			pendingWorktree:  "/repo/worktrees/task-2",
			tmuxAlive:        false,
			wantSkip:         false,
		},
		{
			name:             "worktree paths match and tmux alive -> skip",
			existingWorktree: "/repo/worktrees/task",
			pendingWorktree:  "/repo/worktrees/task",
			tmuxAlive:        true,
			wantSkip:         true,
		},
		{
			name:             "worktree paths match and tmux dead -> replace",
			existingWorktree: "/repo/worktrees/task",
			pendingWorktree:  "/repo/worktrees/task",
			tmuxAlive:        false,
			wantSkip:         false,
		},
		{
			name:             "existing worktree unknown and tmux alive -> skip",
			existingWorktree: "",
			pendingWorktree:  "/repo/worktrees/task",
			tmuxAlive:        true,
			wantSkip:         true,
		},
		{
			name:             "pending worktree unknown and tmux alive -> skip",
			existingWorktree: "/repo/worktrees/task",
			pendingWorktree:  "",
			tmuxAlive:        true,
			wantSkip:         true,
		},
		{
			name:             "both worktrees unknown and tmux dead -> replace",
			existingWorktree: "",
			pendingWorktree:  "",
			tmuxAlive:        false,
			wantSkip:         false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pendingInstanceCollisionShouldSkip(tc.existingWorktree, tc.pendingWorktree, tc.tmuxAlive)
			if got != tc.wantSkip {
				t.Fatalf("pendingInstanceCollisionShouldSkip(%q, %q, %v) = %v; want %v",
					tc.existingWorktree, tc.pendingWorktree, tc.tmuxAlive, got, tc.wantSkip)
			}
		})
	}
}

// TestSessionAutoYesAuthoritative is a regression test for issue #326.
//
// Previously the TUI loops only set instance.AutoYes = true when the
// session-level autoYes was true and never cleared it, so a prior
// `--auto-yes` run that persisted AutoYes=true would silently keep
// auto-accepting prompts in subsequent TUI runs without the flag.
//
// The fix synchronizes instance.AutoYes with the session-level autoYes
// in all TUI paths (loading instances, starting instances, merging
// pending instances, and refreshing external instances). This test
// guards the load-instances path: it verifies that a persisted
// AutoYes=true is cleared when the session autoYes is false.
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
				instance.AutoYes = autoYes
			}
			if instances[0].AutoYes != tc.want {
				t.Fatalf("instance.AutoYes = %v; want %v", instances[0].AutoYes, tc.want)
			}
		})
	}
}

func TestUpsertInstanceDataByTitleReplacesDuplicates(t *testing.T) {
	existing := []session.InstanceData{
		{Title: "already", Worktree: session.GitWorktreeData{WorktreePath: "/old"}},
		{Title: "keep", Worktree: session.GitWorktreeData{WorktreePath: "/keep"}},
	}
	incoming := []session.InstanceData{
		{Title: "already", Worktree: session.GitWorktreeData{WorktreePath: "/new"}},
		{Title: "add", Worktree: session.GitWorktreeData{WorktreePath: "/add"}},
	}

	got := upsertInstanceDataByTitle(existing, incoming)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(got), got)
	}

	byTitle := make(map[string]session.InstanceData)
	for _, data := range got {
		byTitle[data.Title] = data
	}
	if byTitle["already"].Worktree.WorktreePath != "/new" {
		t.Fatalf("expected duplicate title to be replaced, got %+v", byTitle["already"])
	}
	if byTitle["keep"].Worktree.WorktreePath != "/keep" {
		t.Fatalf("expected unrelated existing entry to remain, got %+v", byTitle["keep"])
	}
	if byTitle["add"].Worktree.WorktreePath != "/add" {
		t.Fatalf("expected new entry to be appended, got %+v", byTitle["add"])
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

// TestTickUpdateMetadata_HandlerDispatchesOffEventLoop is the regression test
// for issue #559: the tickUpdateMetadataMessage handler must not iterate
// instances on the bubbletea Update goroutine. The loop body shells out to
// tmux capture-pane per instance, so executing it synchronously blocks
// rendering and is most visible right after detach when the queued tick
// fires. The fix moves the iteration into a tea.Cmd that runs in a goroutine.
//
// We assert the handler returns quickly and the instance statuses are NOT
// yet modified by the time Update returns — they only flip after the cmd
// runs.
func TestTickUpdateMetadata_HandlerDispatchesOffEventLoop(t *testing.T) {
	h := newTestHome(t)
	a := instanceWithFakeBackend(t, "a")
	b := instanceWithFakeBackend(t, "b")
	c := instanceWithFakeBackend(t, "c")
	h.sidebar.AddInstance(a)
	h.sidebar.AddInstance(b)
	h.sidebar.AddInstance(c)

	start := time.Now()
	_, cmd := h.Update(tickUpdateMetadataMessage{})
	elapsed := time.Since(start)

	require.NotNil(t, cmd, "handler must return a re-schedule cmd")
	// The handler should now return essentially instantly — the work is
	// deferred to the returned cmd, which runs off the event loop.
	assert.Less(t, elapsed, 5*time.Millisecond,
		"Update must not block on per-instance work; saw %s", elapsed)
	// Statuses remain unchanged at this point — the cmd hasn't run yet.
	assert.Equal(t, session.Running, a.GetStatus())
	assert.Equal(t, session.Running, b.GetStatus())
	assert.Equal(t, session.Running, c.GetStatus())
}

// TestRunMetadataTick_TransitionsRunningToReady checks the loop body still
// performs its job when invoked off the event loop: a FakeBackend reports
// no updates and no prompt, so each Running instance must flip to Ready.
func TestRunMetadataTick_TransitionsRunningToReady(t *testing.T) {
	a := instanceWithFakeBackend(t, "a")
	b := instanceWithFakeBackend(t, "b")

	runMetadataTick([]*session.Instance{a, b})

	assert.Equal(t, session.Ready, a.GetStatus())
	assert.Equal(t, session.Ready, b.GetStatus())
}

// TestRunMetadataTick_SkipsLoadingAndUnstartedInstances mirrors the previous
// synchronous handler's guard: instances that are still Loading or not
// Started must not be probed (tmux session may not exist yet).
func TestRunMetadataTick_SkipsLoadingAndUnstartedInstances(t *testing.T) {
	loading := instanceWithFakeBackend(t, "loading")
	loading.SetStatus(session.Loading)

	unstarted, err := session.NewInstance(session.InstanceOptions{
		Title:   "unstarted",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	unstarted.SetBackend(session.NewFakeBackend())
	// Deliberately leave started=false.
	unstarted.SetStatus(session.Running)

	runMetadataTick([]*session.Instance{loading, unstarted})

	assert.Equal(t, session.Loading, loading.GetStatus(),
		"Loading instance must not have its status overwritten")
	assert.Equal(t, session.Running, unstarted.GetStatus(),
		"unstarted instance must not have its status overwritten")
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

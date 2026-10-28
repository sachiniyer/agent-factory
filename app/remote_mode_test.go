package app

import (
	"context"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

func stubRemote(t *testing.T) {
	t.Helper()
	prev := isRemoteTarget
	isRemoteTarget = func() bool { return true }
	t.Cleanup(func() { isRemoteTarget = prev })
	// A seeded local registry and tasks.json must never be read.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
}

func TestRemoteProjectListComesFromTheTargetsRegistry(t *testing.T) {
	stubRemote(t)
	failOnDial(t)
	m := &home{appConfig: &config.Config{RootAgents: map[string]config.RootAgentConfig{"/local/only": {}}}}
	m.remote.applyFresh(&remoteReads{projects: []config.Project{
		{ID: "prj_0123456789abcdef0123456789abcdef", Root: "/srv/box/alpha", RepoID: "aaaaaaaaaaaa", CheckoutID: "c1", PathExists: true},
		{ID: "prj_fedcba9876543210fedcba9876543210", Root: "/srv/box/beta", RepoID: "bbbbbbbbbbbb"},
	}})
	data := []session.InstanceData{{Worktree: session.GitWorktreeData{RepoPath: "/srv/box/alpha"}}}
	projects, degraded := m.buildProjectListFrom(data)
	require.False(t, degraded)
	require.Len(t, projects, 2, "a local root_agents key is not a project of the target")
	require.Equal(t, "alpha", projects[0].Name)
	require.Equal(t, "aaaaaaaaaaaa", projects[0].RepoID)
	require.Equal(t, 1, projects[0].SessionCount)
	require.Equal(t, "prj_0123456789abcdef0123456789abcdef", projects[0].RegistryID)
	require.Equal(t, "beta", projects[1].Name)
}

func TestRemoteProjectListKeepsStaleRowsAndReportsDegradedWhenTheTargetFails(t *testing.T) {
	stubRemote(t)
	failOnDial(t)
	m := &home{}
	m.remote.applyFresh(&remoteReads{projects: []config.Project{{ID: "prj_0123456789abcdef0123456789abcdef", Root: "/srv/box/alpha", RepoID: "aaaaaaaaaaaa"}}})
	m.remote.applyFresh(&remoteReads{projectsErr: errors.New("connection refused")})
	projects, degraded := m.buildProjectListFrom(nil)
	require.True(t, degraded, "a failed refresh is reported, not shown as an empty registry")
	require.Len(t, projects, 1, "the last good rows stay on screen")

	cold := &home{}
	_, degraded = cold.buildProjectListFrom(nil)
	require.True(t, degraded, "nothing read yet is not a complete registry either")
}

// A target that accepts the connection and never answers must cost the poll's
// goroutine a bounded wait, must leave no goroutine behind, and must cost the
// event loop nothing.
func TestRemoteHungTargetNeverBlocksTheEventLoop(t *testing.T) {
	stubRemote(t)
	t.Setenv("AF_DAEMON_URL", "http://box.example:8443")
	pp, pt, pto := remoteListProjects, remoteListTasks, remoteReadTimeout
	t.Cleanup(func() { remoteListProjects, remoteListTasks, remoteReadTimeout = pp, pt, pto })
	// A hung target: only the caller's context ends the read.
	remoteListProjects = func(ctx context.Context) ([]config.Project, error) { <-ctx.Done(); return nil, ctx.Err() }
	remoteListTasks = func(ctx context.Context) ([]task.Task, error) { <-ctx.Done(); return nil, ctx.Err() }
	remoteReadTimeout = 100 * time.Millisecond

	// The off-loop read gives up at the deadline and says why. It returns only
	// after both workers have, so nothing is left running past this line.
	start := time.Now()
	reads := fetchRemoteReads(context.Background())
	require.Less(t, time.Since(start), 2*time.Second)
	require.ErrorContains(t, reads.projectsErr, "the daemon at http://box.example:8443 did not answer within 100ms")
	require.ErrorContains(t, reads.tasksErr, "the daemon at http://box.example:8443 did not answer within 100ms")

	// The model's lifetime context also ends the read, well before the deadline.
	remoteReadTimeout = time.Minute // the owner cancel below, not this, must end the read
	owner, cancelOwner := context.WithCancel(context.Background())
	finished := make(chan *remoteReads, 1)
	go func() { finished <- fetchRemoteReads(owner) }()
	cancelOwner()
	select {
	case r := <-finished:
		require.Error(t, r.projectsErr)
	case <-time.After(5 * time.Second):
		t.Fatal("the read outlived the context of the model that owns it")
	}

	// Every loop-side consumer reads the cache and never dials, so none can wait
	// on the target (the stubs above would block until their context ends).
	//
	// The cache holds the timeout the poll delivered. The error a consumer returns
	// from it is that earlier, already-delivered one, handed back at once; the
	// timing assertion is what proves nothing here waited again.
	m := &home{repoID: "aaaaaaaaaaaa", repoRoot: "/srv/box/alpha"}
	m.remote.applyFresh(reads)
	loopStart := time.Now()
	_, degraded := m.buildProjectListFrom(nil)
	require.True(t, degraded)
	_, err := m.activeTasks(m.repoRoot, m.repoID)
	require.ErrorContains(t, err, "http://box.example:8443 did not answer", "the poll's timeout, from the cache")
	_, err = m.repoContextForSelection("/srv/box/alpha", "aaaaaaaaaaaa")
	require.NoError(t, err)
	require.Less(t, time.Since(loopStart), 250*time.Millisecond, "no loop-side call waited on the hung target")
}

// Every selection entry point resolves through the target's row, not local git.
func TestRemoteSelectionNeverProbesLocalGit(t *testing.T) {
	stubRemote(t)
	m := &home{sidebarProjects: []overlay.Project{{Root: "/srv/box/alpha", RepoID: "aaaaaaaaaaaa"}}}
	rc, err := m.repoContextForSelection("/srv/box/alpha", "") // sidebar Enter / mouse: root only
	require.NoError(t, err)
	require.Equal(t, "aaaaaaaaaaaa", rc.ID)
	rc, err = m.repoContextForSelection("/srv/box/alpha", "aaaaaaaaaaaa") // picker
	require.NoError(t, err)
	require.Equal(t, "/srv/box/alpha", rc.Root)
	_, err = m.repoContextForSelection("/srv/box/unknown", "")
	require.ErrorContains(t, err, "not in the daemon's project list")
}

func TestRemoteTasksAreScopedToTheProjectFromTheCache(t *testing.T) {
	stubRemote(t)
	failOnDial(t)
	m := &home{}
	_, err := m.activeTasks("/srv/box/alpha", "aaaaaaaaaaaa")
	require.Error(t, err, "nothing read yet is an error, never the local tasks.json or an empty list")
	m.remote.applyFresh(&remoteReads{tasks: []task.Task{
		{ID: "t1", RepoID: "aaaaaaaaaaaa"},
		{ID: "t2", RepoID: "bbbbbbbbbbbb"},
		{ID: "t3", ProjectPath: "/srv/box/alpha"},
		{ID: "t4", ProjectPath: "/srv/box/alpha/sub"},
		{ID: "t5"},
	}})
	got, err := m.activeTasks("/srv/box/alpha", "aaaaaaaaaaaa")
	require.NoError(t, err)
	require.Equal(t, []string{"t1", "t3"}, remoteTaskIDs(got))
	none, err := m.activeTasks("", "")
	require.NoError(t, err)
	require.Empty(t, none, "registry mode has no active task scope")
}

func TestRemoteRepoContextIsBuiltFromTheRowNotALocalProbe(t *testing.T) {
	rc := remoteRepoContext("/srv/box/alpha", "aaaaaaaaaaaa")
	require.Equal(t, "/srv/box/alpha", rc.WorkspacePath())
	require.Equal(t, "aaaaaaaaaaaa", rc.ID)
}

func TestRemoteHooksSaveIsRefusedNotWrittenLocally(t *testing.T) {
	stubRemote(t)
	require.ErrorContains(t, errRemoteHooksSave(), "edit .agent-factory/config.toml there")
}

func failOnDial(t *testing.T) {
	t.Helper()
	pp, pt := remoteListProjects, remoteListTasks
	remoteListProjects = func(context.Context) ([]config.Project, error) {
		t.Error("event loop dialed the target for projects")
		return nil, nil
	}
	remoteListTasks = func(context.Context) ([]task.Task, error) {
		t.Error("event loop dialed the target for tasks")
		return nil, nil
	}
	t.Cleanup(func() { remoteListProjects, remoteListTasks = pp, pt })
}

func remoteTaskIDs(ts []task.Task) []string {
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		ids = append(ids, t.ID)
	}
	return ids
}

// applyFresh installs reads as a poll that began after every mutation so far.
func (c *remoteCache) applyFresh(r *remoteReads) { c.apply(r, c.seq) }

// #5858: a mutation's own result updates the cache at once, and a poll that began
// before it can never resurrect the older state.
func TestRemoteTaskMutationsUpdateTheCacheAndOutrankOlderPolls(t *testing.T) {
	stubRemote(t)
	failOnDial(t)
	m := &home{repoID: "aaaaaaaaaaaa", repoRoot: "/srv/box/alpha"}
	m.remote.applyFresh(&remoteReads{tasks: []task.Task{
		{ID: "keep", Name: "keep", RepoID: "aaaaaaaaaaaa"},
		{ID: "edit", Name: "old", RepoID: "aaaaaaaaaaaa"},
		{ID: "gone", Name: "gone", RepoID: "aaaaaaaaaaaa"},
	}})
	// A poll starts now, before any mutation.
	pollSeq := m.remote.seq
	staleRead := &remoteReads{tasks: []task.Task{
		{ID: "keep", Name: "keep", RepoID: "aaaaaaaaaaaa"},
		{ID: "edit", Name: "old", RepoID: "aaaaaaaaaaaa"},
		{ID: "gone", Name: "gone", RepoID: "aaaaaaaaaaaa"},
	}}

	// create (the daemon returns the stored record, bound to the project even
	// though the typed path was a ~ spelling), edit, delete — all acknowledged.
	taskAck = taskMutationAck{record: &task.Task{ID: "new", Name: "new", RepoID: "aaaaaaaaaaaa", ProjectPath: "/srv/box/alpha"}}
	m.noteRemoteCreate(task.Task{ID: "new", Name: "new", ProjectPath: "~/alpha"}, nil)
	taskAck = taskMutationAck{record: &task.Task{ID: "edit", Name: "renamed", RepoID: "aaaaaaaaaaaa"}}
	m.noteRemoteUpdate("edit", task.TaskUpdate{}, nil)
	m.noteRemoteRemove("gone", nil)

	got, err := m.activeTasks(m.repoRoot, m.repoID)
	require.NoError(t, err)
	byID := map[string]task.Task{}
	for _, tk := range got {
		byID[tk.ID] = tk
	}
	require.Contains(t, byID, "new", "a create is in the project's list at once")
	require.Equal(t, "renamed", byID["edit"].Name, "an edit does not snap back")
	require.NotContains(t, byID, "gone", "a delete does not reappear")

	// The poll that began before the mutations lands late: its tasks are refused.
	m.remote.apply(staleRead, pollSeq)
	got, _ = m.activeTasks(m.repoRoot, m.repoID)
	require.Len(t, got, 3, "a stale poll must not resurrect state older than an acknowledged mutation")
	require.NotContains(t, remoteTaskIDs(got), "gone")

	// A poll that began after the acknowledgement is authoritative again.
	m.remote.apply(&remoteReads{tasks: []task.Task{{ID: "other", RepoID: "aaaaaaaaaaaa"}}}, m.remote.seq)
	got, _ = m.activeTasks(m.repoRoot, m.repoID)
	require.Equal(t, []string{"other"}, remoteTaskIDs(got))
}

func TestRemoteTaskMutationOutcomes(t *testing.T) {
	stubRemote(t)
	m := &home{}
	m.remote.applyFresh(&remoteReads{tasks: []task.Task{{ID: "a", RepoID: "r"}}})
	// A definitive refusal changes nothing and does not poison in-flight polls.
	seq := m.remote.seq
	m.noteRemoteCreate(task.Task{ID: "b"}, errors.New("daemon said no"))
	require.Equal(t, seq, m.remote.seq)
	require.Equal(t, []string{"a"}, remoteTaskIDs(m.remote.tasks))
	// With no returned record, the sent task is what the cache advances to; an
	// update falls back to applying its patch to the cached copy.
	m.noteRemoteCreate(task.Task{ID: "b", RepoID: "r"}, nil)
	require.ElementsMatch(t, []string{"a", "b"}, remoteTaskIDs(m.remote.tasks))
	name := "renamed"
	m.noteRemoteUpdate("a", task.TaskUpdate{Name: &name}, nil)
	for _, tk := range m.remote.tasks {
		if tk.ID == "a" {
			require.Equal(t, "renamed", tk.Name)
		}
	}
}

func TestRemoteTasksBeforeTheFirstReadIsAnErrorNotAnEmptyList(t *testing.T) {
	stubRemote(t)
	m := &home{}
	_, err := m.activeTasks("/srv/box/alpha", "aaaaaaaaaaaa")
	require.ErrorContains(t, err, "has not been read")
}

// #5860: a hook edit that can never be saved must not trap the user.
func TestRemoteHooksAreReadOnlyAndAStraySaveDiscardsInsteadOfWedging(t *testing.T) {
	h := dirtyHooksHome(t) // a dirty hooks pane (local), as a stray edit would leave it
	stubRemote(t)
	hp := h.hooksPane
	require.True(t, hp.IsDirty())

	// A save that cannot succeed remotely discards the edit, says so, and returns
	// no error: quit and the task actions behind it are not blocked.
	require.NoError(t, h.saveContentPaneState())
	require.False(t, hp.IsDirty(), "the unsaveable edit is dropped, not kept dirty forever")
	require.NotContains(t, hp.GetCommands(), "echo test")
	require.Contains(t, h.takePendingPaneNotice(), "unsaved hook edit was discarded")

	// And from here the pane refuses edits up front.
	hp.SetReadOnly(remoteHooksReadOnlyReason())
	hp.SetFocus(true)
	for _, k := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("n")}, {Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune("D")}} {
		hp.HandleKeyPress(k)
		require.False(t, hp.IsDirty())
		require.False(t, hp.IsEditing())
	}
}

// Any save that keeps failing: the first quit says why and aborts, the next one
// discards the unsaved edits and leaves.
func TestQuitAfterARepeatedSaveFailureDiscardsAndLeaves(t *testing.T) {
	h := dirtyHooksHome(t)
	stubHooksSaveFailure(t)

	_, _ = h.handleQuit()
	require.False(t, h.quitting, "the first quit is refused with the reason")
	require.True(t, h.hooksPane.IsDirty(), "and the edit is kept for the user to decide")

	_, _ = h.handleQuit()
	require.True(t, h.quitting, "the same failure again is the user choosing to leave")
	require.False(t, h.hooksPane.IsDirty())
}

package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func archiveTargetTask(id, name, repoPath, target string, enabled bool) task.Task {
	return task.Task{
		ID: id, Name: name, Prompt: "run it", CronExpr: "*/15 * * * *",
		ProjectPath: repoPath, TargetSession: target, Enabled: enabled, CreatedAt: time.Now(),
	}
}

func archiveTaskControlServer(manager *Manager) *controlServer {
	return &controlServer{manager: manager, scheduler: newTaskScheduler()}
}

// TestArchiveSession_RefusesEnabledTargetTasksBeforeMutation pins the lifecycle
// policy for #2646: archiving is refused once, with every blocking automation
// named, rather than succeeding and leaving those tasks in a permanent retry
// loop. Disabled tasks and enabled tasks aimed elsewhere are not blockers.
func TestArchiveSession_RefusesEnabledTargetTasksBeforeMutation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, source := registerArchivable(t, manager, repoID, repoPath, "worker")
	otherRepo := setupTaskRepo(t)

	require.NoError(t, task.AddTask(archiveTargetTask("block001", "Fleet Sweep", repoPath, "worker", true)))
	require.NoError(t, task.AddTask(archiveTargetTask("block002", "Health Watch", repoPath, "worker", true)))
	require.NoError(t, task.AddTask(archiveTargetTask("disabled1", "Paused Task", repoPath, "worker", false)))
	require.NoError(t, task.AddTask(archiveTargetTask("else0001", "Other Target", repoPath, "captain", true)))
	require.NoError(t, task.AddTask(archiveTargetTask("other001", "Other Project", otherRepo, "worker", true)))

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err, "archive must reject enabled tasks that target the session")
	for _, want := range []string{"Fleet Sweep", "block001", "Health Watch", "block002", "disable or retarget"} {
		assert.Contains(t, err.Error(), want)
	}
	for _, absent := range []string{"Paused Task", "disabled1", "Other Target", "else0001", "Other Project", "other001"} {
		assert.NotContains(t, err.Error(), absent)
	}
	assert.Equal(t, session.LiveReady, inst.GetLiveness(), "rejection must precede the archive fence")
	_, statErr := os.Stat(source)
	assert.NoError(t, statErr, "rejection must not move the worktree")
}

func TestArchiveSession_PublishesLegacyBindingBackfill(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")
	legacy := archiveTargetTask("legacy03", "Legacy Binding", repoPath, "captain", true)
	raw, err := json.Marshal([]task.Task{legacy})
	require.NoError(t, err)
	tasksPath, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tasksPath, raw, 0o600))
	_, events := manager.events.subscribe()

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	var got []agentproto.Event
	for {
		select {
		case event := <-events:
			got = append(got, event)
		default:
			goto drained
		}
	}
drained:
	require.Len(t, got, 2, "the binding commit and archive commit must both reach push-only clients")
	require.Equal(t, agentproto.EventTaskUpdated, got[0].Type,
		"the durable binding projection must publish before the lifecycle event")
	var bound task.Task
	require.NoError(t, json.Unmarshal(got[0].Data, &bound))
	require.Equal(t, legacy.ID, bound.ID)
	require.Equal(t, repoID, bound.RepoID)
	require.Equal(t, agentproto.EventSessionArchived, got[1].Type)
}

// TestArchiveSession_PublishesBindingBackfillBeforeScopeRefusal covers the
// partial-success load: one legacy row's binding is durably committed, then a
// second enabled targeted row with an unresolvable path leaves the scope decision
// unknown. That commit already happened, so its projection must still reach
// push-only clients before the error propagates — otherwise they keep a stale
// repository scope for a row the server has already made authoritative, and no
// later event corrects it.
func TestArchiveSession_PublishesBindingBackfillBeforeScopeRefusal(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")
	resolvable := archiveTargetTask("legacy04", "Legacy Binding", repoPath, "captain", true)
	unresolvable := archiveTargetTask("legacy05", "Unknown Binding", filepath.Join(t.TempDir(), "missing"), "worker", true)
	raw, err := json.Marshal([]task.Task{resolvable, unresolvable})
	require.NoError(t, err)
	tasksPath, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tasksPath, raw, 0o600))
	_, events := manager.events.subscribe()

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err, "an unresolvable enabled legacy binding leaves the blocker set unknown")
	assert.Contains(t, err.Error(), "could not determine")

	var got []agentproto.Event
	for {
		select {
		case event := <-events:
			got = append(got, event)
		default:
			goto drained
		}
	}
drained:
	require.Len(t, got, 1, "the durably committed binding must publish even though the load then refused")
	require.Equal(t, agentproto.EventTaskUpdated, got[0].Type)
	var bound task.Task
	require.NoError(t, json.Unmarshal(got[0].Data, &bound))
	assert.Equal(t, resolvable.ID, bound.ID)
	assert.Equal(t, repoID, bound.RepoID, "the published projection carries the committed identity")
}

// TestArchiveSession_TaskStoreReadFailureLeavesSessionIntact preserves the
// three-valued outcome: an unreadable tasks store means blockers could not be
// determined, not that none exist. Fail before teardown so repair + retry is
// safe and no automation relationship is guessed away.
func TestArchiveSession_TaskStoreReadFailureLeavesSessionIntact(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, source := registerArchivable(t, manager, repoID, repoPath, "worker")

	tasksPath, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tasksPath, []byte("{ not json"), 0o600))

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "task") && strings.Contains(err.Error(), "determine"),
		"error must name the unknown task relationship, got: %v", err)
	assert.Equal(t, session.LiveReady, inst.GetLiveness())
	_, statErr := os.Stat(source)
	assert.NoError(t, statErr, "unknown task state must not move the worktree")
}

// TestArchiveSession_TaskMutationCannotCrossArchiveFence reproduces the exact
// review race: an AddTask lands after the blocker snapshot but before the
// archive fence. Both operations cannot succeed, or the enabled task is left
// targeting an archived session.
func TestArchiveSession_TaskMutationCannotCrossArchiveFence(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")
	server := archiveTaskControlServer(manager)

	checked := make(chan struct{})
	resume := make(chan struct{})
	orig := archiveTargetTasksChecked
	var once sync.Once
	archiveTargetTasksChecked = func() {
		once.Do(func() { close(checked) })
		<-resume
	}
	t.Cleanup(func() { archiveTargetTasksChecked = orig })

	archiveDone := make(chan error, 1)
	go func() {
		_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
		archiveDone <- err
	}()
	<-checked

	addStarted := make(chan struct{})
	addDone := make(chan error, 1)
	go func() {
		close(addStarted)
		addDone <- server.AddTask(AddTaskRequest{Task: archiveTargetTask(
			"race0001", "Racing Task", repoPath, "worker", true,
		)}, &AddTaskResponse{})
	}()
	<-addStarted
	close(resume)

	archiveErr := <-archiveDone
	addErr := <-addDone
	require.NoError(t, archiveErr, "archive reached preflight first and must win the serialized race")
	require.Error(t, addErr, "the later task mutation must observe the archive fence")
	assert.Contains(t, addErr.Error(), "archiv")
	_, getErr := task.GetTask("race0001")
	require.Error(t, getErr, "the losing task mutation must not commit")
}

// TestTaskMutations_RefuseArchivedTarget pins the other side of the fence:
// once archive wins, later add, enable, and retarget writes must all fail rather
// than create the same permanent delivery loop after the fact.
func TestTaskMutations_RefuseArchivedTarget(t *testing.T) {
	tests := []struct {
		name string
		seed task.Task
		act  func(*controlServer, string) error
	}{
		{
			name: "add",
			act: func(server *controlServer, repoPath string) error {
				return server.AddTask(AddTaskRequest{Task: archiveTargetTask(
					"after001", "After Archive", repoPath, "worker", true,
				)}, &AddTaskResponse{})
			},
		},
		{
			name: "enable",
			seed: archiveTargetTask("enable01", "Enable Later", "", "worker", false),
			act: func(server *controlServer, _ string) error {
				enabled := true
				return server.UpdateTask(UpdateTaskRequest{ID: "enable01", Update: task.TaskUpdate{Enabled: &enabled}}, &UpdateTaskResponse{})
			},
		},
		{
			name: "retarget",
			seed: archiveTargetTask("target01", "Retarget Later", "", "captain", true),
			act: func(server *controlServer, _ string) error {
				target := "worker"
				return server.UpdateTask(UpdateTaskRequest{ID: "target01", Update: task.TaskUpdate{TargetSession: &target}}, &UpdateTaskResponse{})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			registerArchivable(t, manager, repoID, repoPath, "worker")
			if tc.seed.ID != "" {
				tc.seed.ProjectPath = repoPath
				require.NoError(t, task.AddTask(tc.seed))
			}
			_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
			require.NoError(t, err)
			before, loadErr := task.LoadTasks()
			require.NoError(t, loadErr)

			err = tc.act(archiveTaskControlServer(manager), repoPath)
			require.Error(t, err, "an enabled task must not be written against an archived target")
			assert.Contains(t, err.Error(), "archiv")
			after, loadErr := task.LoadTasks()
			require.NoError(t, loadErr)
			assert.Equal(t, before, after, "rejected mutation must leave the task store byte-semantically unchanged")
		})
	}
}

// TestTaskMutations_UnrelatedEditToleratesExistingArchivedTarget preserves
// field-level update tolerance across upgrades. Older daemons allowed an
// enabled task to survive after its target was archived; editing its name does
// not create or worsen that relationship, so it must remain possible while an
// enable, retarget, or project rebind is still rejected above.
func TestTaskMutations_UnrelatedEditToleratesExistingArchivedTarget(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	legacyUnsafe := archiveTargetTask("oldstate", "Old Name", repoPath, "worker", true)
	require.NoError(t, task.AddTask(legacyUnsafe), "seed the state accepted before lifecycle validation existed")
	newName := "New Name"
	var resp UpdateTaskResponse
	server := archiveTaskControlServer(manager)
	err = server.UpdateTask(UpdateTaskRequest{
		ID: legacyUnsafe.ID, Update: task.TaskUpdate{Name: &newName},
	}, &resp)
	require.NoError(t, err, "an unrelated patch must not be blocked by a pre-existing invalid target")
	assert.Equal(t, newName, resp.Task.Name)
	assert.True(t, resp.Task.Enabled)
	assert.Equal(t, "worker", resp.Task.TargetSession)
	assert.NotContains(t, server.scheduler.scheduledTaskIDs(), legacyUnsafe.ID,
		"tolerating the edit must not re-arm the pre-existing permanent failure")
}

// TestTaskMutations_TargetWriteFailsClosedDuringWarmup pins the persisted-state
// side of the archive fence. A task write that depends on target liveness may
// not treat the manager's still-empty restore map as proof that the target is
// absent. Untargeted task writes retain their historical warm-up behavior.
func TestTaskMutations_TargetWriteFailsClosedDuringWarmup(t *testing.T) {
	tests := []struct {
		name string
		act  func(*controlServer, string) error
	}{
		{
			name: "add",
			act: func(server *controlServer, repoPath string) error {
				return server.AddTask(AddTaskRequest{Task: archiveTargetTask(
					"warmadd1", "Warm Add", repoPath, "worker", true,
				)}, &AddTaskResponse{})
			},
		},
		{
			name: "enable",
			act: func(server *controlServer, _ string) error {
				enabled := true
				return server.UpdateTask(UpdateTaskRequest{
					ID: "warmup01", Update: task.TaskUpdate{Enabled: &enabled},
				}, &UpdateTaskResponse{})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			live, repoID, repoPath := newStatusTestManager(t)
			registerArchivable(t, live, repoID, repoPath, "worker")
			if tc.name == "enable" {
				require.NoError(t, task.AddTask(archiveTargetTask(
					"warmup01", "Warm Enable", repoPath, "worker", false,
				)))
			}
			_, _, err := live.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
			require.NoError(t, err)

			warming, err := newManagerShell(config.DefaultConfig())
			require.NoError(t, err)
			require.False(t, warming.Ready(), "precondition: persisted sessions have not been restored")
			before, loadErr := task.LoadTasks()
			require.NoError(t, loadErr)

			err = tc.act(archiveTaskControlServer(warming), repoPath)
			require.Error(t, err, "target-dependent mutation must not infer absence from an unrestored manager")
			assert.Contains(t, err.Error(), "starting")
			after, loadErr := task.LoadTasks()
			require.NoError(t, loadErr)
			assert.Equal(t, before, after, "unknown target state must leave the task store unchanged")
		})
	}
}

// TestTaskMutations_RefusePersistedUnloadedTarget covers the ready-manager
// counterpart to the warm-up test. A persisted row can fail to materialize
// because its worktree/backend metadata is broken; absence from m.instances is
// then unknown, not proof the target is available for a new enabled task.
func TestTaskMutations_RefusePersistedUnloadedTarget(t *testing.T) {
	states := []struct {
		name      string
		status    session.Status
		liveness  session.Liveness
		wantError string
	}{
		{name: "archived", status: session.Archived, liveness: session.LiveArchived, wantError: "archiv"},
		{name: "unavailable", status: session.Ready, liveness: session.LiveReady, wantError: "could not determine"},
	}
	actions := []struct {
		name string
		seed func(string) error
		act  func(*controlServer, string) error
	}{
		{
			name: "add",
			seed: func(string) error { return nil },
			act: func(server *controlServer, repoPath string) error {
				return server.AddTask(AddTaskRequest{Task: archiveTargetTask(
					"ghostadd", "Ghost Add", repoPath, "worker", true,
				)}, &AddTaskResponse{})
			},
		},
		{
			name: "enable",
			seed: func(repoPath string) error {
				return task.AddTask(archiveTargetTask("ghostenb", "Ghost Enable", repoPath, "worker", false))
			},
			act: func(server *controlServer, _ string) error {
				enabled := true
				return server.UpdateTask(UpdateTaskRequest{
					ID: "ghostenb", Update: task.TaskUpdate{Enabled: &enabled},
				}, &UpdateTaskResponse{})
			},
		},
	}

	for _, state := range states {
		for _, action := range actions {
			t.Run(state.name+"/"+action.name, func(t *testing.T) {
				t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
				repoPath := setupControlRepo(t)
				repo, err := config.RepoFromPath(repoPath)
				require.NoError(t, err)
				require.NoError(t, action.seed(repoPath))
				require.NoError(t, appendInstanceData(repo.ID, session.InstanceData{
					ID: "ghost-session", Title: "worker", Path: repoPath, Program: "claude",
					Status: state.status, Liveness: state.liveness, BackendType: "local",
				}))
				failLoadFor(t, "worker")

				manager, err := NewManager(config.DefaultConfig())
				require.NoError(t, err)
				require.True(t, manager.Ready())
				manager.mu.Lock()
				_, loaded := manager.instances[daemonInstanceKey(repo.ID, "worker")]
				manager.mu.Unlock()
				require.False(t, loaded, "precondition: persisted target must have failed to materialize")
				before, err := task.LoadTasks()
				require.NoError(t, err)

				err = action.act(archiveTaskControlServer(manager), repoPath)
				require.Error(t, err, "persisted target absence from memory must not be accepted as a missing target")
				assert.Contains(t, err.Error(), state.wantError)
				after, loadErr := task.LoadTasks()
				require.NoError(t, loadErr)
				assert.Equal(t, before, after, "refused target mutation must not commit")
			})
		}
	}
}

// TestTaskMutations_LegacyTaskScopeStillValidatesTarget covers tasks written
// before RepoID was retained. Enabling one without also patching ProjectPath
// must resolve its existing path before checking the archived target.
func TestTaskMutations_LegacyTaskScopeStillValidatesTarget(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	legacy := archiveTargetTask("legacy01", "Legacy Enable", repoPath, "worker", false)
	require.Empty(t, legacy.RepoID)
	raw, err := json.Marshal([]task.Task{legacy})
	require.NoError(t, err)
	tasksPath, err := task.MigrateOnLoadPath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tasksPath, raw, 0o600))

	enabled := true
	err = archiveTaskControlServer(manager).UpdateTask(UpdateTaskRequest{
		ID: legacy.ID, Update: task.TaskUpdate{Enabled: &enabled},
	}, &UpdateTaskResponse{})
	require.Error(t, err, "legacy empty RepoID must not bypass archived-target validation")
	assert.Contains(t, err.Error(), "archiv")
	stored, loadErr := task.GetTask(legacy.ID)
	require.NoError(t, loadErr)
	assert.False(t, stored.Enabled, "rejected legacy update must not commit")
}

// TestDeleteProject_TaskBlockerIsPreflight preserves the project's lifecycle
// configuration when a predictable blocker makes deletion impossible. The
// root-agent opt-in and in-memory respawn policy must be unchanged.
func TestDeleteProject_TaskBlockerIsPreflight(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, source := registerArchivable(t, manager, repoID, repoPath, "worker")
	require.NoError(t, task.AddTask(archiveTargetTask("delete01", "Deletion Blocker", repoPath, "worker", true)))

	seed := config.DefaultConfig()
	seed.RootAgents = map[string]config.RootAgentConfig{repoPath: {}}
	require.NoError(t, config.SaveConfig(seed))

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.Error(t, err)
	assert.Empty(t, result.Archived)
	assert.Contains(t, err.Error(), "Deletion Blocker")

	cfg, loadErr := config.LoadConfig()
	require.NoError(t, loadErr)
	assert.Contains(t, cfg.RootAgents, repoPath, "task refusal must precede durable root-agent removal")
	manager.mu.Lock()
	_, suppressed := manager.deletedRootRepos[repoID]
	manager.mu.Unlock()
	assert.False(t, suppressed, "task refusal must precede in-memory root-agent suppression")
	assert.Equal(t, session.LiveReady, inst.GetLiveness())
	_, statErr := os.Stat(source)
	assert.NoError(t, statErr)
}

// TestDeleteProject_FencesCreateAfterTaskPreflight reproduces the gap where an
// enabled task targets a currently missing session and a create reserves that
// title after DeleteProject's blocker snapshot but before its first mutation.
// Admission must lose once deletion owns the repo lifecycle fence.
func TestDeleteProject_FencesCreateAfterTaskPreflight(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	require.NoError(t, task.AddTask(archiveTargetTask(
		"create01", "Create Race", repoPath, "worker", true,
	)))

	mutationReached := make(chan struct{})
	resumeDelete := make(chan struct{})
	orig := deregisterRootAgents
	deregisterRootAgents = func(string) ([]string, error) {
		close(mutationReached)
		<-resumeDelete
		return nil, errors.New("forced stop before mutation")
	}
	t.Cleanup(func() { deregisterRootAgents = orig })

	deleteDone := make(chan error, 1)
	go func() {
		_, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
		deleteDone <- err
	}()
	select {
	case <-mutationReached:
	case <-time.After(5 * time.Second):
		close(resumeDelete)
		t.Fatal("DeleteProject did not reach its first mutation")
	}

	_, _, release, _, reserveErr := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath, Title: "worker", Program: "claude",
	})
	if reserveErr == nil {
		release()
	}
	close(resumeDelete)
	require.Error(t, <-deleteDone, "test seam stops deletion before any real mutation")
	require.Error(t, reserveErr, "session creation must not cross an active project-deletion fence")
	assert.Contains(t, reserveErr.Error(), "delet")
	_, _, retryRelease, _, retryErr := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath, Title: "worker", Program: "claude",
	})
	require.NoError(t, retryErr, "a failed deletion must release its short-lived create fence")
	retryRelease()
}

// TestDeleteProject_RefusesCreateAlreadyReservedBeforeFence covers the other
// ordering edge. A create that has reserved its title but has not yet published
// pendingCreates is still in flight, so deletion must refuse without mutation.
func TestDeleteProject_RefusesCreateAlreadyReservedBeforeFence(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	_, _, release, _, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath, Title: "worker", Program: "claude",
	})
	require.NoError(t, err)
	defer release()

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.Error(t, err, "an admitted create must be visible before its pending projection is published")
	assert.Empty(t, result.Archived)
	assert.Contains(t, err.Error(), "still starting")
	assert.Contains(t, err.Error(), "worker")
}

func TestProjectDeleteAndRestoreAdmissionAreMutuallyExclusive(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *Manager, string, string) *session.Instance
		restore func(*Manager, string) error
	}{
		{
			name: "archived restore",
			prepare: func(t *testing.T, manager *Manager, repoID, repoPath string) *session.Instance {
				inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
				inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
				_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
				require.NoError(t, err)
				return inst
			},
			restore: func(manager *Manager, repoID string) error {
				_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "worker", RepoID: repoID})
				return err
			},
		},
		{
			name: "lost restore",
			prepare: func(t *testing.T, manager *Manager, repoID, repoPath string) *session.Instance {
				backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
				return registerStarted(t, manager, repoID, repoPath, "worker", backend, true, session.Lost)
			},
			restore: func(manager *Manager, repoID string) error {
				_, _, err := manager.RestoreSession(RestoreSessionRequest{Title: "worker", RepoID: repoID})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			inst := tc.prepare(t, manager, repoID, repoPath)
			before := inst.LifecycleView()

			mutationReached := make(chan struct{})
			resumeDelete := make(chan struct{})
			orig := deregisterRootAgents
			deregisterRootAgents = func(string) ([]string, error) {
				close(mutationReached)
				<-resumeDelete
				return nil, errors.New("forced stop before mutation")
			}
			t.Cleanup(func() { deregisterRootAgents = orig })

			deleteDone := make(chan error, 1)
			go func() {
				_, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
				deleteDone <- err
			}()
			select {
			case <-mutationReached:
			case <-time.After(5 * time.Second):
				close(resumeDelete)
				t.Fatal("DeleteProject did not reach its first mutation")
			}

			restoreErr := tc.restore(manager, repoID)
			close(resumeDelete)
			require.Error(t, <-deleteDone)
			require.Error(t, restoreErr, "restore must not cross an active project-deletion fence")
			assert.Contains(t, restoreErr.Error(), "delet")
			assert.Equal(t, before, inst.LifecycleView(), "refused restore must not mutate the session")
		})
	}
}

func TestDeleteProject_RefusesClaimedArchivedRestore(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	key := daemonInstanceKey(repoID, "worker")
	require.NoError(t, manager.claimRestoreOperation(repoID, key, "worker"))
	t.Cleanup(func() { manager.releaseRestoreOperation(key) })

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.Error(t, err, "a restore claim made before deletion must block deletion before mutation")
	assert.Empty(t, result.Archived)
	assert.Contains(t, err.Error(), "worker")
	assert.Equal(t, session.LiveArchived, inst.GetLiveness())
}

func TestDeleteProject_TargetedExternalSessionsArePreflightBlockers(t *testing.T) {
	for _, title := range []string{session.RootSessionTitle, "inplace"} {
		t.Run(title, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, repoPath, title, "master", "", true, false)
			require.NoError(t, err)
			inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
			require.NoError(t, err)
			inst.SetBackend(session.NewFakeBackend())
			inst.SetGitWorktreeForTest(gw)
			inst.SetStartedForTest(true)
			inst.SetStatusForTest(session.Ready)
			manager.mu.Lock()
			manager.instances[daemonInstanceKey(repoID, title)] = inst
			manager.mu.Unlock()
			require.NoError(t, manager.SaveInstances())
			require.NoError(t, task.AddTask(archiveTargetTask("external1", "External Target", repoPath, title, true)))

			seed := config.DefaultConfig()
			seed.RootAgents = map[string]config.RootAgentConfig{repoPath: {}}
			require.NoError(t, config.SaveConfig(seed))

			result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
			require.Error(t, err, "deletion must not silently remove an enabled task target")
			assert.Empty(t, result.Killed)
			assert.Contains(t, err.Error(), "External Target")
			cfg, loadErr := config.LoadConfig()
			require.NoError(t, loadErr)
			assert.Contains(t, cfg.RootAgents, repoPath, "blocker must precede root-agent mutation")
			manager.mu.Lock()
			_, stillTracked := manager.instances[daemonInstanceKey(repoID, title)]
			manager.mu.Unlock()
			assert.True(t, stillTracked)
		})
	}
}

func TestDeleteProject_AbsentConfiguredRootTargetIsPreflightBlocker(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	cfg := config.DefaultConfig()
	cfg.RootAgents = map[string]config.RootAgentConfig{repoPath: {}}
	require.NoError(t, config.SaveConfig(cfg))
	manager, err := NewManager(cfg)
	require.NoError(t, err)
	require.True(t, manager.repoRootAgentWillMaterialize(repo.ID))
	manager.mu.Lock()
	_, rootLoaded := manager.instances[daemonInstanceKey(repo.ID, session.RootSessionTitle)]
	manager.mu.Unlock()
	require.False(t, rootLoaded, "precondition: configured root is momentarily absent")
	require.NoError(t, task.AddTask(archiveTargetTask(
		"rootgone", "Absent Root Target", repoPath, session.RootSessionTitle, true,
	)))

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repo.ID, RepoPath: repoPath})
	require.Error(t, err, "deletion must not strand a task behind the reserved root title")
	assert.Empty(t, result.Archived)
	assert.Contains(t, err.Error(), "Absent Root Target")
	stored, loadErr := config.LoadConfig()
	require.NoError(t, loadErr)
	assert.Contains(t, stored.RootAgents, repoPath, "blocker must precede root-agent config removal")
	manager.mu.Lock()
	_, suppressed := manager.deletedRootRepos[repo.ID]
	manager.mu.Unlock()
	assert.False(t, suppressed, "blocker must precede in-memory root suppression")
}

func TestDeleteProject_LoadsTaskTargetsOnce(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "alpha")
	registerArchivable(t, manager, repoID, repoPath, "beta")

	orig := loadTasksForRepoID
	loads := 0
	loadTasksForRepoID = func(gotRepoID string) ([]task.Task, []task.Task, error) {
		loads++
		return orig(gotRepoID)
	}
	t.Cleanup(func() { loadTasksForRepoID = orig })

	result, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.NoError(t, err)
	assert.Len(t, result.Archived, 2)
	assert.Equal(t, 1, loads, "project deletion must scope and index its task snapshot once")
}

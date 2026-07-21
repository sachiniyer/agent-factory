package daemon

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The task CRUD/trigger RPCs (#1029 PR 3) promote task writes to the daemon so
// it is the sole task writer among clients: the handler performs the tasks.json
// write and reloads its own scheduler/watchers in-process, and TriggerTask fires
// through the SAME RunTask path the in-daemon scheduler uses.

func enabledCronTask(id, repoPath string) task.Task {
	return task.Task{
		ID:          id,
		Name:        id,
		Prompt:      "do the thing",
		CronExpr:    "0 3 * * *",
		ProjectPath: repoPath,
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now(),
	}
}

// TestControlListTasks_ReadsDisk pins that the ListTasks handler returns the
// full task list read from tasks.json — the read side of the task
// single-writer model, and the disk fallback the CLI mirrors.
func TestControlListTasks_ReadsDisk(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, task.AddTask(enabledCronTask("aaaa0001", "")))
	require.NoError(t, task.AddTask(enabledCronTask("aaaa0002", "")))

	srv := &controlServer{}
	var resp ListTasksResponse
	require.NoError(t, srv.ListTasks(ListTasksRequest{}, &resp))
	require.Len(t, resp.Tasks, 2)
	ids := []string{resp.Tasks[0].ID, resp.Tasks[1].ID}
	assert.ElementsMatch(t, []string{"aaaa0001", "aaaa0002"}, ids)
}

// TestControlAddTask_WritesAndArmsSchedule pins that AddTask persists the task
// AND re-arms the scheduler in the same call, so no separate ReloadTasks poke is
// needed — the write and the schedule refresh happen atomically in the daemon.
func TestControlAddTask_WritesAndArmsSchedule(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	srv := &controlServer{scheduler: newTaskScheduler()}

	var resp AddTaskResponse
	require.NoError(t, srv.AddTask(AddTaskRequest{Task: enabledCronTask("bbbb0001", "")}, &resp))
	assert.True(t, resp.OK)

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1, "AddTask must persist the task to tasks.json")
	assert.Equal(t, "bbbb0001", tasks[0].ID)

	assert.Contains(t, srv.scheduler.scheduledTaskIDs(), "bbbb0001",
		"AddTask must re-arm the scheduler in-process (no separate reload poke)")
}

// TestControlUpdateTask_WritesAndRearms pins that UpdateTask persists the edit
// and re-arms the schedule set.
func TestControlUpdateTask_WritesAndRearms(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, task.AddTask(enabledCronTask("cccc0001", "")))

	srv := &controlServer{scheduler: newTaskScheduler()}
	newCron := "30 6 * * 1"

	var resp UpdateTaskResponse
	require.NoError(t, srv.UpdateTask(UpdateTaskRequest{ID: "cccc0001", Update: task.TaskUpdate{CronExpr: &newCron}}, &resp))
	assert.True(t, resp.OK)
	assert.Equal(t, "30 6 * * 1", resp.Task.CronExpr, "the response carries the merged record")

	got, err := task.GetTask("cccc0001")
	require.NoError(t, err)
	assert.Equal(t, "30 6 * * 1", got.CronExpr, "UpdateTask must persist the edit")
	assert.Contains(t, srv.scheduler.scheduledTaskIDs(), "cccc0001")
}

// TestControlRemoveTask_WritesAndDisarms pins that RemoveTask deletes the task
// and drops it from the scheduler in the same call.
func TestControlRemoveTask_WritesAndDisarms(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, task.AddTask(enabledCronTask("dddd0001", "")))

	srv := &controlServer{scheduler: newTaskScheduler()}
	require.NoError(t, srv.scheduler.Reload()) // arm it first
	require.Contains(t, srv.scheduler.scheduledTaskIDs(), "dddd0001")

	var resp RemoveTaskResponse
	require.NoError(t, srv.RemoveTask(RemoveTaskRequest{ID: "dddd0001"}, &resp))
	assert.True(t, resp.OK)

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	assert.Empty(t, tasks, "RemoveTask must delete the task from tasks.json")
	assert.NotContains(t, srv.scheduler.scheduledTaskIDs(), "dddd0001",
		"RemoveTask must disarm the scheduler in-process")
}

// TestControlAddTask_WarmupSkipsReload pins that during warm-up (manager not
// ready) the write still lands but the reload is skipped — RunDaemon reloads
// from tasks.json right after the restore, so the change is picked up then.
func TestControlAddTask_WarmupSkipsReload(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	manager, err := newManagerShell(config.DefaultConfig()) // not ready: RestoreInstances not called
	require.NoError(t, err)
	require.False(t, manager.Ready())

	srv := &controlServer{manager: manager, scheduler: newTaskScheduler()}
	var resp AddTaskResponse
	require.NoError(t, srv.AddTask(AddTaskRequest{Task: enabledCronTask("eeee0001", "")}, &resp))
	assert.True(t, resp.OK)

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1, "the write must land even during warm-up")
	assert.Empty(t, srv.scheduler.scheduledTaskIDs(),
		"warm-up must skip the reload; RunDaemon arms the scheduler after the restore")
}

func TestTaskControlLockIsSharedAcrossTransportServers(t *testing.T) {
	scheduler := newTaskScheduler()
	enteredLoad := make(chan int32, 2)
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	scheduler.loadTasks = func() ([]task.Task, error) {
		call := calls.Add(1)
		enteredLoad <- call
		if call == 1 {
			<-releaseFirst
		}
		return nil, nil
	}

	// Production registers distinct controlServer values for gob and HTTP, but
	// both carry this same scheduler pointer. The first transport is held inside
	// task reconciliation; the second must not enter even the load phase.
	rpcServer := &controlServer{scheduler: scheduler}
	httpServer := &controlServer{scheduler: scheduler}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- rpcServer.ReloadTasks(ReloadTasksRequest{}, &ReloadTasksResponse{})
	}()
	if call := <-enteredLoad; call != 1 {
		t.Fatalf("first load call = %d, want 1", call)
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- httpServer.ReloadTasks(ReloadTasksRequest{}, &ReloadTasksResponse{})
	}()
	select {
	case call := <-enteredLoad:
		close(releaseFirst)
		t.Fatalf("second transport entered task reconciliation as load call %d before the first released it", call)
	case <-time.After(time.Second):
		// Still blocked on the shared transport-independent control lock.
	}

	close(releaseFirst)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	require.EqualValues(t, 2, calls.Load())
}

// TestControlTriggerTask_UsesSharedFiringPath pins the core #1169-class fix:
// TriggerTask fires through RunTask — the SAME entrypoint the scheduler uses —
// so a manual trigger goes through the daemon's create-or-deliver path, proven
// here by the shared createSessionForTask hook receiving the task's details.
func TestControlTriggerTask_UsesSharedFiringPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	creates, _ := stubTaskDelivery(t)

	require.NoError(t, task.AddTask(enabledCronTask("ffff0001", repo)))

	srv := &controlServer{}
	var resp TriggerTaskResponse
	require.NoError(t, srv.TriggerTask(TriggerTaskRequest{ID: "ffff0001"}, &resp))
	assert.True(t, resp.OK)

	require.Len(t, *creates, 1, "trigger must fire through the shared RunTask path")
	assert.Equal(t, repo, (*creates)[0].RepoPath)
	assert.Equal(t, "do the thing", (*creates)[0].Prompt)

	// The shared path also records the run status, proving RunTask ran to
	// completion rather than a divergent copy.
	got, err := task.GetTask("ffff0001")
	require.NoError(t, err)
	assert.Equal(t, "started", got.LastRunStatus)
}

// TestControlTriggerTask_RefusesWatchTask pins that a watch task cannot be
// manually triggered (it fires from its watch command's stdout), and never
// reaches session creation.
func TestControlTriggerTask_RefusesWatchTask(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	creates, _ := stubTaskDelivery(t)
	require.NoError(t, task.AddTask(task.Task{
		ID:        "ffff0002",
		Name:      "watcher",
		WatchCmd:  "tail -f log",
		Enabled:   true,
		CreatedAt: time.Now(),
	}))

	srv := &controlServer{}
	var resp TriggerTaskResponse
	err := srv.TriggerTask(TriggerTaskRequest{ID: "ffff0002"}, &resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "watch task")
	assert.Empty(t, *creates, "a refused watch trigger must never create a session")
}

// TestControlTriggerTask_RefusesDisabledTask pins that a disabled task is
// refused, whichever caller asks.
func TestControlTriggerTask_RefusesDisabledTask(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	creates, _ := stubTaskDelivery(t)
	require.NoError(t, seedDisabledTask("ffff0003"))

	srv := &controlServer{}
	var resp TriggerTaskResponse
	err := srv.TriggerTask(TriggerTaskRequest{ID: "ffff0003"}, &resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disabled")
	assert.Empty(t, *creates, "a refused disabled trigger must never create a session")
}

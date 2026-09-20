package daemon

import (
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerReadyLookalike loads a settled session whose title claims the
// reserved root name under admission's fold but not as record identity: a
// local case variant ("Ro ot", which owns af_Root) or a provisioned-backend
// "ro ot" (no local tmux name at all). Both predate the widened admission and
// can only exist in storage from before it.
func registerReadyLookalike(t *testing.T, m *Manager, repoID, repoPath, title string, backend session.Backend) {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(backend)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Ready)
	require.NotEmpty(t, session.ReservedTitleCollision(title), "premise: admission refuses to create %q", title)
	require.False(t, session.IsReservedRecordTitle(title, inst.BackendType()),
		"premise: an existing %q record on %s is an ordinary session, not the root", title, inst.BackendType())
	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
}

// TestTaskMutations_RefuseReservedLookalikeTargetWhileItExists pins the #4407
// review fix. A task write commits a DURABLE binding, and the target record's
// lifetime is not fenced by it — KillSession does not consult target tasks —
// so admitting a binding to a title creation refuses defers a permanent
// per-run failure to the moment the record disappears. Existence of the
// record must not lift the refusal; it only changes the wording, which must
// not send the operator to "root" for a session that is not the root.
func TestTaskMutations_RefuseReservedLookalikeTargetWhileItExists(t *testing.T) {
	type act func(server *controlServer, repoPath, target string) error
	actions := []struct {
		name string
		seed func(repoPath, target string) task.Task
		act  act
	}{
		{
			name: "add",
			act: func(server *controlServer, repoPath, target string) error {
				return server.AddTask(AddTaskRequest{Task: archiveTargetTask(
					"look0001", "Lookalike Add", repoPath, target, true,
				)}, &AddTaskResponse{})
			},
		},
		{
			name: "enable",
			seed: func(repoPath, target string) task.Task {
				return archiveTargetTask("look0002", "Lookalike Enable", repoPath, target, false)
			},
			act: func(server *controlServer, _, _ string) error {
				enabled := true
				return server.UpdateTask(UpdateTaskRequest{ID: "look0002", Update: task.TaskUpdate{Enabled: &enabled}}, &UpdateTaskResponse{})
			},
		},
		{
			name: "retarget",
			seed: func(repoPath, _ string) task.Task {
				return archiveTargetTask("look0003", "Lookalike Retarget", repoPath, "worker", true)
			},
			act: func(server *controlServer, _, target string) error {
				return server.UpdateTask(UpdateTaskRequest{ID: "look0003", Update: task.TaskUpdate{TargetSession: &target}}, &UpdateTaskResponse{})
			},
		},
	}
	for _, rec := range lookalikeRecordShapes {
		for _, tc := range actions {
			t.Run(rec.name+"/"+tc.name, func(t *testing.T) {
				manager, repoID, repoPath := newStatusTestManager(t)
				registerReadyLookalike(t, manager, repoID, repoPath, rec.title, rec.backend())
				if tc.seed != nil {
					require.NoError(t, task.AddTask(tc.seed(repoPath, rec.title)))
				}
				before, err := task.LoadTasks()
				require.NoError(t, err)

				err = tc.act(archiveTaskControlServer(manager), repoPath, rec.title)
				require.Error(t, err, "an existing record must not license a binding its title can never re-create")
				assert.Contains(t, err.Error(), rec.title)
				assert.Contains(t, err.Error(), "exists")
				assert.Contains(t, err.Error(), "fail on every run once that session is gone")
				assert.Contains(t, err.Error(), "nothing was changed")
				assert.NotContains(t, err.Error(), "exactly",
					"the session exists and is not the root; telling the operator to use \"root\" names the wrong remedy")

				after, err := task.LoadTasks()
				require.NoError(t, err)
				assert.Equal(t, before, after, "a refused write must leave the task store unchanged")
			})
		}
	}
}

// TestTaskMutations_ReservedLookalikeTargetKeepsItsWayOut is the canary for the
// stricter fence above: the refusal must not trap the bindings that already
// exist. A task enabled against "Ro ot" before admission widened must still be
// editable, disableable, and retargetable to an ordinary title, and a new task
// aimed at an existing session whose title claims nothing reserved is still
// accepted.
func TestTaskMutations_ReservedLookalikeTargetKeepsItsWayOut(t *testing.T) {
	const lookalike = "Ro ot"
	manager, repoID, repoPath := newStatusTestManager(t)
	registerReadyLookalike(t, manager, repoID, repoPath, lookalike, session.NewFakeBackend())
	server := archiveTaskControlServer(manager)

	// Seeded without the fence: the state an older daemon accepted.
	edited := archiveTargetTask("keep0001", "Old Name", repoPath, lookalike, true)
	disabled := archiveTargetTask("keep0002", "To Disable", repoPath, lookalike, true)
	moved := archiveTargetTask("keep0003", "To Retarget", repoPath, lookalike, true)
	for _, seeded := range []task.Task{edited, disabled, moved} {
		require.NoError(t, task.AddTask(seeded))
	}

	newName := "New Name"
	var resp UpdateTaskResponse
	require.NoError(t, server.UpdateTask(UpdateTaskRequest{ID: edited.ID, Update: task.TaskUpdate{Name: &newName}}, &resp),
		"an unrelated edit neither creates nor worsens the binding")
	assert.Equal(t, newName, resp.Task.Name)
	assert.Equal(t, lookalike, resp.Task.TargetSession)

	off := false
	resp = UpdateTaskResponse{}
	require.NoError(t, server.UpdateTask(UpdateTaskRequest{ID: disabled.ID, Update: task.TaskUpdate{Enabled: &off}}, &resp),
		"disabling is how an operator stops the doomed binding")
	assert.False(t, resp.Task.Enabled)

	ordinary := "worker"
	resp = UpdateTaskResponse{}
	require.NoError(t, server.UpdateTask(UpdateTaskRequest{ID: moved.ID, Update: task.TaskUpdate{TargetSession: &ordinary}}, &resp),
		"retargeting away to an auto-creatable title must be accepted")
	assert.Equal(t, ordinary, resp.Task.TargetSession)

	registerOrdinary := func(title string) {
		inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
		require.NoError(t, err)
		inst.SetBackend(session.NewFakeBackend())
		inst.SetStartedForTest(true)
		inst.SetStatusForTest(session.Ready)
		manager.mu.Lock()
		manager.instances[daemonInstanceKey(repoID, title)] = inst
		manager.mu.Unlock()
	}
	// "Ro-ot" is the near miss: it shares letters with the lookalike but derives
	// af_Ro-ot, which claims nothing reserved under any fold.
	for _, title := range []string{"captain", "Ro-ot"} {
		require.Empty(t, session.ReservedTitleCollision(title), "premise: %q claims nothing reserved", title)
		registerOrdinary(title)
		require.NoError(t, server.AddTask(AddTaskRequest{Task: archiveTargetTask(
			"fresh-"+title, "Fresh "+title, repoPath, title, true,
		)}, &AddTaskResponse{}), "an existing ordinary target must still accept a new enabled task")
	}
}

// lookalikeRecordShapes are the two ordinary records whose title admission
// refuses: a local case variant, and a provisioned-backend derived name.
var lookalikeRecordShapes = []struct {
	name    string
	title   string
	backend func() session.Backend
}{
	{"local case variant", "Ro ot", func() session.Backend { return session.NewFakeBackend() }},
	{"remote derived name", "ro ot", func() session.Backend { return fakeRemoteBackend{session.NewFakeBackend()} }},
}

// TestTaskArming_PersistedLookalikeBindingArmsWhileItsRecordExists pins option
// (a) of the #4407 change request. The write-side refusal above cannot reach a
// binding enabled before admission widened, so the arming pass is where such a
// binding meets the widened fold — at the first daemon start after upgrade.
// Delivery sends to an existing target without asking admission, so while the
// ordinary record exists the task works, and arming must schedule it (cron)
// and run it (watch). Once the record is gone the binding can only fail, and
// the next arming pass refuses it.
func TestTaskArming_PersistedLookalikeBindingArmsWhileItsRecordExists(t *testing.T) {
	for _, rec := range lookalikeRecordShapes {
		t.Run(rec.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			registerReadyLookalike(t, manager, repoID, repoPath, rec.title, rec.backend())

			// Seeded without the fence: the bindings an older daemon accepted.
			const cronID, watchID = "arm00001", "arm00002"
			require.NoError(t, task.AddTask(archiveTargetTask(cronID, "Nightly Sweep", repoPath, rec.title, true)))
			watch := watchTask(watchID, "sleep 60", repoPath)
			watch.TargetSession = rec.title
			require.NoError(t, task.AddTask(watch))

			scheduler := newTaskScheduler()
			watchers, _ := newTestSupervisor(t, task.LoadTasks)
			require.NoError(t, armTaskAutomation(manager, scheduler, watchers),
				"startup arming must not refuse a binding whose ordinary target record still exists")
			assert.Contains(t, scheduler.scheduledTaskIDs(), cronID, "the cron binding must be scheduled")
			assert.Contains(t, watchers.watchingTaskIDs(), watchID, "the watch binding must be running")
			for _, id := range []string{cronID, watchID} {
				assert.NotContains(t, reloadArmingTask(t, id).LastRunStatus, "not armed",
					"an armed task must not carry a not-armed status")
			}

			// The record goes away: the binding can no longer be delivered, and
			// the next arming pass must refuse it rather than keep it scheduled.
			manager.mu.Lock()
			delete(manager.instances, daemonInstanceKey(repoID, rec.title))
			manager.mu.Unlock()
			require.NoError(t, config.LoadState().SaveInstances(repoID, []byte("[]")))

			scheduler.controlMu.Lock()
			refused, err := reloadTaskAutomation(manager, scheduler, watchers, everyWatchTask())
			scheduler.controlMu.Unlock()
			require.NoError(t, err)
			joined := errors.Join(refused...)
			require.Error(t, joined, "a binding whose record is gone must be refused at arming")
			assert.Contains(t, joined.Error(), cronID)
			assert.Contains(t, joined.Error(), watchID)
			assert.NotContains(t, scheduler.scheduledTaskIDs(), cronID)
			assert.NotContains(t, watchers.watchingTaskIDs(), watchID)
		})
	}
}

// TestRestartTask_PersistedLookalikeBindingRestarts is the explicit-restart
// sibling of the arming test above. RestartTask re-validates the binding before
// replacing the watch process, and it commits nothing, so it must accept the
// same persisted binding the arming pass accepts. Otherwise a watch the daemon
// keeps running could not be restarted by hand.
func TestRestartTask_PersistedLookalikeBindingRestarts(t *testing.T) {
	for _, rec := range lookalikeRecordShapes {
		t.Run(rec.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			registerReadyLookalike(t, manager, repoID, repoPath, rec.title, rec.backend())
			watch := watchTask("rst00001", "sleep 60", repoPath)
			watch.TargetSession = rec.title
			require.NoError(t, task.AddTask(watch), "seeded without the fence: the binding an older daemon accepted")

			watchers, _ := newTestSupervisor(t, task.LoadTasks)
			server := &controlServer{manager: manager, scheduler: newTaskScheduler(), watchers: watchers}
			require.NoError(t, server.RestartTask(RestartTaskRequest{ID: watch.ID}, &RestartTaskResponse{}),
				"restarting a persisted watch binding whose ordinary record exists commits nothing and must not be refused")
			assert.Equal(t, []string{watch.ID}, watchers.watchingTaskIDs())
		})
	}
}

package api

import (
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schedulerCalls tracks stubbed invocations of the scheduler helpers so
// tests can assert that `af tasks update` reconciles systemd state
// correctly (see #258).
type schedulerCalls struct {
	installed     []task.Task
	removed       []task.Task
	updateTaskErr error
}

// stubSchedulers swaps installScheduler/removeScheduler/updateTask for
// in-memory stubs and restores them on test cleanup.
func stubSchedulers(t *testing.T) *schedulerCalls {
	t.Helper()
	calls := &schedulerCalls{}

	origInstall := installScheduler
	origRemove := removeScheduler
	origUpdate := updateTask
	installScheduler = func(tsk task.Task) error {
		calls.installed = append(calls.installed, tsk)
		return nil
	}
	removeScheduler = func(tsk task.Task) error {
		calls.removed = append(calls.removed, tsk)
		return nil
	}
	updateTask = func(tsk task.Task) error {
		if calls.updateTaskErr != nil {
			return calls.updateTaskErr
		}
		return origUpdate(tsk)
	}
	t.Cleanup(func() {
		installScheduler = origInstall
		removeScheduler = origRemove
		updateTask = origUpdate
	})
	return calls
}

// useTempConfig redirects AGENT_FACTORY_HOME to a temp dir so task
// persistence is isolated per-test.
func useTempConfig(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
}

// resetUpdateFlags clears the package-level update flag variables so
// tests don't leak state into each other.
func resetUpdateFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		taskUpdateNameFlag = ""
		taskUpdatePromptFlag = ""
		taskUpdateCronFlag = ""
		taskUpdateEnabledFlag = ""
	})
	taskUpdateNameFlag = ""
	taskUpdatePromptFlag = ""
	taskUpdateCronFlag = ""
	taskUpdateEnabledFlag = ""
}

// seedTask persists a single task for update tests.
func seedTask(t *testing.T, tsk task.Task) {
	t.Helper()
	require.NoError(t, task.AddTask(tsk))
}

func TestTasksUpdate_DisableRemovesScheduler(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "t1", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateEnabledFlag = "false"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t1"})
	require.NoError(t, err)

	assert.Empty(t, calls.installed, "disable should not install scheduler")
	require.Len(t, calls.removed, 1, "disable should remove scheduler")
	assert.Equal(t, "t1", calls.removed[0].ID)

	got, err := task.GetTask("t1")
	require.NoError(t, err)
	assert.False(t, got.Enabled)
}

func TestTasksUpdate_EnableInstallsScheduler(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "t2", CronExpr: "0 9 * * *", Enabled: false})

	taskUpdateEnabledFlag = "true"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t2"})
	require.NoError(t, err)

	assert.Empty(t, calls.removed, "enable should not remove scheduler")
	require.Len(t, calls.installed, 1, "enable should install scheduler")
	assert.Equal(t, "t2", calls.installed[0].ID)
	assert.True(t, calls.installed[0].Enabled)

	got, err := task.GetTask("t2")
	require.NoError(t, err)
	assert.True(t, got.Enabled)
}

func TestTasksUpdate_CronChangeOnDisabledTaskRemovesScheduler(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "t3", CronExpr: "0 9 * * *", Enabled: false})

	taskUpdateCronFlag = "0 10 * * *"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t3"})
	require.NoError(t, err)

	assert.Empty(t, calls.installed, "cron change on disabled task must not install scheduler")
	require.Len(t, calls.removed, 1, "cron change on disabled task should remove scheduler")

	got, err := task.GetTask("t3")
	require.NoError(t, err)
	assert.Equal(t, "0 10 * * *", got.CronExpr)
	assert.False(t, got.Enabled)
}

func TestTasksUpdate_CronChangeWithDisableRemovesScheduler(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "t4", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateCronFlag = "0 10 * * *"
	taskUpdateEnabledFlag = "false"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t4"})
	require.NoError(t, err)

	assert.Empty(t, calls.installed, "simultaneous cron change + disable must not install scheduler")
	require.Len(t, calls.removed, 1, "simultaneous cron change + disable should remove scheduler")

	got, err := task.GetTask("t4")
	require.NoError(t, err)
	assert.Equal(t, "0 10 * * *", got.CronExpr)
	assert.False(t, got.Enabled)
}

func TestTasksUpdate_CronChangeOnEnabledTaskInstallsScheduler(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "t5", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateCronFlag = "0 10 * * *"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t5"})
	require.NoError(t, err)

	assert.Empty(t, calls.removed, "cron change on enabled task should not remove scheduler")
	require.Len(t, calls.installed, 1, "cron change on enabled task should install scheduler")
	assert.Equal(t, "0 10 * * *", calls.installed[0].CronExpr)
}

func TestTasksUpdate_NameOnlyChangeDoesNotTouchScheduler(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "t6", Name: "old", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateNameFlag = "new"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t6"})
	require.NoError(t, err)

	assert.Empty(t, calls.installed, "name-only change should not install scheduler")
	assert.Empty(t, calls.removed, "name-only change should not remove scheduler")

	got, err := task.GetTask("t6")
	require.NoError(t, err)
	assert.Equal(t, "new", got.Name)
}

func TestTasksUpdate_EnabledToggleNoOpWhenAlreadyEnabled(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "t7", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateEnabledFlag = "true"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t7"})
	require.NoError(t, err)

	assert.Empty(t, calls.installed, "setting enabled=true on already-enabled task should be a no-op")
	assert.Empty(t, calls.removed)
}

// TestTasksUpdate_RollsBackSchedulerWhenUpdateTaskFails is the
// regression test for #324: when installScheduler succeeds but the
// subsequent updateTask write fails, tasksUpdateCmd must best-effort
// roll back the scheduler so that JSON state and scheduler state stay
// consistent.
func TestTasksUpdate_RollsBackSchedulerWhenUpdateTaskFails(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "t8", CronExpr: "0 9 * * *", Enabled: true})

	calls.updateTaskErr = errors.New("simulated JSON write failure")
	taskUpdateCronFlag = "0 10 * * *"

	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t8"})
	assert.Error(t, err, "update should fail when JSON write fails")

	// Scheduler was installed with the new cron, then rolled back to
	// the old cron when the JSON write failed.
	require.Len(t, calls.installed, 2, "scheduler should be installed then rolled back")
	assert.Equal(t, "0 10 * * *", calls.installed[0].CronExpr, "first install uses new cron")
	assert.Equal(t, "0 9 * * *", calls.installed[1].CronExpr, "rollback restores old cron")

	// JSON file still has the old cron because updateTask failed.
	got, err := task.GetTask("t8")
	require.NoError(t, err)
	assert.Equal(t, "0 9 * * *", got.CronExpr, "JSON should retain old cron after failed write")
}

// TestTasksUpdate_RollsBackSchedulerInstallOnEnableWhenUpdateTaskFails
// covers the disabled→enabled transition: installScheduler succeeded
// but updateTask failed, so the scheduler install should be undone via
// removeScheduler.
func TestTasksUpdate_RollsBackSchedulerInstallOnEnableWhenUpdateTaskFails(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "t9", CronExpr: "0 9 * * *", Enabled: false})

	calls.updateTaskErr = errors.New("simulated JSON write failure")
	taskUpdateEnabledFlag = "true"

	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t9"})
	assert.Error(t, err, "update should fail when JSON write fails")

	require.Len(t, calls.installed, 1, "scheduler should be installed once")
	require.Len(t, calls.removed, 1, "scheduler install should be rolled back via remove")
	assert.Equal(t, "t9", calls.removed[0].ID)
}

// TestTasksUpdate_RollsBackSchedulerRemoveOnDisableWhenUpdateTaskFails
// covers the enabled→disabled transition: removeScheduler succeeded
// but updateTask failed, so the scheduler should be re-installed with
// the old cron.
func TestTasksUpdate_RollsBackSchedulerRemoveOnDisableWhenUpdateTaskFails(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "t10", CronExpr: "0 9 * * *", Enabled: true})

	calls.updateTaskErr = errors.New("simulated JSON write failure")
	taskUpdateEnabledFlag = "false"

	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t10"})
	assert.Error(t, err, "update should fail when JSON write fails")

	require.Len(t, calls.removed, 1, "scheduler should be removed once")
	require.Len(t, calls.installed, 1, "scheduler remove should be rolled back via install")
	assert.Equal(t, "0 9 * * *", calls.installed[0].CronExpr, "rollback restores old cron")
	assert.True(t, calls.installed[0].Enabled, "rollback restores enabled=true")
}

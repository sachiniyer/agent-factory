package api

import (
	"testing"

	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schedulerCalls tracks stubbed invocations of the scheduler helpers so
// tests can assert that `af tasks update` reconciles systemd state
// correctly (see #258).
type schedulerCalls struct {
	installed []task.Task
	removed   []task.Task
}

// stubSchedulers swaps installScheduler/removeScheduler for in-memory
// stubs and restores them on test cleanup.
func stubSchedulers(t *testing.T) *schedulerCalls {
	t.Helper()
	calls := &schedulerCalls{}

	origInstall := installScheduler
	origRemove := removeScheduler
	installScheduler = func(tsk task.Task) error {
		calls.installed = append(calls.installed, tsk)
		return nil
	}
	removeScheduler = func(tsk task.Task) error {
		calls.removed = append(calls.removed, tsk)
		return nil
	}
	t.Cleanup(func() {
		installScheduler = origInstall
		removeScheduler = origRemove
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

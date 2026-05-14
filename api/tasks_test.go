package api

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
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

	// Error-injection knobs. installErrs / removeErrs are consumed in
	// order so multi-call flows (e.g. the install→rollback path) can
	// fail a specific call without failing every call.
	installErrs    []error
	removeErrs     []error
	updateTaskErr  error
	removeTaskErr  error
	removeTaskHook func(string)
}

// stubSchedulers swaps installScheduler/removeScheduler/updateTask/removeTask
// for in-memory stubs and restores them on test cleanup.
func stubSchedulers(t *testing.T) *schedulerCalls {
	t.Helper()
	calls := &schedulerCalls{}

	origInstall := installScheduler
	origRemove := removeScheduler
	origUpdate := updateTask
	origRemoveTask := removeTask
	installScheduler = func(tsk task.Task) error {
		calls.installed = append(calls.installed, tsk)
		if len(calls.installErrs) > 0 {
			err := calls.installErrs[0]
			calls.installErrs = calls.installErrs[1:]
			if err != nil {
				return err
			}
		}
		return nil
	}
	removeScheduler = func(tsk task.Task) error {
		calls.removed = append(calls.removed, tsk)
		if len(calls.removeErrs) > 0 {
			err := calls.removeErrs[0]
			calls.removeErrs = calls.removeErrs[1:]
			if err != nil {
				return err
			}
		}
		return nil
	}
	updateTask = func(tsk task.Task) error {
		if calls.updateTaskErr != nil {
			return calls.updateTaskErr
		}
		return origUpdate(tsk)
	}
	removeTask = func(id string) error {
		if calls.removeTaskHook != nil {
			calls.removeTaskHook(id)
		}
		if calls.removeTaskErr != nil {
			return calls.removeTaskErr
		}
		return origRemoveTask(id)
	}
	t.Cleanup(func() {
		installScheduler = origInstall
		removeScheduler = origRemove
		updateTask = origUpdate
		removeTask = origRemoveTask
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

// TestTasksRemove_HappyPath verifies that when both removeScheduler and
// RemoveTask succeed, the final state has no scheduler and no task
// record.
func TestTasksRemove_HappyPath(t *testing.T) {
	useTempConfig(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "r1", CronExpr: "0 9 * * *", Enabled: true})

	err := tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"r1"})
	require.NoError(t, err)

	require.Len(t, calls.removed, 1, "scheduler should be removed once")
	assert.Empty(t, calls.installed, "happy-path remove must not install scheduler")

	_, err = task.GetTask("r1")
	assert.Error(t, err, "task record should be gone")
}

// TestTasksRemove_RemoveSchedulerFailsLeavesTask verifies that when
// removeScheduler fails the task record is NOT deleted (i.e. the
// half-removal #457 describes can't happen by skipping RemoveTask).
func TestTasksRemove_RemoveSchedulerFailsLeavesTask(t *testing.T) {
	useTempConfig(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "r2", CronExpr: "0 9 * * *", Enabled: true})
	calls.removeErrs = []error{errors.New("simulated scheduler removal failure")}

	err := tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"r2"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to remove task scheduler")

	require.Len(t, calls.removed, 1, "removeScheduler should be attempted exactly once")
	assert.Empty(t, calls.installed, "no rollback install should run when removeScheduler fails")

	got, err := task.GetTask("r2")
	require.NoError(t, err, "task record must still exist when scheduler removal fails")
	assert.Equal(t, "r2", got.ID)
}

// TestTasksRemove_RemoveTaskFailsRollsBackScheduler is the regression
// test for #457: removeScheduler succeeded but RemoveTask failed; the
// scheduler must be re-installed so the still-listed task stays
// schedulable.
func TestTasksRemove_RemoveTaskFailsRollsBackScheduler(t *testing.T) {
	useTempConfig(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "r3", CronExpr: "0 9 * * *", Enabled: true})
	calls.removeTaskErr = errors.New("simulated tasks.json write failure")

	err := tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"r3"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to remove task")
	assert.Contains(t, err.Error(), "re-installed", "error should mention the rollback")
	assert.Contains(t, err.Error(), "af tasks remove r3", "error should tell the user how to retry")

	require.Len(t, calls.removed, 1, "scheduler should be removed once")
	require.Len(t, calls.installed, 1, "scheduler should be re-installed exactly once as rollback")
	assert.Equal(t, "r3", calls.installed[0].ID)
	assert.Equal(t, "0 9 * * *", calls.installed[0].CronExpr)
	assert.True(t, calls.installed[0].Enabled)

	got, err := task.GetTask("r3")
	require.NoError(t, err, "task record must remain when RemoveTask fails")
	assert.Equal(t, "r3", got.ID)
}

// TestTasksRemove_RollbackInstallAlsoFails verifies that when both the
// RemoveTask write fails AND the scheduler-rollback InstallScheduler
// fails, the returned error names both failures and tells the user how
// to recover.
func TestTasksRemove_RollbackInstallAlsoFails(t *testing.T) {
	useTempConfig(t)
	calls := stubSchedulers(t)

	seedTask(t, task.Task{ID: "r4", CronExpr: "0 9 * * *", Enabled: true})
	calls.removeTaskErr = errors.New("simulated tasks.json write failure")
	calls.installErrs = []error{errors.New("simulated rollback install failure")}

	err := tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"r4"})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "simulated tasks.json write failure", "compound error must name the RemoveTask failure")
	assert.Contains(t, msg, "simulated rollback install failure", "compound error must name the rollback failure")
	assert.Contains(t, msg, "rollback also failed")
	assert.Contains(t, msg, "tasks.json", "user must be told where the orphaned record lives")

	require.Len(t, calls.removed, 1, "scheduler should be removed once")
	require.Len(t, calls.installed, 1, "rollback install should be attempted once")

	got, err := task.GetTask("r4")
	require.NoError(t, err, "task record must remain when RemoveTask fails")
	assert.Equal(t, "r4", got.ID)
}

// resetAddFlags clears the package-level add flag variables so add-path
// tests don't leak state into each other.
func resetAddFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		taskAddNameFlag = ""
		taskAddPromptFlag = ""
		taskAddCronFlag = ""
		taskAddProgramFlag = ""
		repoFlag = ""
	})
	taskAddNameFlag = ""
	taskAddPromptFlag = ""
	taskAddCronFlag = ""
	taskAddProgramFlag = ""
}

// setupAddRepo creates a throwaway git repo so resolveRepo() inside
// tasksAddCmd succeeds. Returns the repo path.
func setupAddRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", repo).Run(), "git init")
	require.NoError(t, exec.Command("git", "-C", repo, "config", "user.email", "test@example.com").Run())
	require.NoError(t, exec.Command("git", "-C", repo, "config", "user.name", "Test User").Run())
	require.NoError(t, exec.Command("git", "-C", repo, "commit", "--allow-empty", "-m", "init").Run())
	repoFlag = repo
	return repo
}

// TestTasksAdd_HappyPathDoesNotRollBack is the regression guard for the
// existing behavior: when installScheduler succeeds, neither
// removeScheduler nor removeTask runs.
func TestTasksAdd_HappyPathDoesNotRollBack(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t)
	calls := stubSchedulers(t)
	setupAddRepo(t)

	taskAddNameFlag = "happy"
	taskAddPromptFlag = "hello"
	taskAddCronFlag = "0 9 * * *"
	taskAddProgramFlag = "claude"

	err := tasksAddCmd.RunE(tasksAddCmd, nil)
	require.NoError(t, err)

	require.Len(t, calls.installed, 1, "installScheduler runs once")
	assert.Empty(t, calls.removed, "no rollback when install succeeds")

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1, "task record must persist on success")
}

// TestTasksAdd_InstallSchedulerFailsRollsBackBoth is the regression
// test for #458: when installScheduler fails after writing scheduler
// files, the rollback must call BOTH removeScheduler (to clean up
// systemd unit/timer or launchd plist) and removeTask (to clean up the
// JSON record).
func TestTasksAdd_InstallSchedulerFailsRollsBackBoth(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t)
	calls := stubSchedulers(t)
	setupAddRepo(t)

	calls.installErrs = []error{errors.New("simulated systemctl enable failure")}

	taskAddNameFlag = "rollback-both"
	taskAddPromptFlag = "p"
	taskAddCronFlag = "0 9 * * *"
	taskAddProgramFlag = "claude"

	err := tasksAddCmd.RunE(tasksAddCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to install task scheduler")

	require.Len(t, calls.installed, 1, "installScheduler runs once")
	require.Len(t, calls.removed, 1, "rollback must call removeScheduler to clean up files written before install failure")

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	assert.Empty(t, tasks, "task record must be removed when scheduler install fails")
}

// TestTasksAdd_RollbackRemoveSchedulerAlsoFails verifies that when the
// scheduler-file rollback ALSO fails, removeTask is still attempted and
// the returned error names both failures so the user can clean up
// orphaned scheduler files manually.
func TestTasksAdd_RollbackRemoveSchedulerAlsoFails(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t)
	calls := stubSchedulers(t)
	setupAddRepo(t)

	calls.installErrs = []error{errors.New("simulated systemctl enable failure")}
	calls.removeErrs = []error{errors.New("simulated scheduler cleanup failure")}

	taskAddNameFlag = "rollback-fail"
	taskAddPromptFlag = "p"
	taskAddCronFlag = "0 9 * * *"
	taskAddProgramFlag = "claude"

	err := tasksAddCmd.RunE(tasksAddCmd, nil)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "simulated systemctl enable failure", "primary install failure must surface")
	assert.Contains(t, msg, "simulated scheduler cleanup failure", "scheduler-rollback failure must surface")
	assert.Contains(t, msg, "scheduler file cleanup also failed")

	require.Len(t, calls.installed, 1, "installScheduler runs once")
	require.Len(t, calls.removed, 1, "removeScheduler is still attempted")

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	assert.Empty(t, tasks, "removeTask must still run even when scheduler rollback fails")
}

// TestTasksAdd_RejectsEmptyPrompt is the regression guard for #517: Cobra's
// MarkFlagRequired only checks flag presence, so --prompt "" (or
// whitespace-only) used to slip through and create a task that no-ops when
// triggered. tasksAddCmd must reject the value before any scheduler work runs.
func TestTasksAdd_RejectsEmptyPrompt(t *testing.T) {
	for _, prompt := range []string{"", "   ", "\t\n"} {
		t.Run(fmt.Sprintf("prompt=%q", prompt), func(t *testing.T) {
			useTempConfig(t)
			resetAddFlags(t)
			calls := stubSchedulers(t)
			setupAddRepo(t)

			taskAddNameFlag = "blank"
			taskAddPromptFlag = prompt
			taskAddCronFlag = "0 9 * * *"
			taskAddProgramFlag = "claude"

			err := tasksAddCmd.RunE(tasksAddCmd, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "prompt must be non-empty")

			assert.Empty(t, calls.installed, "scheduler install must not run when prompt fails validation")

			tasks, err := task.LoadTasks()
			require.NoError(t, err)
			assert.Empty(t, tasks, "no task record must be persisted when prompt fails validation")
		})
	}
}

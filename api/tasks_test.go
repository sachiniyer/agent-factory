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

// daemonCalls tracks stubbed invocations of the daemon RPC helpers so tests
// can assert that task CRUD pokes the daemon to reload its schedule set
// (#782) without dialing — or spawning — a real daemon.
type daemonCalls struct {
	reloads   int
	reloadErr error
	triggered []string
	runErr    error
}

// stubDaemon swaps the daemon RPC indirections for in-memory stubs and
// restores them on test cleanup.
func stubDaemon(t *testing.T) *daemonCalls {
	t.Helper()
	calls := &daemonCalls{}

	origReload := reloadDaemonTasks
	origRun := runTask
	reloadDaemonTasks = func() error {
		calls.reloads++
		return calls.reloadErr
	}
	runTask = func(id string) error {
		calls.triggered = append(calls.triggered, id)
		return calls.runErr
	}
	t.Cleanup(func() {
		reloadDaemonTasks = origReload
		runTask = origRun
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

// resetAddFlags clears the package-level add flag variables so tests don't
// leak state into each other.
func resetAddFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		taskAddNameFlag = ""
		taskAddPromptFlag = ""
		taskAddCronFlag = ""
		taskAddProgramFlag = ""
		repoFlag = ""
	}
	t.Cleanup(reset)
	reset()
}

// seedTask persists a single task for update tests.
func seedTask(t *testing.T, tsk task.Task) {
	t.Helper()
	require.NoError(t, task.AddTask(tsk))
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

func TestTasksAdd_PersistsTaskAndPokesDaemon(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t)
	calls := stubDaemon(t)
	repo := setupAddRepo(t)

	taskAddNameFlag = "nightly"
	taskAddPromptFlag = "do the nightly sweep"
	taskAddCronFlag = "0 3 * * *"
	taskAddProgramFlag = "claude"

	err := tasksAddCmd.RunE(tasksAddCmd, nil)
	require.NoError(t, err)

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "nightly", tasks[0].Name)
	assert.True(t, tasks[0].Enabled)
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	resolvedProject, err := filepath.EvalSymlinks(tasks[0].ProjectPath)
	require.NoError(t, err)
	assert.Equal(t, resolvedRepo, resolvedProject)

	assert.Equal(t, 1, calls.reloads, "add must poke the daemon to reload schedules")
}

// TestTasksAdd_DaemonPokeFailureDoesNotFailAdd pins the eventual-consistency
// contract that replaced the old install/rollback dance (#324/#457/#458):
// the task record is the source of truth; a failed reload poke is logged and
// the daemon picks the task up at its next start.
func TestTasksAdd_DaemonPokeFailureDoesNotFailAdd(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t)
	calls := stubDaemon(t)
	calls.reloadErr = errors.New("simulated daemon-start failure")
	setupAddRepo(t)

	taskAddNameFlag = "poke-fail"
	taskAddPromptFlag = "p"
	taskAddCronFlag = "0 9 * * *"
	taskAddProgramFlag = "claude"

	err := tasksAddCmd.RunE(tasksAddCmd, nil)
	require.NoError(t, err, "a failed daemon poke must not fail the add")

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1, "task record must persist even when the poke fails")
}

// TestTasksAdd_RejectsEmptyPrompt is the regression guard for #517: Cobra's
// MarkFlagRequired only checks flag presence, so --prompt "" (or
// whitespace-only) used to slip through and create a task that no-ops when
// triggered.
func TestTasksAdd_RejectsEmptyPrompt(t *testing.T) {
	for _, prompt := range []string{"", "   ", "\t\n"} {
		t.Run(fmt.Sprintf("prompt=%q", prompt), func(t *testing.T) {
			useTempConfig(t)
			resetAddFlags(t)
			calls := stubDaemon(t)
			setupAddRepo(t)

			taskAddNameFlag = "blank"
			taskAddPromptFlag = prompt
			taskAddCronFlag = "0 9 * * *"
			taskAddProgramFlag = "claude"

			err := tasksAddCmd.RunE(tasksAddCmd, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "prompt must be non-empty")

			assert.Zero(t, calls.reloads, "no daemon poke when validation fails")

			tasks, err := task.LoadTasks()
			require.NoError(t, err)
			assert.Empty(t, tasks, "no task record must be persisted when prompt fails validation")
		})
	}
}

func TestTasksAdd_RejectsInvalidCron(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t)
	calls := stubDaemon(t)
	setupAddRepo(t)

	taskAddNameFlag = "badcron"
	taskAddPromptFlag = "p"
	taskAddCronFlag = "61 * * * *"
	taskAddProgramFlag = "claude"

	err := tasksAddCmd.RunE(tasksAddCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid cron expression")
	assert.Zero(t, calls.reloads)
}

func TestTasksUpdate_DisablePersistsAndPokesDaemon(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "t1", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateEnabledFlag = "false"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t1"})
	require.NoError(t, err)

	got, err := task.GetTask("t1")
	require.NoError(t, err)
	assert.False(t, got.Enabled)
	assert.Equal(t, 1, calls.reloads, "update must poke the daemon to reload schedules")
}

func TestTasksUpdate_CronChangePersistsAndPokesDaemon(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "t2", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateCronFlag = "30 6 * * 1"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t2"})
	require.NoError(t, err)

	got, err := task.GetTask("t2")
	require.NoError(t, err)
	assert.Equal(t, "30 6 * * 1", got.CronExpr)
	assert.Equal(t, 1, calls.reloads)
}

func TestTasksUpdate_RejectsInvalidCron(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "t3", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateCronFlag = "not a cron"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t3"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid cron expression")

	got, err := task.GetTask("t3")
	require.NoError(t, err)
	assert.Equal(t, "0 9 * * *", got.CronExpr, "cron must remain unchanged on validation failure")
	assert.Zero(t, calls.reloads)
}

func TestTasksUpdate_RejectsEmptyPrompt(t *testing.T) {
	for _, prompt := range []string{"   ", "\t\n"} {
		t.Run(fmt.Sprintf("prompt=%q", prompt), func(t *testing.T) {
			useTempConfig(t)
			resetUpdateFlags(t)
			calls := stubDaemon(t)

			seedTask(t, task.Task{
				ID:       "t-whitespace",
				Name:     "test",
				Prompt:   "valid prompt",
				CronExpr: "0 9 * * *",
				Enabled:  true,
			})

			taskUpdatePromptFlag = prompt
			err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t-whitespace"})

			require.Error(t, err, "whitespace-only prompt should be rejected")
			assert.Contains(t, err.Error(), "prompt must be non-empty")

			got, err := task.GetTask("t-whitespace")
			require.NoError(t, err)
			assert.Equal(t, "valid prompt", got.Prompt, "prompt should remain unchanged")
			assert.Zero(t, calls.reloads)
		})
	}
}

func TestTasksUpdate_RejectsBadEnabledValue(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "t4", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateEnabledFlag = "yes"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t4"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--enabled must be 'true' or 'false'")
	assert.Zero(t, calls.reloads)
}

func TestTasksRemove_RemovesTaskAndPokesDaemon(t *testing.T) {
	useTempConfig(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "t5", CronExpr: "0 9 * * *", Enabled: true})

	err := tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"t5"})
	require.NoError(t, err)

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	assert.Empty(t, tasks)
	assert.Equal(t, 1, calls.reloads, "remove must poke the daemon to reload schedules")
}

func TestTasksRemove_MissingTaskErrors(t *testing.T) {
	useTempConfig(t)
	calls := stubDaemon(t)

	err := tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"nope1234"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Zero(t, calls.reloads, "no daemon poke when nothing changed")
}

func TestTasksTrigger_RunsThroughDaemon(t *testing.T) {
	useTempConfig(t)
	calls := stubDaemon(t)

	err := tasksRunCmd.RunE(tasksRunCmd, []string{"t6abcd"})
	require.NoError(t, err)
	assert.Equal(t, []string{"t6abcd"}, calls.triggered)
}

func TestTasksTrigger_RejectsInvalidTaskID(t *testing.T) {
	useTempConfig(t)
	calls := stubDaemon(t)

	err := tasksRunCmd.RunE(tasksRunCmd, []string{"../evil"})
	require.Error(t, err)
	assert.Empty(t, calls.triggered, "invalid IDs must be rejected before reaching the daemon")
}

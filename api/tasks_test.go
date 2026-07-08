package api

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// daemonCalls tracks stubbed invocations of the daemon task RPCs so tests can
// assert that task CRUD dispatches to the daemon (#1029 PR 3) without dialing —
// or spawning — a real daemon. Task writes are now daemon-owned: the CLI sends
// the write to the daemon, which persists tasks.json and reloads its own
// scheduler/watchers in-process, so `writes` counts successful add/update/remove
// RPC dispatches (each of which subsumes the old separate reload poke).
type daemonCalls struct {
	writes    int
	addErr    error
	updateErr error
	removeErr error
	triggered []string
	runErr    error
}

// stubDaemon swaps the daemon task-RPC indirections for in-memory stubs and
// restores them on test cleanup. The write stubs perform the REAL disk write
// (task.AddTask/UpdateTask/RemoveTask) so the tests' on-disk assertions
// (task.LoadTasks/GetTask) still hold exactly as they did when the CLI wrote
// disk directly — the behavior under test is only that the write now dispatches
// through the daemon RPC. Injecting an *Err short-circuits before the write to
// model an RPC failure. The read stub defaults to ErrDaemonUnavailable so
// list/get fall back to the disk the tests seed; read-path tests override it.
func stubDaemon(t *testing.T) *daemonCalls {
	t.Helper()
	calls := &daemonCalls{}

	origAdd := daemonAddTask
	origUpdate := daemonUpdateTask
	origRemove := daemonRemoveTask
	origTrigger := daemonTriggerTask
	origList := daemonListTasksNoSpawn

	daemonAddTask = func(tk task.Task) error {
		if calls.addErr != nil {
			return calls.addErr
		}
		if err := task.AddTask(tk); err != nil {
			return err
		}
		calls.writes++
		return nil
	}
	daemonUpdateTask = func(tk task.Task) error {
		if calls.updateErr != nil {
			return calls.updateErr
		}
		if err := task.UpdateTask(tk); err != nil {
			return err
		}
		calls.writes++
		return nil
	}
	daemonRemoveTask = func(id string) error {
		if calls.removeErr != nil {
			return calls.removeErr
		}
		if err := task.RemoveTask(id); err != nil {
			return err
		}
		calls.writes++
		return nil
	}
	daemonTriggerTask = func(id string) error {
		calls.triggered = append(calls.triggered, id)
		return calls.runErr
	}
	daemonListTasksNoSpawn = func() ([]task.Task, error) {
		return nil, daemon.ErrDaemonUnavailable
	}
	t.Cleanup(func() {
		daemonAddTask = origAdd
		daemonUpdateTask = origUpdate
		daemonRemoveTask = origRemove
		daemonTriggerTask = origTrigger
		daemonListTasksNoSpawn = origList
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
	reset := func() {
		taskUpdateNameFlag = ""
		taskUpdatePromptFlag = ""
		taskUpdateCronFlag = ""
		taskUpdateWatchCmdFlag = ""
		taskUpdateTargetSessionFlag = ""
		taskUpdateEnabledFlag = ""
		taskUpdateProgramFlag = ""
		// --target-session uses Changed() semantics ("" is a meaningful
		// value), so the cobra-level Changed marker must reset too.
		tasksUpdateCmd.Flags().Lookup("target-session").Changed = false
	}
	t.Cleanup(reset)
	reset()
}

// resetAddFlags clears the package-level add flag variables so tests don't
// leak state into each other.
func resetAddFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		taskAddNameFlag = ""
		taskAddPromptFlag = ""
		taskAddCronFlag = ""
		taskAddWatchCmdFlag = ""
		taskAddTargetSessionFlag = ""
		taskAddProgramFlag = ""
		repoFlag = ""
	}
	t.Cleanup(reset)
	reset()
}

// captureStdout redirects os.Stdout while fn runs and returns what jsonOut
// printed, so a test can assert on the command's JSON output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	require.NoError(t, w.Close())
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(data)
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

func TestTasksAdd_PersistsTaskViaDaemon(t *testing.T) {
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

	assert.Equal(t, 1, calls.writes, "add must dispatch the write to the daemon")
}

// TestTasksAdd_DaemonWriteFailureFailsAdd pins the daemon-owned-write contract
// (#1029 PR 3): the daemon is now the sole task writer, so a failed AddTask RPC
// fails the CLI command — there is no client-side disk write to fall back to.
// This replaces the old eventual-consistency "failed reload poke is fine" test,
// which no longer applies now that the write itself goes through the daemon.
func TestTasksAdd_DaemonWriteFailureFailsAdd(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t)
	calls := stubDaemon(t)
	calls.addErr = errors.New("simulated daemon RPC failure")
	setupAddRepo(t)

	taskAddNameFlag = "rpc-fail"
	taskAddPromptFlag = "p"
	taskAddCronFlag = "0 9 * * *"
	taskAddProgramFlag = "claude"

	err := tasksAddCmd.RunE(tasksAddCmd, nil)
	require.Error(t, err, "a failed daemon write RPC must fail the add")
	assert.Contains(t, err.Error(), "failed to add task")
	assert.Zero(t, calls.writes, "no successful write is recorded when the RPC fails")
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

			assert.Zero(t, calls.writes, "no daemon write is dispatched when validation fails")

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
	assert.Zero(t, calls.writes)
}

// TestTasksAdd_InvalidRepoNamesPathNotRequired is half of the #892 regression:
// when --repo is provided but points at a non-git directory, the error must name
// the offending path and must NOT claim "--repo is required" — the user did
// provide it. Previously every resolveRepo() failure was relabeled "required",
// contradicting the user's own command.
func TestTasksAdd_InvalidRepoNamesPathNotRequired(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t)
	calls := stubDaemon(t)

	// An existing directory that is not a git repository.
	notARepo := t.TempDir()
	repoFlag = notARepo

	taskAddNameFlag = "x"
	taskAddPromptFlag = "do it"
	taskAddCronFlag = "0 9 * * *"
	taskAddProgramFlag = "claude"

	err := tasksAddCmd.RunE(tasksAddCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), notARepo, "error must name the invalid --repo path")
	assert.Contains(t, err.Error(), "not a valid git repository")
	assert.NotContains(t, err.Error(), "--repo is required", "must not claim --repo is missing when it was provided")
	assert.Zero(t, calls.writes, "no daemon write is dispatched when repo resolution fails")
}

// TestTasksAdd_AbsentRepoInNonRepoCwdSaysRequired is the other half of #892:
// with no --repo and a cwd that is not a git repo, the error must report that
// --repo is required (the only case where that wording is accurate).
func TestTasksAdd_AbsentRepoInNonRepoCwdSaysRequired(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t) // leaves repoFlag = ""
	stubDaemon(t)

	// cwd must be outside any git repo so CurrentRepo() fails.
	t.Chdir(t.TempDir())

	taskAddNameFlag = "x"
	taskAddPromptFlag = "do it"
	taskAddCronFlag = "0 9 * * *"
	taskAddProgramFlag = "claude"

	err := tasksAddCmd.RunE(tasksAddCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--repo is required")
}

func TestTasksUpdate_DisablePersistsViaDaemon(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "t1", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateEnabledFlag = "false"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t1"})
	require.NoError(t, err)

	got, err := task.GetTask("t1")
	require.NoError(t, err)
	assert.False(t, got.Enabled)
	assert.Equal(t, 1, calls.writes, "update must dispatch the write to the daemon")
}

func TestTasksUpdate_SwitchWatchToCronAndDisableWithoutPrompt(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{
		ID:       "watch1437",
		Name:     "watch",
		WatchCmd: "tail -f events.log",
		Enabled:  true,
	})

	taskUpdateCronFlag = "30 6 * * 1"
	taskUpdateEnabledFlag = "false"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"watch1437"})
	require.NoError(t, err)

	got, err := task.GetTask("watch1437")
	require.NoError(t, err)
	assert.Equal(t, "30 6 * * 1", got.CronExpr)
	assert.Empty(t, got.WatchCmd)
	assert.Empty(t, got.Prompt)
	assert.False(t, got.Enabled)
	assert.Equal(t, 1, calls.writes)
}

func TestTasksUpdate_CronChangePersistsViaDaemon(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "t2", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateCronFlag = "30 6 * * 1"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t2"})
	require.NoError(t, err)

	got, err := task.GetTask("t2")
	require.NoError(t, err)
	assert.Equal(t, "30 6 * * 1", got.CronExpr)
	assert.Equal(t, 1, calls.writes)
}

func TestTasksUpdate_RejectsInvalidCron(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "t3", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateCronFlag = "not a cron"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t3"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid cron expression")

	got, err := task.GetTask("t3")
	require.NoError(t, err)
	assert.Equal(t, "0 9 * * *", got.CronExpr, "cron must remain unchanged on validation failure")
	assert.Zero(t, calls.writes)
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
			assert.Zero(t, calls.writes)
		})
	}
}

func TestTasksUpdate_RejectsBlankWatchCmd(t *testing.T) {
	// Whitespace-only --watch-cmd used to pass the != "" presence check,
	// trim to "", and silently clear BOTH triggers. On disabled tasks
	// ValidateTrigger tolerates the no-trigger draft state, so the wipe
	// persisted with no error (#814).
	for _, enabled := range []bool{true, false} {
		for _, watch := range []string{"   ", "\t\n"} {
			t.Run(fmt.Sprintf("enabled=%v/watch=%q", enabled, watch), func(t *testing.T) {
				useTempConfig(t)
				resetUpdateFlags(t)
				calls := stubDaemon(t)

				seedTask(t, task.Task{
					ID:       "wb1",
					Name:     "nightly",
					Prompt:   "sweep",
					CronExpr: "0 3 * * *",
					Enabled:  enabled,
				})

				taskUpdateWatchCmdFlag = watch
				err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"wb1"})

				require.Error(t, err, "blank watch-cmd must be rejected")
				assert.Contains(t, err.Error(), "watch-cmd must be non-empty")

				got, err := task.GetTask("wb1")
				require.NoError(t, err)
				assert.Equal(t, "0 3 * * *", got.CronExpr, "existing trigger must survive a rejected update")
				assert.Empty(t, got.WatchCmd)
				assert.Zero(t, calls.writes)
			})
		}
	}
}

func TestTasksUpdate_RejectsBlankCron(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, cron := range []string{"   ", "\t\n"} {
			t.Run(fmt.Sprintf("enabled=%v/cron=%q", enabled, cron), func(t *testing.T) {
				useTempConfig(t)
				resetUpdateFlags(t)
				calls := stubDaemon(t)

				seedTask(t, task.Task{
					ID:       "cb1",
					Name:     "log watch",
					WatchCmd: "tail -f errors.log",
					Enabled:  enabled,
				})

				taskUpdateCronFlag = cron
				err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"cb1"})

				require.Error(t, err, "blank cron must be rejected")
				assert.Contains(t, err.Error(), "cron expression must be non-empty")

				got, err := task.GetTask("cb1")
				require.NoError(t, err)
				assert.Equal(t, "tail -f errors.log", got.WatchCmd, "existing trigger must survive a rejected update")
				assert.Empty(t, got.CronExpr)
				assert.Zero(t, calls.writes)
			})
		}
	}
}

func TestTasksUpdate_TrimsCronAndWatchCmd(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	stubDaemon(t)

	seedTask(t, task.Task{ID: "tr1", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateWatchCmdFlag = "  tail -f errors.log  "
	require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"tr1"}))
	got, err := task.GetTask("tr1")
	require.NoError(t, err)
	assert.Equal(t, "tail -f errors.log", got.WatchCmd, "watch-cmd must be stored trimmed, matching the add path")

	resetUpdateFlags(t)
	taskUpdateCronFlag = "  30 6 * * 1  "
	require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"tr1"}))
	got, err = task.GetTask("tr1")
	require.NoError(t, err)
	assert.Equal(t, "30 6 * * 1", got.CronExpr, "cron must be stored trimmed, matching the add path")
	assert.Empty(t, got.WatchCmd)
}

func TestTasksUpdate_RejectsBadEnabledValue(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "t4", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateEnabledFlag = "yes"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"t4"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--enabled must be 'true' or 'false'")
	assert.Zero(t, calls.writes)
}

func TestTasksRemove_RemovesTaskViaDaemon(t *testing.T) {
	useTempConfig(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "t5", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	err := tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"t5"})
	require.NoError(t, err)

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	assert.Empty(t, tasks)
	assert.Equal(t, 1, calls.writes, "remove must dispatch the write to the daemon")
}

func TestTasksRemove_MissingTaskErrors(t *testing.T) {
	useTempConfig(t)
	calls := stubDaemon(t)

	err := tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"nope1234"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Zero(t, calls.writes, "no daemon write is dispatched when nothing changed")
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

// TestTasksAdd_ExactlyOneTrigger pins the #782 CLI contract: --cron and
// --watch-cmd are mutually exclusive and one of them is required.
func TestTasksAdd_ExactlyOneTrigger(t *testing.T) {
	cases := []struct {
		name  string
		cron  string
		watch string
	}{
		{"neither", "", ""},
		{"both", "0 3 * * *", "tail -f log"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useTempConfig(t)
			resetAddFlags(t)
			calls := stubDaemon(t)
			setupAddRepo(t)

			taskAddNameFlag = "trigger-shape"
			taskAddPromptFlag = "p"
			taskAddCronFlag = tc.cron
			taskAddWatchCmdFlag = tc.watch

			err := tasksAddCmd.RunE(tasksAddCmd, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "exactly one of --cron or --watch-cmd")
			assert.Zero(t, calls.writes)

			tasks, err := task.LoadTasks()
			require.NoError(t, err)
			assert.Empty(t, tasks)
		})
	}
}

// TestTasksAdd_WatchTaskAllowsEmptyPrompt verifies the watch-task defaults:
// no prompt is fine (each event delivers the raw emitted line) and the new
// fields persist.
func TestTasksAdd_WatchTaskAllowsEmptyPrompt(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t)
	calls := stubDaemon(t)
	setupAddRepo(t)

	taskAddNameFlag = "gh-issues"
	taskAddWatchCmdFlag = "gh-issue-watch.sh"
	taskAddTargetSessionFlag = "captain"
	taskAddProgramFlag = "claude"

	err := tasksAddCmd.RunE(tasksAddCmd, nil)
	require.NoError(t, err)

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "gh-issue-watch.sh", tasks[0].WatchCmd)
	assert.Equal(t, "captain", tasks[0].TargetSession)
	assert.Empty(t, tasks[0].CronExpr)
	assert.Empty(t, tasks[0].Prompt)
	assert.True(t, tasks[0].Enabled)
	assert.Equal(t, 1, calls.writes, "add must dispatch the write to the daemon so the watcher starts")
}

// TestTasksUpdate_SwitchingTriggersClearsTheOther verifies that setting one
// trigger via update clears the other, keeping the exactly-one invariant.
func TestTasksUpdate_SwitchingTriggersClearsTheOther(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	stubDaemon(t)

	seedTask(t, task.Task{ID: "sw1", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	// cron → watch
	taskUpdateWatchCmdFlag = "tail -f errors.log"
	require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"sw1"}))
	got, err := task.GetTask("sw1")
	require.NoError(t, err)
	assert.Equal(t, "tail -f errors.log", got.WatchCmd)
	assert.Empty(t, got.CronExpr, "switching to watch must clear cron")

	// watch → cron
	resetUpdateFlags(t)
	taskUpdateCronFlag = "0 4 * * *"
	require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"sw1"}))
	got, err = task.GetTask("sw1")
	require.NoError(t, err)
	assert.Equal(t, "0 4 * * *", got.CronExpr)
	assert.Empty(t, got.WatchCmd, "switching to cron must clear watch-cmd")
}

// TestTasksUpdate_RejectsBothTriggerFlags pins that one update cannot set
// both triggers at once.
func TestTasksUpdate_RejectsBothTriggerFlags(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	stubDaemon(t)

	seedTask(t, task.Task{ID: "bt1", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	taskUpdateCronFlag = "0 4 * * *"
	taskUpdateWatchCmdFlag = "tail -f x"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"bt1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// TestTasksUpdate_SwitchToCronRequiresPrompt covers the watch→cron edge: a
// watch task may have no prompt, but a cron task cannot run without one.
func TestTasksUpdate_SwitchToCronRequiresPrompt(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	stubDaemon(t)

	seedTask(t, task.Task{ID: "sc1", WatchCmd: "tail -f x", Enabled: true})

	taskUpdateCronFlag = "0 4 * * *"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"sc1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs a prompt")

	// With a prompt in the same update it succeeds.
	resetUpdateFlags(t)
	taskUpdateCronFlag = "0 4 * * *"
	taskUpdatePromptFlag = "scheduled sweep"
	require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"sc1"}))
}

// TestTasksUpdate_TargetSessionSetAndClear pins the Changed() semantics:
// --target-session is applied when given — including an explicit empty value,
// which reverts to a-new-session-per-run — and left untouched when absent.
func TestTasksUpdate_TargetSessionSetAndClear(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	stubDaemon(t)

	seedTask(t, task.Task{ID: "ts1", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	// Set it.
	require.NoError(t, tasksUpdateCmd.Flags().Set("target-session", "captain"))
	require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"ts1"}))
	got, err := task.GetTask("ts1")
	require.NoError(t, err)
	assert.Equal(t, "captain", got.TargetSession)

	// An unrelated update leaves it alone.
	resetUpdateFlags(t)
	taskUpdateNameFlag = "renamed"
	require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"ts1"}))
	got, err = task.GetTask("ts1")
	require.NoError(t, err)
	assert.Equal(t, "captain", got.TargetSession, "absent flag must not clear target_session")

	// An explicit empty value clears it.
	resetUpdateFlags(t)
	require.NoError(t, tasksUpdateCmd.Flags().Set("target-session", ""))
	require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"ts1"}))
	got, err = task.GetTask("ts1")
	require.NoError(t, err)
	assert.Empty(t, got.TargetSession, "explicit empty value must clear target_session")
}

// TestTasksUpdate_ProgramChange is the regression guard for #866: the CLI
// `tasks update` gained a --program flag so the program field is editable from
// the CLI, matching the TUI and the backend that already persisted it.
func TestTasksUpdate_ProgramChange(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "pr1", Prompt: "p", CronExpr: "0 9 * * *", Program: "claude", Enabled: true})

	out := captureStdout(t, func() {
		taskUpdateProgramFlag = "codex"
		require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"pr1"}))
	})

	got, err := task.GetTask("pr1")
	require.NoError(t, err)
	assert.Equal(t, "codex", got.Program, "program must persist through update")
	assert.Equal(t, 1, calls.writes, "update must dispatch the write to the daemon")
	assert.Contains(t, out, `"program": "codex"`, "JSON output must reflect the new program")
}

// TestTasksUpdate_RejectsInvalidProgram pins that --program is validated
// against the same enum the add path uses, and a bad value leaves the task
// untouched.
func TestTasksUpdate_RejectsInvalidProgram(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "pr2", Prompt: "p", CronExpr: "0 9 * * *", Program: "claude", Enabled: true})

	taskUpdateProgramFlag = "not-a-real-agent"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"pr2"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--program flag must be one of")

	got, err := task.GetTask("pr2")
	require.NoError(t, err)
	assert.Equal(t, "claude", got.Program, "program must remain unchanged on validation failure")
	assert.Zero(t, calls.writes, "no daemon write is dispatched when validation fails")
}

// TestTasksUpdate_OmittingProgramLeavesUnchanged pins the partial-update
// contract: an update that does not pass --program must not touch the program.
func TestTasksUpdate_OmittingProgramLeavesUnchanged(t *testing.T) {
	useTempConfig(t)
	resetUpdateFlags(t)
	stubDaemon(t)

	seedTask(t, task.Task{ID: "pr3", Prompt: "p", CronExpr: "0 9 * * *", Program: "aider", Enabled: true})

	taskUpdateNameFlag = "renamed"
	require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"pr3"}))

	got, err := task.GetTask("pr3")
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Name)
	assert.Equal(t, "aider", got.Program, "absent --program must not change the program")
}

// --- Read path: list/get non-spawn + disk fallback (#1029 PR 3) ---

// TestTasksList_FallsBackToDiskWhenNoDaemon pins that `tasks list` reads the
// disk when no daemon is reachable — the stub returns ErrDaemonUnavailable by
// default — so the command keeps working in scripts/CI with no daemon running
// and never spawns one.
func TestTasksList_FallsBackToDiskWhenNoDaemon(t *testing.T) {
	useTempConfig(t)
	stubDaemon(t) // daemonListTasksNoSpawn defaults to ErrDaemonUnavailable
	seedTask(t, task.Task{ID: "d1", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	tasks, err := listTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1, "list must fall back to the on-disk task")
	assert.Equal(t, "d1", tasks[0].ID)
}

// TestTasksList_PrefersDaemonSnapshot pins that when a daemon IS reachable the
// live snapshot is authoritative — even when it diverges from disk — so the CLI
// mirrors the daemon's view rather than a stale disk read.
func TestTasksList_PrefersDaemonSnapshot(t *testing.T) {
	useTempConfig(t)
	stubDaemon(t)
	// Disk holds one task; the daemon reports a DIFFERENT one.
	seedTask(t, task.Task{ID: "ondisk", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})
	daemonListTasksNoSpawn = func() ([]task.Task, error) {
		return []task.Task{{ID: "live", Prompt: "p", CronExpr: "0 1 * * *", Enabled: true}}, nil
	}

	tasks, err := listTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "live", tasks[0].ID, "a reachable daemon's snapshot is authoritative")
}

// TestTasksGet_FallsBackToDiskWhenNoDaemon pins the get read path's disk
// fallback and its not-found behavior against the seeded disk state.
func TestTasksGet_FallsBackToDiskWhenNoDaemon(t *testing.T) {
	useTempConfig(t)
	stubDaemon(t)
	seedTask(t, task.Task{ID: "g1", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	got, err := getTaskByID("g1")
	require.NoError(t, err)
	assert.Equal(t, "g1", got.ID)

	_, err = getTaskByID("missing1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestTasksGet_PrefersDaemonSnapshot pins that a reachable daemon is
// authoritative for get: a match comes from the live snapshot, and a miss
// returns not-found WITHOUT re-reading disk (even though disk holds the id).
func TestTasksGet_PrefersDaemonSnapshot(t *testing.T) {
	useTempConfig(t)
	stubDaemon(t)
	// Disk holds "shadow"; the daemon does NOT report it.
	seedTask(t, task.Task{ID: "shadow", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})
	daemonListTasksNoSpawn = func() ([]task.Task, error) {
		return []task.Task{{ID: "live", Prompt: "p", CronExpr: "0 1 * * *", Enabled: true}}, nil
	}

	got, err := getTaskByID("live")
	require.NoError(t, err)
	assert.Equal(t, "live", got.ID)

	// A daemon miss is authoritative — no disk fallback even though "shadow"
	// exists on disk.
	_, err = getTaskByID("shadow")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found",
		"a reachable daemon's miss is authoritative; get must not re-read disk")
}

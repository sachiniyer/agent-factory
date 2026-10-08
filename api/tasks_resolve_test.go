package api

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/task"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The id-taking task verbs resolve the argument as an id OR a name (#4676):
// `af tasks list` and every UI show the NAME prominently, but until this
// change `af tasks show hello-cron` failed with "task with id not found".
//
// The contract each test below pins, because each clause is a way to get it
// wrong:
//
//   - An exact id match always wins — even over another task's name spelled
//     the same — so a name can never shadow an id.
//   - An exact name resolves only when exactly ONE task in scope carries it;
//     several matches are refused with their ids, never acted on at random.
//   - The scope is the same one an id gets: --repo wins, else the cwd's
//     project, else rule-3 global resolution with ambiguity refused.
//   - A miss keeps the old error shape while naming both things searched:
//     "task with id or name <arg> not found".
//   - `remove`/`update` get exactly this resolution — a wrong pick there is a
//     real deletion, not a wrong printout.
//
// Every test uses stubDaemon + seedTask: the read path falls back to the disk
// the tests seed, and the write stubs perform the real store operation, so no
// daemon is spawned.

// TestTasksResolve_NameResolvesForEveryVerb runs all six id-taking verbs by
// name and asserts each acted on the task the name belongs to.
func TestTasksResolve_NameResolvesForEveryVerb(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)

	seedTask(t, task.Task{ID: "a1b2c3d4", Name: "hello-cron", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})
	seedTask(t, task.Task{ID: "w9w9w9w9", Name: "watcher", WatchCmd: "./poll.sh", Enabled: true})

	// get
	out := captureJSON(t, func() error {
		return tasksGetCmd.RunE(tasksGetCmd, []string{"hello-cron"})
	})
	var got task.Task
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "a1b2c3d4", got.ID, "a name must resolve to the task that owns it")

	// show
	var rendered bytes.Buffer
	tasksShowCmd.SetOut(&rendered)
	t.Cleanup(func() { tasksShowCmd.SetOut(nil) })
	require.NoError(t, tasksShowCmd.RunE(tasksShowCmd, []string{"watcher"}))
	assert.Contains(t, rendered.String(), "w9w9w9w9")

	// trigger
	require.NoError(t, tasksRunCmd.RunE(tasksRunCmd, []string{"hello-cron"}))
	require.Equal(t, []string{"a1b2c3d4"}, calls.triggered,
		"triggering by name must dispatch the resolved id, not the name")

	// restart
	require.NoError(t, tasksRestartCmd.RunE(tasksRestartCmd, []string{"watcher"}))
	require.Equal(t, []string{"w9w9w9w9"}, calls.restarted)

	// update
	taskUpdateNameFlag = "renamed-cron"
	require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"hello-cron"}))
	require.NotNil(t, calls.lastUpdate.Name)
	assert.Equal(t, "renamed-cron", *calls.lastUpdate.Name)
	stored, err := task.GetTask("a1b2c3d4")
	require.NoError(t, err)
	assert.Equal(t, "renamed-cron", stored.Name)

	// remove, by the task's NEW name — renaming must not strand it.
	require.NoError(t, tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"renamed-cron"}))
	left, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, left, 1)
	assert.Equal(t, "w9w9w9w9", left[0].ID,
		"remove-by-name must delete the resolved task and only that one")
}

// TestTasksResolve_IDInputUnchanged pins the id path inside the shared
// resolver: the record an id fetches is byte-identical to what the same id
// produced before names were accepted, and the same record a name fetches.
func TestTasksResolve_IDInputUnchanged(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	seedTask(t, task.Task{ID: "a1b2c3d4", Name: "hello-cron", Prompt: "say hi", CronExpr: "0 9 * * *", Enabled: true})

	byID := captureJSON(t, func() error {
		return tasksGetCmd.RunE(tasksGetCmd, []string{"a1b2c3d4"})
	})
	byName := captureJSON(t, func() error {
		return tasksGetCmd.RunE(tasksGetCmd, []string{"hello-cron"})
	})
	assert.Equal(t, string(byID), string(byName),
		"resolving by name must produce the same record as the id")

	var got task.Task
	require.NoError(t, json.Unmarshal(byID, &got))
	assert.Equal(t, "a1b2c3d4", got.ID)
	assert.Equal(t, "say hi", got.Prompt)
}

// TestTasksResolve_NonIDShapedNameResolves: a name that could never be an id —
// it fails task.ValidateTaskID's character class — still resolves, which is
// the whole point of the feature: display names carry spaces and capitals.
func TestTasksResolve_NonIDShapedNameResolves(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	seedTask(t, task.Task{ID: "a1b2c3d4", Name: "Daily triage", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	out := captureJSON(t, func() error {
		return tasksGetCmd.RunE(tasksGetCmd, []string{"Daily triage"})
	})
	var got task.Task
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "a1b2c3d4", got.ID)
}

// TestTasksResolve_AmbiguousNameRefused pins the fail-closed rule: a name two
// in-scope tasks share is refused, the error lists BOTH ids so the caller can
// pick one, and a destructive verb reaches no daemon call at all.
func TestTasksResolve_AmbiguousNameRefused(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	calls := stubDaemon(t)

	// Both bound to the cwd's project so the "in this project" wording holds:
	// the api test binary's cwd IS inside a git repo (this one).
	cwd := mkRepo(t, "here")
	t.Chdir(cwd)
	seedTask(t, task.Task{ID: "aaaa1111", Name: "nightly", Prompt: "p", CronExpr: "0 9 * * *", ProjectPath: cwd, Enabled: true})
	seedTask(t, task.Task{ID: "bbbb2222", Name: "nightly", Prompt: "p", CronExpr: "0 3 * * *", ProjectPath: cwd, Enabled: true})

	err := tasksGetCmd.RunE(tasksGetCmd, []string{"nightly"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `task name "nightly" matches 2 tasks in this project`)
	assert.Contains(t, err.Error(), "aaaa1111")
	assert.Contains(t, err.Error(), "bbbb2222",
		"an ambiguous name must list every match's id so the caller can pick one")

	err = tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"nightly"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aaaa1111")
	assert.Equal(t, 0, calls.writes,
		"an ambiguous name must be refused BEFORE any mutation is dispatched")

	tasks, lerr := task.LoadTasks()
	require.NoError(t, lerr)
	assert.Len(t, tasks, 2, "nothing may be removed on an ambiguous name")
}

// TestTasksResolve_ExactIDWinsOverCollidingName is the misroute guarantee: a
// task literally NAMED like another task's id cannot redirect the command —
// the id is tried first, globally, and only a miss falls through to names.
func TestTasksResolve_ExactIDWinsOverCollidingName(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)

	// "bbbb2222" is a valid task id AND a legal name; the name owner must lose.
	seedTask(t, task.Task{ID: "zzzz9999", Name: "bbbb2222", Prompt: "decoy", CronExpr: "0 9 * * *", Enabled: true})
	seedTask(t, task.Task{ID: "bbbb2222", Name: "real-owner", Prompt: "real", CronExpr: "0 3 * * *", Enabled: true})

	out := captureJSON(t, func() error {
		return tasksGetCmd.RunE(tasksGetCmd, []string{"bbbb2222"})
	})
	var got task.Task
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "bbbb2222", got.ID)
	assert.Equal(t, "real-owner", got.Name,
		"the id owner must win; the task merely named bbbb2222 is the decoy")

	// The destructive path obeys the same precedence.
	require.NoError(t, tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"bbbb2222"}))
	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "zzzz9999", tasks[0].ID,
		"remove must delete the id owner, never the task that shares its spelling")
}

// TestTasksResolve_UnknownNameKeepsErrorShape pins the miss wording: the same
// not-found shape as before, extended to name both things that were searched
// — the id and the name — so a script parsing "not found" still matches and a
// human learns the arg can be either.
func TestTasksResolve_UnknownNameKeepsErrorShape(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	seedTask(t, task.Task{ID: "a1b2c3d4", Name: "hello-cron", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})

	for _, verb := range []struct {
		cmd  *cobra.Command
		args []string
	}{
		{tasksGetCmd, []string{"nosuch"}},
		{tasksRemoveCmd, []string{"nosuch"}},
	} {
		err := verb.cmd.RunE(verb.cmd, verb.args)
		require.Error(t, err)
		assert.Equal(t, `failed to get task: task with id or name "nosuch" not found`, err.Error())
	}
}

// TestTasksResolve_NameFollowsProjectScope pins that a name resolves inside
// the SAME scope an id gets: a duplicate across projects is narrowed by the
// cwd's project, a name held only elsewhere is refused with the owning
// project and its --repo, and an explicit --repo reaches it.
func TestTasksResolve_NameFollowsProjectScope(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)

	alpha := mkRepo(t, "alpha")
	beta := mkRepo(t, "beta")
	seedTask(t, task.Task{ID: "aaaa1111", Name: "nightly", Prompt: "p", CronExpr: "0 9 * * *", ProjectPath: alpha, Enabled: true})
	seedTask(t, task.Task{ID: "bbbb2222", Name: "nightly", Prompt: "p", CronExpr: "0 3 * * *", ProjectPath: beta, Enabled: true})
	seedTask(t, task.Task{ID: "cccc3333", Name: "beta-only", Prompt: "p", CronExpr: "0 5 * * *", ProjectPath: beta, Enabled: true})
	t.Chdir(alpha)

	// The in-scope match wins over a foreign same-named task — one match in
	// scope is unambiguous.
	out := captureJSON(t, func() error {
		return tasksGetCmd.RunE(tasksGetCmd, []string{"nightly"})
	})
	var got task.Task
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "aaaa1111", got.ID,
		"a name held once in scope resolves there even when another project also holds it")

	// A name held ONLY by another project is refused, naming the project, the
	// id it resolved to, and the --repo that would reach it.
	err := tasksGetCmd.RunE(tasksGetCmd, []string{"beta-only"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "task named \"beta-only\"")
	assert.Contains(t, err.Error(), "cccc3333")
	assert.Contains(t, err.Error(), beta)
	assert.Contains(t, err.Error(), "--repo")

	// The named project reaches it.
	repoFlag = beta
	out = captureJSON(t, func() error {
		return tasksGetCmd.RunE(tasksGetCmd, []string{"beta-only"})
	})
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "cccc3333", got.ID)
	repoFlag = ""
}

// TestTasksResolve_NoProjectContext pins rule 3 for names: outside a git
// repository a name held by one task resolves globally, and one held by
// several is refused listing the ids AND projects — never a pick at random.
func TestTasksResolve_NoProjectContext(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)

	alpha := mkRepo(t, "alpha")
	beta := mkRepo(t, "beta")
	seedTask(t, task.Task{ID: "aaaa1111", Name: "nightly", Prompt: "p", CronExpr: "0 9 * * *", ProjectPath: alpha, Enabled: true})
	seedTask(t, task.Task{ID: "bbbb2222", Name: "nightly", Prompt: "p", CronExpr: "0 3 * * *", ProjectPath: beta, Enabled: true})
	seedTask(t, task.Task{ID: "dddd4444", Name: "solo", Prompt: "p", CronExpr: "0 5 * * *", ProjectPath: beta, Enabled: true})
	t.Chdir(t.TempDir()) // not a git repository

	out := captureJSON(t, func() error {
		return tasksGetCmd.RunE(tasksGetCmd, []string{"solo"})
	})
	var got task.Task
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "dddd4444", got.ID)

	err := tasksGetCmd.RunE(tasksGetCmd, []string{"nightly"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `task name "nightly" matches 2 tasks:`)
	assert.Contains(t, err.Error(), "aaaa1111")
	assert.Contains(t, err.Error(), "bbbb2222")
	assert.Contains(t, err.Error(), alpha)
	assert.Contains(t, err.Error(), beta)
}

// TestTasksResolve_DaemonSnapshotAuthoritativeForNames: when a daemon is
// reachable its list is the truth the name pass searches, so a name present
// only on disk does NOT resolve — the same authority the id pass already had.
func TestTasksResolve_DaemonSnapshotAuthoritativeForNames(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	seedTask(t, task.Task{ID: "ondisk00", Name: "disk-only", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})
	daemonListTasksNoSpawn = func() ([]task.Task, error) {
		return []task.Task{{ID: "live0000", Name: "live-name", Prompt: "p", CronExpr: "0 1 * * *", Enabled: true}}, nil
	}

	out := captureJSON(t, func() error {
		return tasksGetCmd.RunE(tasksGetCmd, []string{"live-name"})
	})
	var got task.Task
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "live0000", got.ID)

	err := tasksGetCmd.RunE(tasksGetCmd, []string{"disk-only"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found",
		"a reachable daemon's miss is authoritative; names must not re-read disk")
}

// TestTasksResolve_RemoteNameResolves: against a remote daemon the name pass
// searches THAT daemon's list — never this machine's store — and a mutation
// by name dispatches the resolved id to the same host.
func TestTasksResolve_RemoteNameResolves(t *testing.T) {
	for _, spelling := range remoteTargetNames {
		t.Run(spelling, func(t *testing.T) {
			home, stub := setupRemoteCase(t, spelling)

			out := captureStdout(t, func() {
				require.NoError(t, tasksGetCmd.RunE(tasksGetCmd, []string{stubDaemonTaskName}))
			})
			require.Contains(t, out, stubDaemonTaskID,
				"a remote name must resolve to the remote task's id")
			requireLocalStoreUntouched(t, home)

			captureStdout(t, func() {
				require.NoError(t, tasksRemoveCmd.RunE(tasksRemoveCmd, []string{stubDaemonTaskName}))
			})
			removes := stub.snapshot().removes
			require.Len(t, removes, 1, "the targeted daemon must receive exactly one remove")
			require.Equal(t, stubDaemonTaskID, removes[0].ID,
				"the daemon must be sent the resolved id, not the name")
			requireLocalStoreUntouched(t, home)

			// A name only the LOCAL store holds must not resolve remotely.
			err := tasksGetCmd.RunE(tasksGetCmd, []string{localTaskName})
			require.Error(t, err)
			require.Contains(t, err.Error(), "not found")
		})
	}
}

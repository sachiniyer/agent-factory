package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These pin the ONE project-context contract (#1893/#1891): every session and
// task command defaults to the current directory's project, cross-project work
// takes an explicit --repo, and only read-only listing widens via --all.
//
// Before this, `af tasks list` listed every project's tasks and
// `af tasks get/update/remove/trigger` accepted --repo and silently discarded
// it — so an id was a capability to mutate any project's automation from
// anywhere. Each test below fails against that behavior.

// mkRepo creates a throwaway git repository and returns its root, resolved
// through symlinks so it compares equal to what git reports from inside it (on
// macOS t.TempDir() hands back a /var → /private/var symlink).
func mkRepo(t *testing.T, name string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), name)
	require.NoError(t, exec.Command("git", "init", repo).Run(), "git init %s", name)
	require.NoError(t, exec.Command("git", "-C", repo, "config", "user.email", "test@example.com").Run())
	require.NoError(t, exec.Command("git", "-C", repo, "config", "user.name", "Test User").Run())
	require.NoError(t, exec.Command("git", "-C", repo, "commit", "--allow-empty", "-m", "init").Run())
	real, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	return real
}

// seedTasksInTwoProjects puts one task in each of two projects, then chdirs into
// alpha so the cwd resolves to it.
func seedTasksInTwoProjects(t *testing.T) (alpha, beta string) {
	t.Helper()
	alpha = mkRepo(t, "alpha")
	beta = mkRepo(t, "beta")
	seedTask(t, task.Task{ID: "aaaa1111", Name: "alpha-task", Prompt: "p", CronExpr: "0 9 * * *", ProjectPath: alpha, Enabled: true})
	seedTask(t, task.Task{ID: "bbbb2222", Name: "beta-task", Prompt: "p", CronExpr: "0 9 * * *", ProjectPath: beta, Enabled: true})
	t.Chdir(alpha)
	return alpha, beta
}

// TestTasksList_DefaultsToCurrentProject is the headline default change: from
// inside a repository, `af tasks list` shows only that project's tasks.
func TestTasksList_DefaultsToCurrentProject(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	_, _ = seedTasksInTwoProjects(t)

	out := captureTasksList(t)
	require.Len(t, out, 1, "a scoped list must not include the other project's task")
	assert.Equal(t, "aaaa1111", out[0].ID)
}

// TestTasksList_AllSpansEveryProject pins the explicit opt-in that restores the
// old breadth.
func TestTasksList_AllSpansEveryProject(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	seedTasksInTwoProjects(t)

	tasksListAllFlag = true
	out := captureTasksList(t)
	require.Len(t, out, 2, "--all must span every project")
}

// TestTasksList_RepoScopesToNamedProject pins cross-project listing via --repo
// from inside a different project.
func TestTasksList_RepoScopesToNamedProject(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	_, beta := seedTasksInTwoProjects(t)

	repoFlag = beta
	out := captureTasksList(t)
	require.Len(t, out, 1)
	assert.Equal(t, "bbbb2222", out[0].ID, "--repo must scope to the named project, not the cwd's")
}

// TestTasksList_OutsideRepoListsEveryProject pins rule 3: with no project
// context there is nothing to scope to, so breadth is honest rather than a
// guess. This keeps `af tasks list` working from a systemd unit or CI step.
func TestTasksList_OutsideRepoListsEveryProject(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	seedTasksInTwoProjects(t)

	t.Chdir(t.TempDir()) // not a git repository
	out := captureTasksList(t)
	require.Len(t, out, 2)
}

// TestTasksList_RepoAndAllAreMutuallyExclusive: --repo names one project and
// --all spans every project, so passing both is a contradiction, not a
// precedence puzzle.
func TestTasksList_RepoAndAllAreMutuallyExclusive(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	alpha, _ := seedTasksInTwoProjects(t)

	repoFlag = alpha
	tasksListAllFlag = true
	err := tasksListCmd.RunE(tasksListCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// TestTasksList_MatchesProjectByIdentityNotPathString covers the TUI/CLI storage
// split: the CLI stores a task's project as the git main-worktree root, the TUI
// stores whatever absolute path the user typed. A subdirectory names the same
// project and must not be filtered out — string equality would hide the task
// from its own project's list.
func TestTasksList_MatchesProjectByIdentityNotPathString(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)

	alpha := mkRepo(t, "alpha")
	sub := filepath.Join(alpha, "nested", "dir")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	// A TUI-created task pointed at a subdirectory of the project.
	seedTask(t, task.Task{ID: "aaaa1111", Name: "sub-task", Prompt: "p", CronExpr: "0 9 * * *", ProjectPath: sub, Enabled: true})
	t.Chdir(alpha)

	out := captureTasksList(t)
	require.Len(t, out, 1, "a task pointed at a subdirectory belongs to that project")
}

// TestTasksList_UnboundTaskStaysVisible: a task with no project binding cannot
// run (the daemon rejects a non-repo ProjectPath) and no supported path creates
// one, but scoping it out would strand it — invisible everywhere, deletable
// nowhere. It matches every scope so it can be seen and removed.
func TestTasksList_UnboundTaskStaysVisible(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)

	alpha := mkRepo(t, "alpha")
	seedTask(t, task.Task{ID: "cccc3333", Name: "orphan", Prompt: "p", CronExpr: "0 9 * * *", Enabled: true})
	t.Chdir(alpha)

	out := captureTasksList(t)
	require.Len(t, out, 1, "an unbound task must stay reachable")
}

// TestTasksRemove_RefusesOtherProjectsTask is the #1893 mutation guard. Against
// the old code this passed --repo, had it discarded, deleted beta's task and
// reported {"ok":true}.
func TestTasksRemove_RefusesOtherProjectsTask(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	calls := stubDaemon(t)
	seedTasksInTwoProjects(t) // cwd = alpha

	err := tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"bbbb2222"})
	require.Error(t, err, "an id owned by another project must not be removable from here")
	assert.Contains(t, err.Error(), "belongs to project")
	assert.Contains(t, err.Error(), "--repo", "the error must name the flag that would authorize it")
	assert.Zero(t, calls.writes, "the destructive RPC must not fire")

	// And the task really is still there.
	got, err := task.GetTask("bbbb2222")
	require.NoError(t, err)
	require.NotNil(t, got)
}

// TestTasksRemove_AllowsCurrentProjectsTask pins that the guard does not block
// the normal path.
func TestTasksRemove_AllowsCurrentProjectsTask(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	calls := stubDaemon(t)
	seedTasksInTwoProjects(t) // cwd = alpha

	require.NoError(t, tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"aaaa1111"}))
	assert.Equal(t, 1, calls.writes)
}

// TestTasksRemove_ExplicitRepoReachesOtherProject pins the escape hatch: naming
// the project authorizes the cross-project mutation.
func TestTasksRemove_ExplicitRepoReachesOtherProject(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	calls := stubDaemon(t)
	_, beta := seedTasksInTwoProjects(t) // cwd = alpha

	repoFlag = beta
	require.NoError(t, tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"bbbb2222"}))
	assert.Equal(t, 1, calls.writes)
}

// TestTasksRemove_OutsideRepoResolvesGlobally pins rule 3 for an id: with no
// project context the id still resolves, so scripts outside a repo keep working.
func TestTasksRemove_OutsideRepoResolvesGlobally(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	calls := stubDaemon(t)
	seedTasksInTwoProjects(t)

	t.Chdir(t.TempDir()) // not a git repository
	require.NoError(t, tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"bbbb2222"}))
	assert.Equal(t, 1, calls.writes)
}

// TestTasksGet_RefusesOtherProjectsTask — inspection is scoped too, so an id
// from another project does not leak its prompt/schedule.
func TestTasksGet_RefusesOtherProjectsTask(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	seedTasksInTwoProjects(t) // cwd = alpha

	err := tasksGetCmd.RunE(tasksGetCmd, []string{"bbbb2222"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to project")
}

// TestTasksTrigger_RefusesOtherProjectsTask — triggering another project's
// automation is the loudest cross-project mutation of all: it starts an agent.
func TestTasksTrigger_RefusesOtherProjectsTask(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	calls := stubDaemon(t)
	seedTasksInTwoProjects(t) // cwd = alpha

	err := tasksRunCmd.RunE(tasksRunCmd, []string{"bbbb2222"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to project")
	assert.Empty(t, calls.triggered, "the trigger RPC must not fire")
}

// TestTasksUpdate_RefusesOtherProjectsTask
func TestTasksUpdate_RefusesOtherProjectsTask(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)
	seedTasksInTwoProjects(t) // cwd = alpha

	taskUpdateEnabledFlag = "false"
	err := tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"bbbb2222"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to project")
	assert.Zero(t, calls.writes, "the update RPC must not fire")
}

// TestTasksAdd_EchoesResolvedProject is #1891's visibility half: the successful
// result names the project the task bound to, so a wrong binding is legible at
// creation instead of surfacing later as worktrees in the wrong place.
func TestTasksAdd_EchoesResolvedProject(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t)
	resetScopeFlags(t)
	stubDaemon(t)
	repo := setupAddRepo(t)

	taskAddNameFlag = "nightly"
	taskAddPromptFlag = "do the nightly sweep"
	taskAddCronFlag = "0 3 * * *"

	out := captureJSON(t, func() error { return tasksAddCmd.RunE(tasksAddCmd, nil) })

	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	require.NotEmpty(t, got["id"], "the id must still be reported")
	assert.Equal(t, repo, got["project_path"], "tasks add must report the resolved project binding")
}

// TestTasksAdd_RefusesCloneInsideAfHome is #1891's guard half. The DLQ agent
// cloned into $AGENT_FACTORY_HOME/runtime/<name> and ran `af tasks add` there;
// the task bound to the clone, so every watcher-created worktree attached to it
// and the automation was invisible from the intended project.
func TestTasksAdd_RefusesCloneInsideAfHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	resetAddFlags(t)
	resetScopeFlags(t)
	stubDaemon(t)

	// A full clone living inside af's own home — its OWN main root, not a
	// linked worktree of a real project.
	clone := filepath.Join(home, "runtime", "detail-dlq-monitor")
	require.NoError(t, os.MkdirAll(filepath.Dir(clone), 0o755))
	require.NoError(t, exec.Command("git", "init", clone).Run())
	require.NoError(t, exec.Command("git", "-C", clone, "config", "user.email", "t@e.com").Run())
	require.NoError(t, exec.Command("git", "-C", clone, "config", "user.name", "T").Run())
	require.NoError(t, exec.Command("git", "-C", clone, "commit", "--allow-empty", "-m", "init").Run())
	t.Chdir(clone)

	taskAddNameFlag = "dlq-watch"
	taskAddPromptFlag = "check the dlq"
	taskAddCronFlag = "*/5 * * * *"

	err := tasksAddCmd.RunE(tasksAddCmd, nil)
	require.Error(t, err, "a cwd-derived binding to a clone inside af's home must not silently create an automation project")
	assert.Contains(t, err.Error(), "--repo", "the error must name the escape hatch")
	assert.Contains(t, strings.ToLower(err.Error()), "af's home")

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	assert.Empty(t, tasks, "no task may be created by the refused add")
}

// TestTasksAdd_ExplicitRepoInsideAfHomeIsAllowed pins the escape hatch: a caller
// who names the path has STATED the binding rather than inherited it, so
// legitimate uses stay open.
func TestTasksAdd_ExplicitRepoInsideAfHomeIsAllowed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	resetAddFlags(t)
	resetScopeFlags(t)
	stubDaemon(t)

	clone := filepath.Join(home, "runtime", "deliberate")
	require.NoError(t, os.MkdirAll(filepath.Dir(clone), 0o755))
	require.NoError(t, exec.Command("git", "init", clone).Run())
	require.NoError(t, exec.Command("git", "-C", clone, "config", "user.email", "t@e.com").Run())
	require.NoError(t, exec.Command("git", "-C", clone, "config", "user.name", "T").Run())
	require.NoError(t, exec.Command("git", "-C", clone, "commit", "--allow-empty", "-m", "init").Run())

	repoFlag = clone
	taskAddNameFlag = "deliberate"
	taskAddPromptFlag = "on purpose"
	taskAddCronFlag = "*/5 * * * *"

	require.NoError(t, tasksAddCmd.RunE(tasksAddCmd, nil), "an explicit --repo must remain an escape hatch")
}

// TestTasksAdd_SessionWorktreeInsideAfHomeIsAllowed is the guard's most
// important negative: af's own session worktrees live under af's home, and an
// agent running `af tasks add` from one must NOT be refused. A linked worktree
// resolves to its main repository root, which is outside af's home — the guard
// keys off that resolved root precisely so normal sessions never trip it.
func TestTasksAdd_SessionWorktreeInsideAfHomeIsAllowed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	resetAddFlags(t)
	resetScopeFlags(t)
	stubDaemon(t)

	project := mkRepo(t, "real-project")
	wt := filepath.Join(home, "worktrees", "feature-x")
	require.NoError(t, os.MkdirAll(filepath.Dir(wt), 0o755))
	require.NoError(t, exec.Command("git", "-C", project, "worktree", "add", "-b", "feature-x", wt).Run(), "git worktree add")
	t.Chdir(wt)

	taskAddNameFlag = "from-a-session"
	taskAddPromptFlag = "do it"
	taskAddCronFlag = "0 9 * * *"

	out := captureJSON(t, func() error { return tasksAddCmd.RunE(tasksAddCmd, nil) })
	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, project, got["project_path"],
		"a task added from a session worktree binds to the real project, not the worktree")
}

// TestGuardProjectBinding_IgnoresProjectsOutsideAfHome is the ordinary case: a
// normal checkout anywhere else is never questioned.
func TestGuardProjectBinding_IgnoresProjectsOutsideAfHome(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := mkRepo(t, "ordinary")
	require.NoError(t, guardProjectBinding(&config.RepoContext{Root: repo, ID: config.RepoIDFromRoot(repo)}, false))
}

// resetScopeFlags clears the scope-related package globals between tests.
func resetScopeFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		repoFlag = ""
		tasksListAllFlag = false
	}
	t.Cleanup(reset)
	reset()
}

// captureTasksList runs `tasks list` and decodes its bare-array payload.
func captureTasksList(t *testing.T) []task.Task {
	t.Helper()
	out := captureJSON(t, func() error { return tasksListCmd.RunE(tasksListCmd, nil) })
	var got []task.Task
	require.NoError(t, json.Unmarshal(out, &got))
	return got
}

// captureJSON runs fn with stdout redirected to a pipe and returns what it
// printed, so tests can assert on the actual machine-readable payload rather
// than on internal state.
func captureJSON(t *testing.T, fn func() error) []byte {
	t.Helper()
	prev := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	done := make(chan []byte, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- []byte(sb.String())
	}()

	runErr := fn()
	_ = w.Close()
	os.Stdout = prev
	out := <-done
	_ = r.Close()
	require.NoError(t, runErr)
	return out
}

// TestWhoami_RepoFlagIsCheckedNotDropped: whoami inherits the persistent --repo
// but used to parse and drop it — the silent mis-resolution class #1814 fixed
// for get/preview. Identity cannot be scoped (the caller's tmux name already
// names exactly one session), so a --repo naming a different project is an
// assertion that must FAIL rather than be ignored.
func TestWhoami_RepoFlagIsCheckedNotDropped(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)

	other := mkRepo(t, "other")
	stubCurrentTmuxName(t, func() (string, error) { return "af_me_agent", nil })
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title: "me", TmuxName: "af_me_agent", Path: "/home/agent/src/myrepo",
		}}, nil
	})

	repoFlag = other
	err := sessionsWhoamiCmd.RunE(sessionsWhoamiCmd, nil)
	require.Error(t, err, "--repo naming another project must not be silently ignored")
	assert.Contains(t, err.Error(), "belongs to project")
}

// TestWhoami_MatchingRepoFlagPasses: the assertion must not break the ordinary
// case where the caller passes the project they are actually in.
func TestWhoami_MatchingRepoFlagPasses(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)

	mine := mkRepo(t, "mine")
	stubCurrentTmuxName(t, func() (string, error) { return "af_me_agent", nil })
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title: "me", TmuxName: "af_me_agent", Path: mine,
		}}, nil
	})

	repoFlag = mine
	require.NoError(t, sessionsWhoamiCmd.RunE(sessionsWhoamiCmd, nil))
}

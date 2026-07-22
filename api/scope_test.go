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

func TestTasksRestart_RefusesOtherProjectsTask(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	calls := stubDaemon(t)
	seedTasksInTwoProjects(t) // cwd = alpha

	err := tasksRestartCmd.RunE(tasksRestartCmd, []string{"bbbb2222"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to project")
	assert.Empty(t, calls.restarted, "the restart RPC must not fire")
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

// TestSessionsCreate_RefusesCloneInsideAfHome pins the other binding command
// named by the shared #1891 contract. A session created from the stray clone is
// just as invisible from the intended project as a task, so it must stop before
// the daemon sees a create request.
func TestSessionsCreate_RefusesCloneInsideAfHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	silenceStdio(t)

	clone := filepath.Join(home, "runtime", "detail-dlq-monitor")
	require.NoError(t, os.MkdirAll(filepath.Dir(clone), 0o755))
	require.NoError(t, exec.Command("git", "init", clone).Run())
	require.NoError(t, exec.Command("git", "-C", clone, "config", "user.email", "t@e.com").Run())
	require.NoError(t, exec.Command("git", "-C", clone, "config", "user.name", "T").Run())
	require.NoError(t, exec.Command("git", "-C", clone, "commit", "--allow-empty", "-m", "init").Run())
	t.Chdir(clone)

	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		t.Fatalf("daemon received refused AF-home binding: %+v", req)
		return nil, nil
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })
	setSessionsCreateFlags(t, "stray-session", "", false, false)

	err := sessionsCreateCmd.RunE(sessionsCreateCmd, nil)
	require.Error(t, err, "a cwd-derived session binding to a clone inside af's home must be refused")
	assert.Contains(t, err.Error(), "--repo")
	assert.Contains(t, strings.ToLower(err.Error()), "af's home")
}

func TestSessionsSendPromptCreate_RefusesCloneInsideAfHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	resetSendPromptState(t)
	silenceStdio(t)

	clone := filepath.Join(home, "runtime", "detail-dlq-monitor")
	require.NoError(t, os.MkdirAll(filepath.Dir(clone), 0o755))
	require.NoError(t, exec.Command("git", "init", clone).Run())
	t.Chdir(clone)

	prevDeliver := deliverPromptViaDaemon
	deliverPromptViaDaemon = func(req daemon.DeliverPromptRequest) (string, error) {
		t.Fatalf("daemon received refused send-prompt --create binding: %+v", req)
		return "", nil
	}
	t.Cleanup(func() { deliverPromptViaDaemon = prevDeliver })
	sendPromptCreateFlag = true

	err := sessionsSendPromptCmd.RunE(sessionsSendPromptCmd, []string{"stray-session", "hello"})
	require.Error(t, err, "send-prompt --create must share the AF-home binding refusal")
	assert.Contains(t, err.Error(), "--repo")
	assert.Contains(t, strings.ToLower(err.Error()), "af's home")
}

func TestSessionsCreate_ExplicitRepoInsideAfHomeIsAllowed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	silenceStdio(t)

	clone := filepath.Join(home, "runtime", "deliberate")
	require.NoError(t, os.MkdirAll(filepath.Dir(clone), 0o755))
	require.NoError(t, exec.Command("git", "init", clone).Run())

	called := false
	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		called = true
		return &session.InstanceData{Title: req.Title}, nil
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })
	setSessionsCreateFlags(t, "deliberate-session", clone, false, false)

	require.NoError(t, sessionsCreateCmd.RunE(sessionsCreateCmd, nil), "explicit --repo must remain the escape hatch")
	assert.True(t, called, "explicit binding did not reach the daemon")
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
		sessionsListAllFlag = false
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

// TestWhoami_NonCanonicalSessionPathStillMatches covers the Codex P2 on this
// PR: a session's stored Path is kept as ENTERED and may never have been
// git-resolved, while --repo is resolved through git. Hashing Path raw made a
// blanket `whoami --repo <canonical root>` reject the caller's OWN session.
// The comparison resolves the session's root the same way, so the two agree.
func TestWhoami_NonCanonicalSessionPathStillMatches(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)

	repo := mkRepo(t, "proj")
	// A non-canonical spelling of the very same project: trailing slash and a
	// "." segment. RepoIDFromRoot would hash this differently from repo.ID.
	noisy := filepath.Join(repo, ".") + string(filepath.Separator)
	require.NotEqual(t, config.RepoIDFromRoot(noisy), config.RepoIDFromRoot(repo),
		"fixture must actually be a different raw hash, else the test proves nothing")

	stubCurrentTmuxName(t, func() (string, error) { return "af_me_agent", nil })
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "me", TmuxName: "af_me_agent", Path: noisy}}, nil
	})

	repoFlag = repo
	require.NoError(t, sessionsWhoamiCmd.RunE(sessionsWhoamiCmd, nil),
		"a non-canonically-stored path must still match its own project")
}

// TestWhoami_PrefersWorktreeRepoPath pins the #667 derivation sessionRepoRoot
// shares with `archive --self`: the worktree's RepoPath is the canonical root,
// and Path is only the fallback.
func TestWhoami_PrefersWorktreeRepoPath(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)

	real := mkRepo(t, "real")
	stubCurrentTmuxName(t, func() (string, error) { return "af_me_agent", nil })
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title: "me", TmuxName: "af_me_agent",
			Path:     "/some/stale/entered/path",
			Worktree: session.GitWorktreeData{RepoPath: real},
		}}, nil
	})

	repoFlag = real
	require.NoError(t, sessionsWhoamiCmd.RunE(sessionsWhoamiCmd, nil),
		"the worktree RepoPath is the canonical root and must win over Path")
}

// TestTasksGet_InvalidRepoReportedBeforeNotFound covers the Codex P3: an
// invalid --repo must name the path it could not resolve rather than being
// masked by a not-found for the id.
func TestTasksGet_InvalidRepoReportedBeforeNotFound(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)

	repoFlag = filepath.Join(t.TempDir(), "not-a-git-repo")
	err := tasksGetCmd.RunE(tasksGetCmd, []string{"missing1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid git repository",
		"a bad --repo must be reported, not masked by the missing id")
}

// TestTasksRemove_RefusesTaskReboundBetweenCheckAndMutate reproduces the
// check-then-act race end to end (#1893 review). The CLI's scope check
// authorizes the id against alpha; the stub then rebinds the task to beta —
// standing in for a concurrent client, using the supported ProjectPath patch
// (#1836) — before the delete lands. The delete must be refused: without the
// expectation threaded through, a command run from alpha silently deletes
// beta's task, which is the exact harm this PR exists to prevent.
func TestTasksRemove_RefusesTaskReboundBetweenCheckAndMutate(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	_, beta := seedTasksInTwoProjects(t)

	orig := daemonRemoveTask
	daemonRemoveTask = func(id string, expect task.ProjectExpectation) error {
		// Between the CLI's check and the daemon's delete, another client
		// rebinds the task to a different project.
		_, err := task.UpdateTask(id, task.TaskUpdate{ProjectPath: &beta}, task.ProjectExpectation{})
		require.NoError(t, err)
		return orig(id, expect)
	}
	t.Cleanup(func() { daemonRemoveTask = orig })

	err := tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"aaaa1111"})
	require.Error(t, err, "a task rebound between the scope check and the delete must not be deleted")

	got, err := task.GetTask("aaaa1111")
	require.NoError(t, err, "the refused delete must leave the task on disk")
	assert.Equal(t, beta, got.ProjectPath)
}

// TestTasksRemove_CarriesExpectationToDaemon guards the wiring itself. The race
// test above only fails if the expectation is BOTH produced and threaded
// through; this pins that the CLI actually sends an enforced one, so dropping it
// at the call site can never pass silently on the happy path.
func TestTasksRemove_CarriesExpectationToDaemon(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	calls := stubDaemon(t)
	alpha, _ := seedTasksInTwoProjects(t)

	require.NoError(t, tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"aaaa1111"}))
	assert.True(t, calls.lastExpect.Enforce, "the CLI must ask the daemon to re-verify the binding under its lock")
	assert.Equal(t, alpha, calls.lastExpect.ProjectPath)
}

// TestTasksUpdate_CarriesExpectationToDaemon is the same wiring guard for the
// patch path.
func TestTasksUpdate_CarriesExpectationToDaemon(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	calls := stubDaemon(t)
	alpha, _ := seedTasksInTwoProjects(t)

	taskUpdateEnabledFlag = "false"
	t.Cleanup(func() { taskUpdateEnabledFlag = "" })
	require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{"aaaa1111"}))
	assert.True(t, calls.lastExpect.Enforce)
	assert.Equal(t, alpha, calls.lastExpect.ProjectPath)
}

// TestTasksTrigger_CarriesExpectationToDaemon: trigger fires a session, so a
// stale authorization here starts work in the wrong project rather than merely
// editing a row.
func TestTasksTrigger_CarriesExpectationToDaemon(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	calls := stubDaemon(t)
	alpha, _ := seedTasksInTwoProjects(t)

	require.NoError(t, tasksRunCmd.RunE(tasksRunCmd, []string{"aaaa1111"}))
	assert.True(t, calls.lastExpect.Enforce)
	assert.Equal(t, alpha, calls.lastExpect.ProjectPath)
}

func TestTasksRestart_CarriesExpectationToDaemon(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	calls := stubDaemon(t)
	alpha, _ := seedTasksInTwoProjects(t)

	require.NoError(t, tasksRestartCmd.RunE(tasksRestartCmd, []string{"aaaa1111"}))
	assert.True(t, calls.lastExpect.Enforce)
	assert.Equal(t, alpha, calls.lastExpect.ProjectPath)
}

// mkTaskInDeletedSubdir binds a task to a subdirectory of alpha and then deletes
// that subdirectory, leaving the project itself intact — the shape a TUI-created
// task takes when its recorded path is a subdir or linked worktree that later
// goes away.
func mkTaskInDeletedSubdir(t *testing.T) (alpha, sub string) {
	t.Helper()
	alpha = mkRepo(t, "alpha")
	sub = filepath.Join(alpha, "services", "dlq")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	seedTask(t, task.Task{
		ID: "aaaa1111", Name: "alpha-task", Prompt: "p", CronExpr: "0 9 * * *",
		ProjectPath: sub, Enabled: true,
	})
	require.NoError(t, os.RemoveAll(filepath.Join(alpha, "services")))
	return alpha, sub
}

// TestTasksList_DeletedSubdirStaysInOwningProject: hashing the stale leaf path
// produced an ID no project could ever equal, so the task vanished from its own
// project's list. Resolving through a surviving ancestor keeps it visible.
func TestTasksList_DeletedSubdirStaysInOwningProject(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	alpha, _ := mkTaskInDeletedSubdir(t)
	t.Chdir(alpha)

	out := captureTasksList(t)
	require.Len(t, out, 1, "a task whose recorded subdirectory was deleted must stay visible in its owning project")
	assert.Equal(t, "aaaa1111", out[0].ID)
}

// TestTasksRemove_DeletedSubdirStillReachable: the other half of the strand —
// every id-taking command rejected the task, so it could not even be cleaned up.
func TestTasksRemove_DeletedSubdirStillReachable(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	alpha, _ := mkTaskInDeletedSubdir(t)
	t.Chdir(alpha)

	require.NoError(t, tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"aaaa1111"}),
		"the owning project must be able to remove a task whose recorded subdir was deleted")
}

// TestRequireTaskInScope_SuggestsResolvableRepo: the refusal names a --repo to
// pass. Naming the deleted subdirectory made that suggestion unusable — it reads
// as the fix and cannot work — so it must name the surviving project root.
func TestRequireTaskInScope_SuggestsResolvableRepo(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	alpha, sub := mkTaskInDeletedSubdir(t)
	beta := mkRepo(t, "beta")
	t.Chdir(beta)

	err := tasksRemoveCmd.RunE(tasksRemoveCmd, []string{"aaaa1111"})
	require.Error(t, err, "beta must not reach alpha's task")
	// NotContains on the deleted path is the load-bearing half: the subdir path
	// has alpha as a PREFIX, so a Contains check on alpha alone passes even when
	// the message suggests the unusable deleted path.
	assert.NotContains(t, err.Error(), "--repo "+sub,
		"suggesting the deleted subdirectory reads as a fix but cannot resolve")
	assert.Contains(t, err.Error(), "--repo "+alpha,
		"the suggested --repo must name the surviving project root")
}

// seedTaskRaw writes tasks.json directly, bypassing task.AddTask. AddTask
// DERIVES RepoID from ProjectPath by design — a client must not be able to
// inject an id that disagrees with the path it binds — so it is the wrong tool
// for constructing a record whose retained id and recorded path diverge, which
// is precisely the state these tests need to exercise.
func seedTaskRaw(t *testing.T, tasks ...task.Task) {
	t.Helper()
	dir, err := config.GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	data, err := json.Marshal(tasks)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tasks.json"), data, 0o644))
}

// TestTasksList_RetainedRepoIDBeatsPathRederivation pins the retained id as
// authoritative over re-derivation. When the recorded path resolves to a
// DIFFERENT project than the id captured at bind time — a deleted inner repo
// whose path an outer repo now covers, or a path since reused — re-deriving
// produces a confidently wrong answer and silently moves the task between
// projects. The retained id is what makes that impossible.
func TestTasksList_RetainedRepoIDBeatsPathRederivation(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	alpha := mkRepo(t, "alpha")
	beta := mkRepo(t, "beta")
	alphaRepo, err := config.RepoFromPath(alpha)
	require.NoError(t, err)

	// The recorded path resolves to beta; the id retained at bind time says alpha.
	seedTaskRaw(t, task.Task{
		ID: "aaaa1111", Name: "alpha-task", Prompt: "p", CronExpr: "0 9 * * *",
		ProjectPath: beta, RepoID: alphaRepo.ID, Enabled: true,
	})

	t.Chdir(alpha)
	out := captureTasksList(t)
	require.Len(t, out, 1, "the retained id must keep the task in the project it was bound to")

	t.Chdir(beta)
	out = captureTasksList(t)
	require.Empty(t, out, "re-deriving from the recorded path must not move the task into another project")
}

// TestTasksAdd_RetainsRepoID: the retained id only helps if it is actually
// written at bind time, while the path still resolves.
func TestTasksAdd_RetainsRepoID(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	alpha := mkRepo(t, "alpha")
	t.Chdir(alpha)

	taskAddNameFlag = "nightly"
	taskAddPromptFlag = "sweep"
	taskAddCronFlag = "0 3 * * *"
	require.NoError(t, tasksAddCmd.RunE(tasksAddCmd, nil))

	tasks, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	repo, err := config.RepoFromPath(alpha)
	require.NoError(t, err)
	assert.Equal(t, repo.ID, tasks[0].RepoID, "tasks add must retain the project's id at bind time")
}

// TestWhoami_InvalidRepoErrorsOnRootlessRow: a session row with neither
// Worktree.RepoPath nor Path skipped the --repo block entirely, so an explicitly
// malformed flag was accepted and session data printed. Validating what the flag
// NAMES is independent of whether there is anything to compare it against.
func TestWhoami_InvalidRepoErrorsOnRootlessRow(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)

	stubCurrentTmuxName(t, func() (string, error) { return "af_me_agent", nil })
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		// A remote-backed row that records no repo root at all.
		return []session.InstanceData{{Title: "me", TmuxName: "af_me_agent"}}, nil
	})

	repoFlag = filepath.Join(t.TempDir(), "not-a-repo")
	err := sessionsWhoamiCmd.RunE(sessionsWhoamiCmd, nil)
	require.Error(t, err, "an explicitly malformed --repo must never be silently ignored")
}

// TestWhoami_RootlessRowStillPassesWithValidRepo keeps the fix from over-
// correcting: a VALID --repo against a row with no root to compare must still
// succeed, since asserting on a value we do not have would fail a caller who is
// exactly where they claim.
func TestWhoami_RootlessRowStillPassesWithValidRepo(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)

	valid := mkRepo(t, "valid")
	stubCurrentTmuxName(t, func() (string, error) { return "af_me_agent", nil })
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "me", TmuxName: "af_me_agent"}}, nil
	})

	repoFlag = valid
	assert.NoError(t, sessionsWhoamiCmd.RunE(sessionsWhoamiCmd, nil))
}

// TestTasksList_RelativeProjectPathIsNotAdoptedByCwd guards the ancestor walk
// against inventing an identity. Climbing a RELATIVE recorded path reaches ".",
// which resolves to whatever repository the caller is standing in — so a task
// with a hand-edited relative path would silently appear in (and be mutable
// from) every project it was listed in. A relative path is not evidence of
// belonging anywhere, so it degrades to path equality instead.
func TestTasksList_RelativeProjectPathIsNotAdoptedByCwd(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	stubDaemon(t)
	alpha := mkRepo(t, "alpha")
	seedTaskRaw(t, task.Task{
		ID: "aaaa1111", Name: "x", Prompt: "p", CronExpr: "0 9 * * *",
		ProjectPath: "nope/gone", Enabled: true,
	})
	t.Chdir(alpha)

	out := captureTasksList(t)
	assert.Empty(t, out, "a relative recorded path must not adopt the current directory's project")
}

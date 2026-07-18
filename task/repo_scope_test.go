package task

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These pin #2098: the TUI task pane loads through LoadTasksForRepo, which used
// raw path equality (`t.ProjectPath == repoRoot`). A task created from a
// SUBDIRECTORY of the repo records that subdirectory as its ProjectPath, so it
// never string-matched the repo root and was invisible in its own project's
// pane — while the same task listed fine under `af tasks list`, which had
// already moved to repo-identity matching (#1893, api/scope.go).

// mkScopeRepo creates a throwaway git repository and returns its root, resolved
// through symlinks so it compares equal to what git reports from inside it (on
// macOS t.TempDir() hands back a /var → /private/var symlink).
func mkScopeRepo(t *testing.T, name string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), name)
	require.NoError(t, exec.Command("git", "init", repo).Run(), "git init %s", name)
	real, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	return real
}

// TestLoadTasksForRepo_SubdirectoryProjectPath is the headline #2098 repro: a
// task bound to `<repo>/some/subdir` belongs to `<repo>`, because git resolves
// any subdirectory to the same top-level. The legacy row (no RepoID) is the
// one that regressed — it forces the git resolution fallback.
func TestLoadTasksForRepo_SubdirectoryProjectPath(t *testing.T) {
	repo := mkScopeRepo(t, "project")
	subdir := filepath.Join(repo, "some", "subdir")
	require.NoError(t, exec.Command("mkdir", "-p", subdir).Run())

	setupTestTasks(t, []Task{
		{ID: "root0001", Name: "from-root", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: repo, Program: "claude", Enabled: true, CreatedAt: time.Now()},
		{ID: "subd0001", Name: "from-subdir", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: subdir, Program: "claude", Enabled: true, CreatedAt: time.Now()},
	})

	got, err := LoadTasksForRepo(repo)
	require.NoError(t, err)

	var names []string
	for _, tk := range got {
		names = append(names, tk.Name)
	}
	assert.ElementsMatch(t, []string{"from-root", "from-subdir"}, names,
		"a task created from a subdirectory of the repo must appear in that repo's pane (#2098)")
}

// TestLoadTasksForCurrentRepo_CreatedFromSubdirectory is the user's story end to
// end: the TUI task form prefills its project path from os.Getwd()
// (ui/task_pane.go EnterCreateMode), so a user standing in `repo/some/subdir`
// creates a task bound to that subdirectory — and then the pane, which loads
// through LoadTasksForCurrentRepo, must show it. AddTask is the real writer, so
// this also pins that the bind-time RepoID it stamps resolves to the project.
func TestLoadTasksForCurrentRepo_CreatedFromSubdirectory(t *testing.T) {
	repo := mkScopeRepo(t, "project")
	subdir := filepath.Join(repo, "some", "subdir")
	require.NoError(t, exec.Command("mkdir", "-p", subdir).Run())
	setupTestTasks(t, []Task{})

	t.Chdir(subdir)
	require.NoError(t, AddTask(Task{
		ID: "subd0002", Name: "from-subdir", Prompt: "p", CronExpr: "0 * * * *",
		ProjectPath: subdir, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}))

	got, err := LoadTasksForCurrentRepo()
	require.NoError(t, err)
	require.Len(t, got, 1, "a task created from a subdirectory must show in its own project's pane (#2098)")
	assert.Equal(t, "from-subdir", got[0].Name)
}

// TestLoadTasksForRepo_OtherRepoStillExcluded is the over-listing guard: widening
// the match to repo identity must not pull in a DIFFERENT project's tasks. A
// sibling repo shares a parent directory with this one, so any fix that reduced
// to lexical prefix matching on the parent would fail here.
func TestLoadTasksForRepo_OtherRepoStillExcluded(t *testing.T) {
	parent := t.TempDir()
	mkSibling := func(name string) string {
		repo := filepath.Join(parent, name)
		require.NoError(t, exec.Command("git", "init", repo).Run(), "git init %s", name)
		real, err := filepath.EvalSymlinks(repo)
		require.NoError(t, err)
		return real
	}
	alpha, beta := mkSibling("alpha"), mkSibling("beta")
	betaSub := filepath.Join(beta, "nested")
	require.NoError(t, exec.Command("mkdir", "-p", betaSub).Run())

	setupTestTasks(t, []Task{
		{ID: "alph0001", Name: "alpha-task", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: alpha, Program: "claude", Enabled: true, CreatedAt: time.Now()},
		{ID: "beta0001", Name: "beta-task", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: beta, Program: "claude", Enabled: true, CreatedAt: time.Now()},
		{ID: "beta0002", Name: "beta-subdir-task", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: betaSub, Program: "claude", Enabled: true, CreatedAt: time.Now()},
	})

	got, err := LoadTasksForRepo(alpha)
	require.NoError(t, err)
	require.Len(t, got, 1, "only alpha's own task belongs to alpha")
	assert.Equal(t, "alpha-task", got[0].Name)
}

// TestLoadTasksForRepo_RetainedRepoIDWins pins that the bind-time RepoID is the
// authority when present, so a task whose recorded subdirectory has since been
// DELETED still lists in its project rather than vanishing. This is the whole
// reason Task.RepoID exists (#1893) and it costs no git resolution.
func TestLoadTasksForRepo_RetainedRepoIDWins(t *testing.T) {
	repo := mkScopeRepo(t, "project")
	gone := filepath.Join(repo, "deleted", "subdir") // never created on disk

	setupTestTasks(t, []Task{
		{ID: "gone0001", Name: "orphan-subdir", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: gone, RepoID: repoIDForPath(repo), Program: "claude", Enabled: true, CreatedAt: time.Now()},
	})

	got, err := LoadTasksForRepo(repo)
	require.NoError(t, err)
	require.Len(t, got, 1, "a retained RepoID keeps the task in its project even after the recorded subdirectory is gone")
	assert.Equal(t, "orphan-subdir", got[0].Name)
}

// TestLoadTasksForRepo_LinkedWorktree covers the other shape of the same bug: a
// task bound to a linked worktree of the repo. git resolves a linked worktree
// back to the main repo root (config.ResolveMainRepoRoot), so it is the same
// project — this is exactly how an af session's own worktree resolves.
func TestLoadTasksForRepo_LinkedWorktree(t *testing.T) {
	repo := mkScopeRepo(t, "project")
	for _, args := range [][]string{
		{"-C", repo, "config", "user.email", "test@example.com"},
		{"-C", repo, "config", "user.name", "Test User"},
		{"-C", repo, "commit", "--allow-empty", "-m", "init"},
	} {
		require.NoError(t, exec.Command("git", args...).Run(), "git %v", args)
	}
	wt := filepath.Join(t.TempDir(), "linked")
	out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", "feature", wt).CombinedOutput()
	require.NoError(t, err, "git worktree add: %s", out)
	realWT, err := filepath.EvalSymlinks(wt)
	require.NoError(t, err)

	setupTestTasks(t, []Task{
		{ID: "wtre0001", Name: "worktree-task", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: realWT, Program: "claude", Enabled: true, CreatedAt: time.Now()},
	})

	got, err := LoadTasksForRepo(repo)
	require.NoError(t, err)
	require.Len(t, got, 1, "a linked worktree resolves to the main repo root, so its task belongs to that project")
	assert.Equal(t, "worktree-task", got[0].Name)
}

// TestLoadTasksForRepo_NonRepoPathsFallBackToPathEquality pins the degenerate
// case the old behavior handled and the new one must keep: paths that do not
// resolve to any repository (a stale clone, a hand-edited row) still scope by
// their recorded path, so they stay visible where they were recorded rather
// than collapsing into one bucket.
func TestLoadTasksForRepo_NonRepoPathsFallBackToPathEquality(t *testing.T) {
	setupTestTasks(t, []Task{
		{ID: "one00001", Name: "A", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: "/repos/one", Program: "claude", Enabled: true, CreatedAt: time.Now()},
		{ID: "two00001", Name: "B", Prompt: "p", CronExpr: "0 * * * *", ProjectPath: "/repos/two", Program: "claude", Enabled: true, CreatedAt: time.Now()},
	})

	one, err := LoadTasksForRepo("/repos/one")
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, "A", one[0].Name)

	none, err := LoadTasksForRepo("/repos/absent")
	require.NoError(t, err)
	assert.Empty(t, none)
}

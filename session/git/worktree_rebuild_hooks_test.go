package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

// gatedHookRepo builds a real repo whose single post-worktree hook is a gate: it
// records that it started, blocks until the test releases it, and only then
// records that it FINISHED. The completion marker is what a duplicate run is
// counted by — it is the operator's side effect actually happening, not merely a
// process existing.
//
// The markers are written to ABSOLUTE paths outside the worktree on purpose. A
// hook that outlives its worktree keeps running with its cwd on a deleted
// directory, so relative I/O would fail with ENOENT and hide the very thing this
// measures; absolute-path work is exactly the half that still lands (#2770).
func gatedHookRepo(t *testing.T) (repoRoot, startedDir, releaseFile, doneDir string) {
	t.Helper()
	sandboxHome(t)
	repoRoot = createGitRepo(t)

	markers := t.TempDir()
	startedDir = filepath.Join(markers, "started")
	doneDir = filepath.Join(markers, "done")
	releaseFile = filepath.Join(markers, "release")
	require.NoError(t, os.MkdirAll(startedDir, 0o755))
	require.NoError(t, os.MkdirAll(doneDir, 0o755))

	// $$ is the hook shell's pid, so concurrent runs cannot overwrite each
	// other's markers — two runs leave two files, which is the whole point.
	hook := "touch " + startedDir + "/$$; " +
		"while [ ! -f " + releaseFile + " ]; do sleep 0.05; done; " +
		"touch " + doneDir + "/$$"

	repoID := config.RepoIDFromRoot(repoRoot)
	require.NoError(t, config.SaveRepoConfig(repoID, &config.RepoConfig{
		PostWorktreeCommands: []string{hook},
	}))

	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))
	return repoRoot, startedDir, releaseFile, doneDir
}

func commitInitial(t *testing.T, repoRoot string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repoRoot, "commit", "--allow-empty", "-m", "initial")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func countMarkers(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	return len(entries)
}

// waitForMarkers polls until dir holds at least n entries, and reports whether
// it got there. Used in both directions: to wait for a hook to really be in
// flight, and to give a duplicate every chance to appear before ruling it out.
func waitForMarkers(t *testing.T, dir string, n int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if countMarkers(t, dir) >= n {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func closed(ch <-chan struct{}, timeout time.Duration) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

// A rebuild must not leave the previous hook run executing (#2770).
//
// Recovery reuses the live GitWorktree: respawn calls RebuildFromExistingBranch
// on i.gitWorktree with no Cleanup() in between. So a hook still running from
// before the session was Lost was never cancelled, and the rebuild started a
// second one over the same path — the operator's post_worktree_commands running
// TWICE. These are provisioning commands, and the duplicate is invisible:
// hooksDone is overwritten by the new run, so nothing is tracking the old one.
//
// The gate holds both runs open at once so the overlap is a fact rather than a
// race: with the bug BOTH complete when released.
func TestRebuildFromExistingBranch_CancelsTheHookRunAlreadyInFlight(t *testing.T) {
	repoRoot, startedDir, releaseFile, doneDir := gatedHookRepo(t)
	commitInitial(t, repoRoot)

	gw, _, err := NewGitWorktree(repoRoot, "rebuild-hooks")
	require.NoError(t, err)
	require.NoError(t, gw.Setup())

	first := gw.HooksDone()
	require.NotNil(t, first, "Setup launched no hook run, so there is nothing to duplicate")
	require.True(t, waitForMarkers(t, startedDir, 1, 10*time.Second),
		"the first hook never started; the test would prove nothing")

	// The session's worktree vanishes underneath it — the Lost-recovery trigger.
	require.NoError(t, os.RemoveAll(gw.GetWorktreePath()))

	require.NoError(t, gw.RebuildFromExistingBranch())
	second := gw.HooksDone()
	require.NotNil(t, second)
	require.False(t, first == second, "the rebuild reused the previous run's completion channel")

	// The rebuild must have retired the in-flight run. Its channel closes on
	// cancellation, so this is the direct question: is the old run still going?
	require.True(t, closed(first, 10*time.Second),
		"the hook run from before the rebuild is STILL RUNNING: the rebuild started a second one "+
			"without cancelling it, so the operator's post_worktree_commands execute twice over this path")

	// Release the gate and let the surviving run finish.
	require.NoError(t, os.WriteFile(releaseFile, []byte("go"), 0o644))
	require.True(t, closed(second, 20*time.Second), "the rebuilt worktree's hook run never finished")

	// Give a duplicate every chance to show up before ruling it out: the killed
	// run would race to its own completion marker the moment the gate opened.
	if waitForMarkers(t, doneDir, 2, 2*time.Second) {
		t.Fatalf("post-worktree hooks COMPLETED %d times for one worktree, want 1: the run that was "+
			"in flight when the worktree vanished was never cancelled, so its side effects landed a "+
			"second time in the tree the rebuild had just recreated", countMarkers(t, doneDir))
	}
	require.Equal(t, 1, countMarkers(t, doneDir), "the rebuilt worktree was left unprovisioned")

	// The fresh context must be live, not the one just cancelled — otherwise the
	// rebuild "fixes" the duplicate by never provisioning at all.
	require.Equal(t, 2, countMarkers(t, startedDir),
		"the rebuild's own hook run never started; it inherited the cancellation it had just issued")
}

// The same obligation on the fresh-rebuild arm, which recovery falls back to
// when the recorded branch is gone too. It is a separate function with its own
// copy of the launch, so a fix applied to only one of them leaves this open.
func TestRebuildFreshFromRecordedBase_CancelsTheHookRunAlreadyInFlight(t *testing.T) {
	repoRoot, startedDir, releaseFile, doneDir := gatedHookRepo(t)
	commitInitial(t, repoRoot)

	gw, branchName, err := NewGitWorktree(repoRoot, "fresh-rebuild-hooks")
	require.NoError(t, err)
	require.NoError(t, gw.Setup())

	first := gw.HooksDone()
	require.NotNil(t, first)
	require.True(t, waitForMarkers(t, startedDir, 1, 10*time.Second), "the first hook never started")

	// Both the directory and the branch are gone: the fresh-rebuild precondition.
	require.NoError(t, os.RemoveAll(gw.GetWorktreePath()))
	out, err := exec.Command("git", "-C", repoRoot, "worktree", "prune").CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.Command("git", "-C", repoRoot, "branch", "-D", branchName).CombinedOutput()
	require.NoError(t, err, string(out))

	require.NoError(t, gw.RebuildFreshFromRecordedBase())

	require.True(t, closed(first, 10*time.Second),
		"the hook run from before the fresh rebuild is STILL RUNNING: the operator's "+
			"post_worktree_commands execute twice over this path")

	require.NoError(t, os.WriteFile(releaseFile, []byte("go"), 0o644))
	require.True(t, closed(gw.HooksDone(), 20*time.Second), "the rebuilt worktree's hook run never finished")

	if waitForMarkers(t, doneDir, 2, 2*time.Second) {
		t.Fatalf("post-worktree hooks COMPLETED %d times for one worktree, want 1", countMarkers(t, doneDir))
	}
	require.Equal(t, 2, countMarkers(t, startedDir),
		"the fresh rebuild's own hook run never started; it inherited the cancellation it had just issued")
}

// A rebuild after Cleanup must still provision. Cleanup cancels hooksCtx
// permanently, so a rebuild that reused it would start hooks that return at
// their first context check and silently leave the tree unprovisioned — the
// failure the fresh context in restartHooks exists to prevent. Left un-gated so
// the hook can complete on its own.
func TestRebuildAfterCleanup_StillRunsHooks(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	commitInitial(t, repoRoot)

	doneDir := filepath.Join(t.TempDir(), "done")
	require.NoError(t, os.MkdirAll(doneDir, 0o755))
	repoID := config.RepoIDFromRoot(repoRoot)
	require.NoError(t, config.SaveRepoConfig(repoID, &config.RepoConfig{
		PostWorktreeCommands: []string{"touch " + doneDir + "/$$"},
	}))
	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	gw, _, err := NewGitWorktree(repoRoot, "cleanup-then-rebuild")
	require.NoError(t, err)
	require.NoError(t, gw.Setup())
	require.True(t, closed(gw.HooksDone(), 20*time.Second), "the initial hook run never finished")

	state, err := gw.Cleanup()
	require.NoError(t, err)
	require.Equal(t, CleanupSettled, state)

	require.NoError(t, gw.RebuildFreshFromRecordedBase())
	require.True(t, closed(gw.HooksDone(), 20*time.Second), "the rebuilt worktree's hook run never finished")
	require.True(t, waitForMarkers(t, doneDir, 2, 10*time.Second),
		"the rebuild ran no hooks: it inherited the context Cleanup had already cancelled, so the "+
			"recovered worktree is silently unprovisioned")
}

// Sanity: a worktree whose hooks are still in flight reports them, so the
// markers above cannot be explained by hooks that never ran at all.
func TestSetupLaunchesHooksBeforeTheyFinish(t *testing.T) {
	repoRoot, startedDir, releaseFile, _ := gatedHookRepo(t)
	commitInitial(t, repoRoot)

	gw, _, err := NewGitWorktree(repoRoot, "in-flight")
	require.NoError(t, err)
	require.NoError(t, gw.Setup())
	t.Cleanup(func() { _ = os.WriteFile(releaseFile, []byte("go"), 0o644) })

	require.True(t, waitForMarkers(t, startedDir, 1, 10*time.Second))
	require.False(t, closed(gw.HooksDone(), 200*time.Millisecond),
		"a gated hook reported done while still blocked; the gate is not holding")
	require.False(t, strings.Contains(gw.GetWorktreePath(), " "), "unexpected path shape")
}

// The recreated tree must be untouched by the run that preceded it (#2770,
// post-review finding on the first fix).
//
// Cancelling is not enough on its own if it happens AFTER the rebuild: the
// survivor's relative I/O fails against the deleted directory, but its
// ABSOLUTE-path work keeps landing wherever it points, so a hook cancelled after
// `worktree add` has already written provisioning output into the tree the
// rebuild just created. The window is however long the git surgery takes.
//
// The hook here writes its pid into the worktree by absolute path, continuously,
// exactly as a provisioning command's output would land. The rebuilt tree must
// carry the NEW run's pid and only that.
func TestRebuildFromExistingBranch_RecreatedTreeIsUntouchedByThePriorRun(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	commitInitial(t, repoRoot)

	// $$ is the hook shell's pid; the worktree path is resolved by the hook at
	// run time via $PWD's absolute value captured before any deletion.
	repoID := config.RepoIDFromRoot(repoRoot)
	require.NoError(t, config.SaveRepoConfig(repoID, &config.RepoConfig{
		PostWorktreeCommands: []string{
			`d="$PWD"; while true; do echo "$$" >> "$d/hook-touched"; sleep 0.01; done`,
		},
	}))
	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "test/"
	require.NoError(t, config.SaveConfig(cfg))

	gw, _, err := NewGitWorktree(repoRoot, "tree-untouched")
	require.NoError(t, err)
	require.NoError(t, gw.Setup())
	t.Cleanup(func() { _, _ = gw.Cleanup() })

	worktreePath := gw.GetWorktreePath()
	touched := filepath.Join(worktreePath, "hook-touched")
	require.True(t, waitForFile(t, touched, 10*time.Second), "the first hook never wrote into the worktree")
	oldPID := strings.TrimSpace(strings.Split(strings.TrimSpace(readFile(t, touched)), "\n")[0])
	require.NotEmpty(t, oldPID)

	// The worktree vanishes — the Lost-recovery trigger. The old hook keeps
	// running, still pointed at this absolute path.
	require.NoError(t, os.RemoveAll(worktreePath))

	require.NoError(t, gw.RebuildFromExistingBranch())
	require.True(t, waitForFile(t, touched, 10*time.Second), "the rebuilt worktree's hook never ran")

	// Let the new run write for a while, then check WHOSE pids are in the tree.
	time.Sleep(500 * time.Millisecond)
	for _, line := range strings.Split(strings.TrimSpace(readFile(t, touched)), "\n") {
		require.NotEqual(t, oldPID, strings.TrimSpace(line),
			"the hook run from before the rebuild wrote into the RECREATED worktree: it was still "+
				"alive while the tree was being rebuilt, so the operator's provisioning output landed "+
				"in the new checkout on top of the run that owns it")
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

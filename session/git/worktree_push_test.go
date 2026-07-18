package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runGitOut runs a git command in dir and returns its output (runGit, shared with
// the network tests, is the fail-on-error, no-output form).
func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, string(out))
	return string(out)
}

// pushTestRepo sets up a repo with an initial commit, a bare `origin` it can push
// to, and a session worktree on branch `<branch>` — the shape a sandbox has at
// archive time. Returns the GitWorktree bound to the session worktree + branch,
// and the bare origin path so tests can assert what landed there.
func pushTestRepo(t *testing.T, branch string) (*GitWorktree, string) {
	t.Helper()
	repoRoot := createGitRepo(t)
	// SnapshotAndPushBranch commits via the production git runner (os.Environ, no
	// injected identity), so the repo itself must carry a committer identity — the
	// sandbox configures this globally; here we set it repo-local.
	runGit(t, repoRoot, "config", "user.email", "af@agent-factory.local")
	runGit(t, repoRoot, "config", "user.name", "Agent Factory")
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "init")

	bare := filepath.Join(t.TempDir(), "origin.git")
	require.NoError(t, exec.Command("git", "clone", "--bare", repoRoot, bare).Run())
	runGit(t, repoRoot, "remote", "add", "origin", bare)

	wtPath := filepath.Join(filepath.Dir(repoRoot), "session-wt")
	runGit(t, repoRoot, "worktree", "add", "-b", branch, wtPath)

	gw, err := NewGitWorktreeFromStorage(repoRoot, wtPath, "session", branch, "", false, true)
	require.NoError(t, err)
	return gw, bare
}

func branchTip(t *testing.T, dir, ref string) string {
	t.Helper()
	return strings.TrimSpace(runGitOut(t, dir, "rev-parse", ref))
}

// TestSnapshotAndPushBranch_PushesCommittedWork pins the archive primitive: a
// committed change on the session branch is pushed to origin, and the returned
// branch name matches (#1592 Phase 4 PR6).
func TestSnapshotAndPushBranch_PushesCommittedWork(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/feature")
	wt := gw.GetWorktreePath()

	require.NoError(t, os.WriteFile(filepath.Join(wt, "file.txt"), []byte("work"), 0644))
	runGit(t, wt, "add", "-A")
	runGit(t, wt, "commit", "-m", "real work")
	want := branchTip(t, wt, "HEAD")

	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, "root/feature", got)

	// origin now has the branch at the committed tip.
	assert.Equal(t, want, branchTip(t, bare, "refs/heads/root/feature"))
}

// TestSnapshotAndPushBranch_SnapshotsUncommitted pins the "nothing lost"
// guarantee: uncommitted work in the worktree is committed as a snapshot and
// pushed, so it survives the sandbox teardown (#1592 Phase 4 PR6).
func TestSnapshotAndPushBranch_SnapshotsUncommitted(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/wip")
	wt := gw.GetWorktreePath()

	tipBefore := branchTip(t, wt, "HEAD")
	// Leave an UNCOMMITTED file in the worktree.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("uncommitted"), 0644))

	_, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)

	// A snapshot commit was made (tip advanced) and pushed with the file present.
	tipAfter := branchTip(t, wt, "HEAD")
	assert.NotEqual(t, tipBefore, tipAfter, "a snapshot commit should have been made")
	pushed := branchTip(t, bare, "refs/heads/root/wip")
	assert.Equal(t, tipAfter, pushed)
	files := runGitOut(t, wt, "show", "--name-only", "--format=", "HEAD")
	assert.Contains(t, files, "dirty.txt", "the uncommitted file should be in the snapshot commit")
}

// TestSnapshotAndPushBranch_SnapshotsUntrackedWhenConfigHidesThem is the #2101
// regression, and it is data loss: the archive gate ran `git status --porcelain`,
// which honours status.showUntrackedFiles. With it set to `no` — set here on the
// main repo, since a worktree shares .git/config with it, exactly how the setting
// reaches a session worktree in the wild — the gate read the tree as clean and
// skipped the `add -A` + commit entirely. `add -A` would have staged the
// untracked file fine; it simply never ran, so the work was dropped when the
// sandbox was reaped. The gate must not depend on user config.
func TestSnapshotAndPushBranch_SnapshotsUntrackedWhenConfigHidesThem(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/hidden")
	wt := gw.GetWorktreePath()
	runGit(t, gw.GetRepoPath(), "config", "status.showUntrackedFiles", "no")

	// Precondition: the un-forced status the old gate used really does read clean.
	require.Empty(t, strings.TrimSpace(runGitOut(t, wt, "status", "--porcelain")),
		"precondition: status.showUntrackedFiles=no must hide the untracked file")

	tipBefore := branchTip(t, wt, "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "untracked.txt"), []byte("uncommitted work"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "notes"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "notes", "deep.txt"), []byte("nested work"), 0644))

	_, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)

	tipAfter := branchTip(t, wt, "HEAD")
	assert.NotEqual(t, tipBefore, tipAfter, "untracked work must produce a snapshot commit")
	assert.Equal(t, tipAfter, branchTip(t, bare, "refs/heads/root/hidden"))
	files := runGitOut(t, wt, "show", "--name-only", "--format=", "HEAD")
	assert.Contains(t, files, "untracked.txt", "untracked file must survive archive despite showUntrackedFiles=no")
	assert.Contains(t, files, "notes/deep.txt", "untracked file in an untracked dir must survive too")
}

// TestSnapshotAndPushBranch_CleanTreeJustPushes pins that a clean tree makes no
// snapshot commit — it only pushes the existing history.
func TestSnapshotAndPushBranch_CleanTreeJustPushes(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/clean")
	wt := gw.GetWorktreePath()
	runGit(t, wt, "commit", "--allow-empty", "-m", "a commit")
	tip := branchTip(t, wt, "HEAD")

	_, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)

	assert.Equal(t, tip, branchTip(t, wt, "HEAD"), "clean tree must not add a snapshot commit")
	assert.Equal(t, tip, branchTip(t, bare, "refs/heads/root/clean"))
}

// TestSnapshotAndPushBranch_PreservesWorkWhenHeadOnOtherBranch is the #1721
// regression: an agent switched branches inside the sandbox (git checkout -b …),
// so the worktree HEAD is no longer on g.branchName. The snapshot commit lands on
// the current HEAD; archive must still make that uncommitted work durable on the
// session branch (by pushing HEAD, not the stale stored branch), or it is lost
// when the sandbox is reaped.
func TestSnapshotAndPushBranch_PreservesWorkWhenHeadOnOtherBranch(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/session")
	wt := gw.GetWorktreePath()

	// Agent switched to a different branch, then left uncommitted work.
	runGit(t, wt, "checkout", "-b", "agent-scratch")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "work.txt"), []byte("precious"), 0644))

	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, "root/session", got, "the pushed/returned branch is the session branch, not the scratch branch")

	// Origin's session branch — the one restore re-clones — must carry the work.
	pushed := runGitOut(t, bare, "ls-tree", "-r", "--name-only", "refs/heads/root/session")
	assert.Contains(t, pushed, "work.txt",
		"uncommitted work committed on the detoured HEAD must survive on the session branch")
}

// TestSnapshotAndPushBranch_PreservesWorkWhenHeadDetached is the #1721 regression
// for a detached HEAD: the snapshot commit is created off any branch, and archive
// must still push it onto the session branch so it survives sandbox teardown.
func TestSnapshotAndPushBranch_PreservesWorkWhenHeadDetached(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/session")
	wt := gw.GetWorktreePath()

	// Detach HEAD, then leave uncommitted work.
	runGit(t, wt, "checkout", "--detach", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "work.txt"), []byte("precious"), 0644))

	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, "root/session", got)

	pushed := runGitOut(t, bare, "ls-tree", "-r", "--name-only", "refs/heads/root/session")
	assert.Contains(t, pushed, "work.txt",
		"uncommitted work committed on a detached HEAD must survive on the session branch")
}

// TestSnapshotAndPushBranch_RejectsExternalWorktree pins that an in-place/external
// worktree (the user's own tree) is never pushed by the archive primitive.
func TestSnapshotAndPushBranch_RejectsExternalWorktree(t *testing.T) {
	sandboxHome(t)
	gw, _ := pushTestRepo(t, "root/ext")
	gw.externalWorktree = true
	if _, err := gw.SnapshotAndPushBranch(); err == nil {
		t.Fatal("SnapshotAndPushBranch on an external worktree: want error")
	}
}

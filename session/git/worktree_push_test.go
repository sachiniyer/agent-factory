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

// TestSnapshotAndPushBranch_ProbesWorktreeFilesystem pins the two #5295-review
// invariants together: the push path probes the worktree itself — the
// destination filesystem git writes hit, including when the worktree is a
// mount point and its parent sits on another filesystem — AND no
// .af-add-probe-* debris may linger in the tracked tree, where a later
// archive retry's `git add -A` would stage it as user content. The probe's
// unlink-before-write is what makes inside-tree probing safe.
func TestSnapshotAndPushBranch_ProbesWorktreeFilesystem(t *testing.T) {
	sandboxHome(t)
	gw, _ := pushTestRepo(t, "root/probe")
	wt := gw.GetWorktreePath()

	// Force the push to fail so annotateGitWriteError runs: repoint origin at
	// a path that does not exist.
	runGit(t, gw.GetRepoPath(), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	var probed []string
	old := writeSpaceProbe
	writeSpaceProbe = func(dir string) error {
		probed = append(probed, dir)
		return nil
	}
	defer func() { writeSpaceProbe = old }()

	_, err := gw.SnapshotAndPushBranch()
	require.Error(t, err, "push to a dead origin must fail")

	assert.Contains(t, probed, wt,
		"the destination filesystem itself must be probed — a mount-point worktree's parent answers for a different filesystem")
	matches, _ := filepath.Glob(filepath.Join(wt, ".af-add-probe-*"))
	assert.Empty(t, matches, "no probe debris inside the tracked worktree")
}

// TestSnapshotAndPushBranch_UnprovenProbeNameIsUserContent pins the
// two-sided debris contract (#5295 review): an .af-add-probe-* file the
// sweep cannot prove it created — here, ANY file, since this process
// abandoned none — is treated as user content end to end: it is NOT
// deleted, and it IS included in the archive snapshot like any other
// file. Excluding probe-shaped names from `add` would silently drop a
// user file whose only sin is its name; a committed stray is a cosmetic
// wart, data loss is not.
func TestSnapshotAndPushBranch_UnprovenProbeNameIsUserContent(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/debris")
	wt := gw.GetWorktreePath()

	// An unproven probe-named file — the shape CreateTemp emits, content a
	// user could plausibly have. Also real user work.
	debris := filepath.Join(wt, ".af-add-probe-123456")
	require.NoError(t, os.WriteFile(debris, []byte("looks like probe debris, is not"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "real.txt"), []byte("work"), 0o600))

	_, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)

	// Snapshotted as user content: the commit carries both files.
	tree := runGitOut(t, bare, "ls-tree", "-r", "--name-only", "refs/heads/root/debris")
	assert.Contains(t, tree, "real.txt")
	assert.Contains(t, tree, ".af-add-probe-123456",
		"an unproven probe-named file is user content — the snapshot must keep it")

	// And never deleted by the sweep.
	data, rerr := os.ReadFile(debris)
	require.NoError(t, rerr, "unproven probe-named file must survive the sweep")
	assert.Equal(t, "looks like probe debris, is not", string(data))
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

// behindOriginRepo is the #5407 shape: origin's session branch is at B, but the
// sandbox's HEAD sits one commit behind it at A (an agent ran `git reset --hard
// HEAD~1` or checked out an older commit). HEAD is an ancestor of origin's tip.
func behindOriginRepo(t *testing.T, branch string) (gw *GitWorktree, bare, tipA, tipB string) {
	t.Helper()
	gw, bare = pushTestRepo(t, branch)
	wt := gw.GetWorktreePath()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a"), 0644))
	runGit(t, wt, "add", "-A")
	runGit(t, wt, "commit", "-m", "A")
	tipA = branchTip(t, wt, "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "b.txt"), []byte("b"), 0644))
	runGit(t, wt, "add", "-A")
	runGit(t, wt, "commit", "-m", "B")
	tipB = branchTip(t, wt, "HEAD")
	runGit(t, wt, "push", "origin", "HEAD:refs/heads/"+branch)
	runGit(t, wt, "reset", "--hard", tipA)
	return gw, bare, tipA, tipB
}

// TestSnapshotAndPushBranch_BehindOnlyCleanHeadIsAlreadyDurable: a clean HEAD that
// is an ancestor of origin's session branch has nothing to push — origin already
// holds every commit — so the archive must succeed instead of aborting on a
// non-fast-forward rejection (#5407).
func TestSnapshotAndPushBranch_BehindOnlyCleanHeadIsAlreadyDurable(t *testing.T) {
	sandboxHome(t)
	gw, bare, _, tipB := behindOriginRepo(t, "root/behind-clean")

	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err, "a behind-only HEAD is recoverable, not divergent")
	assert.Equal(t, "root/behind-clean", got)
	assert.Equal(t, tipB, branchTip(t, bare, "refs/heads/root/behind-clean"), "origin's tip must not move backwards")
}

// TestSnapshotAndPushBranch_BehindOnlyDirtyHeadFastForwardsThenSnapshots: with
// uncommitted work on a behind-only HEAD, the worktree is fast-forwarded onto
// origin's tip (carrying the dirty tree) before the snapshot commit, so the push
// is a plain fast-forward and the work is not stranded in the sandbox (#5407).
func TestSnapshotAndPushBranch_BehindOnlyDirtyHeadFastForwardsThenSnapshots(t *testing.T) {
	sandboxHome(t)
	gw, bare, _, tipB := behindOriginRepo(t, "root/behind-dirty")
	wt := gw.GetWorktreePath()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("uncommitted"), 0644))

	_, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)

	pushed := branchTip(t, bare, "refs/heads/root/behind-dirty")
	assert.Equal(t, branchTip(t, wt, "HEAD"), pushed)
	assert.NotEqual(t, tipB, pushed, "the snapshot commit advanced origin")
	runGit(t, bare, "merge-base", "--is-ancestor", tipB, pushed)
	assert.Contains(t, runGitOut(t, bare, "show", "--name-only", "--format=", pushed), "dirty.txt")
}

// TestSnapshotAndPushBranch_DivergedHeadStillFailsWithoutStranding: a genuinely
// divergent HEAD must still be refused, and the refusal must not leave a snapshot
// commit behind that a retry then skips (#5407).
func TestSnapshotAndPushBranch_DivergedHeadStillFailsWithoutStranding(t *testing.T) {
	sandboxHome(t)
	gw, bare, tipA, tipB := behindOriginRepo(t, "root/diverged")
	wt := gw.GetWorktreePath()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "c.txt"), []byte("c"), 0644))
	runGit(t, wt, "add", "-A")
	runGit(t, wt, "commit", "-m", "C")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("uncommitted"), 0644))
	tipC := branchTip(t, wt, "HEAD")
	_ = tipA

	_, err := gw.SnapshotAndPushBranch()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "diverged")
	assert.Equal(t, tipB, branchTip(t, bare, "refs/heads/root/diverged"))
	assert.Equal(t, tipC, branchTip(t, wt, "HEAD"), "no snapshot commit may be stranded by a refused push")
	assert.NotEmpty(t, strings.TrimSpace(runGitOut(t, wt, "status", "--porcelain")), "the dirty work stays uncommitted for the retry")
}

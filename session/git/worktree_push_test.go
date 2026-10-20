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

// advanceOriginAndResetHeadBehind is the shared setup for the behind-only HEAD
// regression: push the session branch to origin, advance it one commit, then
// move the worktree HEAD back to the ancestor — the state a restored sandbox
// lands in the moment its agent runs `git checkout <old-sha>` or
// `git reset --hard HEAD~1`. Returns origin's advanced tip and the worktree's
// pre-archive HEAD (the ancestor).
func advanceOriginAndResetHeadBehind(t *testing.T, gw *GitWorktree, bare, branch string) (originTip, ancestorHead string) {
	t.Helper()
	wt := gw.GetWorktreePath()
	runGit(t, wt, "commit", "--allow-empty", "-m", "origin advance")
	runGit(t, wt, "push", "origin", branch)
	originTip = branchTip(t, bare, "refs/heads/"+branch)
	runGit(t, wt, "reset", "--hard", "HEAD~1")
	ancestorHead = branchTip(t, wt, "HEAD")
	require.NotEqual(t, ancestorHead, originTip, "precondition: HEAD must be behind origin's tip")
	return originTip, ancestorHead
}

// TestSnapshotAndPushBranch_BehindCleanFastForwards is the core behind-only
// regression: a CLEAN worktree whose HEAD is an ancestor of origin's session
// branch. The old code pushed HEAD with a plain non-force push and was
// rejected as non-fast-forward (overwriting nothing on origin), aborting the
// archive to Lost. The fix fast-forwards HEAD onto origin's tip, so the push
// becomes a no-op fast-forward and the archive succeeds.
func TestSnapshotAndPushBranch_BehindCleanFastForwards(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/behind-clean")
	wt := gw.GetWorktreePath()
	originTip, _ := advanceOriginAndResetHeadBehind(t, gw, bare, "root/behind-clean")
	require.Empty(t, strings.TrimSpace(runGitOut(t, wt, "status", "--porcelain")),
		"precondition: tree must be clean after reset --hard")

	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, "root/behind-clean", got)

	// HEAD was fast-forwarded to origin's tip; origin is untouched (no-op push).
	assert.Equal(t, originTip, branchTip(t, wt, "HEAD"))
	assert.Equal(t, originTip, branchTip(t, bare, "refs/heads/root/behind-clean"))
	// No snapshot commit was made (clean tree, nothing to snapshot).
	assert.Empty(t, strings.TrimSpace(runGitOut(t, wt, "status", "--porcelain")))
}

// TestSnapshotAndPushBranch_BehindDetachedCleanFastForwards is the behind-only
// regression for a DETACHED HEAD — the restored-sandbox case where the agent
// ran `git checkout --detach <old-sha>` to inspect old code. The fast-forward
// must work off any branch (#1721), and the push must still target the session
// branch on origin.
func TestSnapshotAndPushBranch_BehindDetachedCleanFastForwards(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/behind-detached")
	wt := gw.GetWorktreePath()
	originTip, _ := advanceOriginAndResetHeadBehind(t, gw, bare, "root/behind-detached")
	runGit(t, wt, "checkout", "--detach", "HEAD")
	require.Empty(t, strings.TrimSpace(runGitOut(t, wt, "status", "--porcelain")))

	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, "root/behind-detached", got)

	assert.Equal(t, originTip, branchTip(t, wt, "HEAD"))
	assert.Equal(t, originTip, branchTip(t, bare, "refs/heads/root/behind-detached"))
}

// TestSnapshotAndPushBranch_BehindDirtySnapshotsOnOriginTip is the behind-only
// regression with UNCOMMITTED work — the case that stranded a WIP commit in the
// old code. The fix builds the snapshot commit directly on origin's tip (not on
// the behind HEAD) so the push fast-forwards, the agent's uncommitted work
// (modified + untracked) survives on the session branch, and no WIP commit is
// stranded on the old HEAD for the retry loop to fail on.
func TestSnapshotAndPushBranch_BehindDirtySnapshotsOnOriginTip(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/behind-dirty")
	wt := gw.GetWorktreePath()

	// Put a tracked file on origin's tip so we can both modify it and prove
	// the snapshot lands on origin's tip, not the behind HEAD.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "tracked.txt"), []byte("base\n"), 0o644))
	runGit(t, wt, "add", "-A")
	runGit(t, wt, "commit", "-m", "add tracked.txt")
	runGit(t, wt, "push", "origin", "root/behind-dirty")
	originTip := branchTip(t, bare, "refs/heads/root/behind-dirty")
	runGit(t, wt, "reset", "--hard", "HEAD~1") // HEAD back at init, behind origin
	ancestorHead := branchTip(t, wt, "HEAD")
	require.NotEqual(t, ancestorHead, originTip)

	// Leave uncommitted work: a modification to a tracked file AND a brand-new
	// untracked file (the kind `git stash create` would drop, which is why the
	// fix uses plumbing instead of stash).
	require.NoError(t, os.WriteFile(filepath.Join(wt, "tracked.txt"), []byte("agent edit\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "untracked.txt"), []byte("new work\n"), 0o644))
	require.NotEmpty(t, strings.TrimSpace(runGitOut(t, wt, "status", "--porcelain")),
		"precondition: tree must be dirty")

	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, "root/behind-dirty", got)

	// The snapshot commit's ONLY parent is origin's tip — NOT the behind HEAD.
	// This is the anti-stranding assertion: a WIP commit on the old HEAD
	// (what the old code created) would have made HEAD^ == ancestorHead.
	pushedTip := branchTip(t, wt, "HEAD")
	assert.Equal(t, pushedTip, branchTip(t, bare, "refs/heads/root/behind-dirty"))
	assert.Equal(t, originTip, strings.TrimSpace(runGitOut(t, wt, "rev-parse", "HEAD^")),
		"the snapshot commit's parent must be origin's tip, not the behind HEAD")
	assert.NotEqual(t, ancestorHead, strings.TrimSpace(runGitOut(t, wt, "rev-parse", "HEAD^")),
		"no WIP commit stranded on the behind HEAD")

	// The agent's uncommitted work survives on origin's session branch —
	// both the modified tracked file and the untracked file.
	files := runGitOut(t, bare, "ls-tree", "-r", "--name-only", "refs/heads/root/behind-dirty")
	assert.Contains(t, files, "tracked.txt")
	assert.Contains(t, files, "untracked.txt")
	assert.Contains(t, runGitOut(t, bare, "show", "refs/heads/root/behind-dirty:tracked.txt"), "agent edit")
	assert.Contains(t, runGitOut(t, bare, "show", "refs/heads/root/behind-dirty:untracked.txt"), "new work")

	// The worktree is clean after — nothing stranded for a retry to re-fail on.
	assert.Empty(t, strings.TrimSpace(runGitOut(t, wt, "status", "--porcelain")))
}

// TestSnapshotAndPushBranch_BehindDirtyRetryIsIdempotent pins the other half
// of the stranding bug: the lost-restore recovery loop re-issues the identical
// failing push on every retry, burning its whole budget on deterministic
// non-fast-forward rejections. With the fix the FIRST call succeeds, and a
// retry (a second SnapshotAndPushBranch call) is a clean no-op — HEAD already
// equals origin's tip — rather than a re-failure.
func TestSnapshotAndPushBranch_BehindDirtyRetryIsIdempotent(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/behind-retry")
	wt := gw.GetWorktreePath()
	_, _ = advanceOriginAndResetHeadBehind(t, gw, bare, "root/behind-retry")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "work.txt"), []byte("precious\n"), 0o644))

	// First archive attempt (the one the old code wedged on).
	_, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	tipAfterFirst := branchTip(t, bare, "refs/heads/root/behind-retry")

	// The recovery loop's retry: a second call must not re-fail. HEAD now
	// equals origin's tip and the tree is clean, so this is a no-op push.
	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, "root/behind-retry", got)
	assert.Equal(t, tipAfterFirst, branchTip(t, bare, "refs/heads/root/behind-retry"),
		"the retry must not change origin's tip")
	assert.Equal(t, tipAfterFirst, branchTip(t, wt, "HEAD"))
}

// TestSnapshotAndPushBranch_DivergedRefusesWithoutStranding pins that a
// GENUINELY divergent HEAD (neither ancestor of the other) is still refused —
// the non-force guarantee from #1721/#1733 — but now refused BEFORE any
// snapshot commit is created, with an error that names the condition and
// distinguishes it from the recoverable behind-only case. The old code created
// the WIP commit first and then failed the push, stranding the commit; this
// test asserts HEAD does not advance and the uncommitted work stays
// uncommitted, so neither --force-reap nor `af sessions kill` discards a
// pushed-but-rejected WIP.
func TestSnapshotAndPushBranch_DivergedRefusesWithoutStranding(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/diverged")
	wt := gw.GetWorktreePath()

	// Establish origin's tip, then diverge the worktree HEAD from it.
	runGit(t, wt, "commit", "--allow-empty", "-m", "origin advance")
	runGit(t, wt, "push", "origin", "root/diverged")
	originTip := branchTip(t, bare, "refs/heads/root/diverged")
	runGit(t, wt, "reset", "--hard", "HEAD~1")
	runGit(t, wt, "commit", "--allow-empty", "-m", "divergent local")
	divergentTip := branchTip(t, wt, "HEAD")
	require.NotEqual(t, originTip, divergentTip, "precondition: HEAD must diverge from origin")

	// Uncommitted work the old code would have committed as a stranded WIP.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "uncommitted.txt"), []byte("precious\n"), 0o644))
	require.NotEmpty(t, strings.TrimSpace(runGitOut(t, wt, "status", "--porcelain")))

	_, err := gw.SnapshotAndPushBranch()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "diverged",
		"the error must name the divergence and distinguish it from the recoverable behind-only case")

	// No snapshot commit was created: HEAD is unchanged.
	assert.Equal(t, divergentTip, branchTip(t, wt, "HEAD"),
		"HEAD must not advance on a refused push (no stranded WIP commit)")
	// The uncommitted work is still uncommitted in the worktree, not stranded
	// as a commit the retry loop would then re-fail on.
	assert.NotEmpty(t, strings.TrimSpace(runGitOut(t, wt, "status", "--porcelain")),
		"uncommitted work must remain uncommitted, not stranded as a WIP commit")
	// origin is untouched.
	assert.Equal(t, originTip, branchTip(t, bare, "refs/heads/root/diverged"))
}

// TestSnapshotAndPushBranch_FirstArchiveSkipsClassification pins that a
// brand-new session branch (the branch does not yet exist on origin) is NOT
// misclassified as diverged: the fetch of the not-yet-existent ref leaves
// FETCH_HEAD unset, so the classification is skipped and the push creates the
// branch on origin — the normal first-archive path.
func TestSnapshotAndPushBranch_FirstArchiveSkipsClassification(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/first")
	wt := gw.GetWorktreePath()
	// origin does NOT have root/first yet (pushTestRepo only clones the main
	// branch). Leave uncommitted work and archive.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "work.txt"), []byte("precious\n"), 0o644))

	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, "root/first", got)

	pushed := runGitOut(t, bare, "ls-tree", "-r", "--name-only", "refs/heads/root/first")
	assert.Contains(t, pushed, "work.txt",
		"first archive must create the branch on origin carrying the uncommitted work")
}

// TestSnapshotAndPushBranch_BehindCleanDoesNotTouchOrigin verifies the
// fast-forward reconciliation is genuinely non-destructive on origin: when
// HEAD is behind and the tree is clean, origin's tip is byte-identical before
// and after the (no-op) push. This is the "overwrites nothing" half of the
// bug — the behind-only push protects nothing on origin, so it must not
// rewrite it.
func TestSnapshotAndPushBranch_BehindCleanDoesNotTouchOrigin(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/behind-notouch")
	originTip, _ := advanceOriginAndResetHeadBehind(t, gw, bare, "root/behind-notouch")

	_, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)

	assert.Equal(t, originTip, branchTip(t, bare, "refs/heads/root/behind-notouch"),
		"a behind-only clean archive must not rewrite origin's tip")
}

// TestSnapshotAndPushBranch_BehindCleanSyncsSubmoduleOnRetry pins the
// submodule-gitlink retry regression: a behind, clean worktree whose origin
// advanced an initialized submodule. The fast-forward (reset --hard) advances
// the gitlink in the index without checking out the new submodule commit, so a
// re-checked `git status` would report ` M <submodule>` and send a retry into
// the dirty path where `add -A` stages the stale gitlink and pushes a commit
// that reverts origin's submodule update. The fix synchronizes the submodule
// checkout after the fast-forward (git submodule update --recursive) so the
// retry sees a clean tree and takes the no-op clean path.
func TestSnapshotAndPushBranch_BehindCleanSyncsSubmoduleOnRetry(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/behind-sub")

	// Add a submodule to the session branch and push the initial state.
	wt := gw.GetWorktreePath()
	// protocol.file.allow=always is required for git submodule operations
	// against local paths (the bare sub-repo); the production code path
	// (SnapshotAndPushBranch's submodule update) runs via runGitNetworkCommand
	// which does not pass -c, so the config must be set in the repo.
	runGit(t, wt, "config", "protocol.file.allow", "always")
	subPath := filepath.Join(wt, "mysub")
	subBare := filepath.Join(t.TempDir(), "sub.git")
	require.NoError(t, exec.Command("git", "init", "--bare", subBare).Run())
	runGit(t, subBare, "config", "user.email", "af@agent-factory.local")
	runGit(t, subBare, "config", "user.name", "Agent Factory")
	subClone := filepath.Join(t.TempDir(), "sub-clone")
	runGit(t, "", "-c", "protocol.file.allow=always", "clone", subBare, subClone)
	runGit(t, subClone, "config", "user.email", "af@agent-factory.local")
	runGit(t, subClone, "config", "user.name", "Agent Factory")
	require.NoError(t, os.WriteFile(filepath.Join(subClone, "s.txt"), []byte("sub-v1\n"), 0o644))
	runGit(t, subClone, "add", "-A")
	runGit(t, subClone, "commit", "-m", "sub init")
	runGit(t, subClone, "push", "origin", "refs/heads/master")
	runGit(t, wt, "-c", "protocol.file.allow=always", "submodule", "add", subBare, "mysub")
	runGit(t, wt, "commit", "-m", "add submodule")
	runGit(t, wt, "push", "origin", "root/behind-sub")

	// Advance the submodule on its own remote and update the gitlink on origin's
	// session branch, then reset the worktree HEAD back to the ancestor (the
	// behind state a restored sandbox lands in).
	require.NoError(t, os.WriteFile(filepath.Join(subClone, "s.txt"), []byte("sub-v2\n"), 0o644))
	runGit(t, subClone, "add", "-A")
	runGit(t, subClone, "commit", "-m", "sub advance")
	runGit(t, subClone, "push", "origin", "refs/heads/master")
	// Update the gitlink in the worktree to the advanced submodule, commit, and
	// push so origin's session branch references sub-v2.
	runGit(t, subPath, "fetch", "origin")
	runGit(t, subPath, "checkout", "origin/master")
	runGit(t, wt, "add", "mysub")
	runGit(t, wt, "commit", "-m", "advance submodule gitlink")
	runGit(t, wt, "push", "origin", "root/behind-sub")
	originTip := branchTip(t, bare, "refs/heads/root/behind-sub")
	// Reset to the ancestor (before the submodule gitlink advance) — the
	// submodule gitlink is at sub-v1 but the checkout is still at sub-v2
	// (reset --hard does not recurse into submodules). Sync the checkout to
	// the ancestor's gitlink so the tree is clean and the archive takes the
	// clean-behind path (the path the fix targets).
	runGit(t, wt, "reset", "--hard", "HEAD~1")
	runGit(t, wt, "-c", "protocol.file.allow=always", "submodule", "update", "--recursive")
	ancestorHead := branchTip(t, wt, "HEAD")
	require.NotEqual(t, ancestorHead, originTip, "precondition: HEAD must be behind origin's tip")
	// The submodule checkout is at sub-v1 (the ancestor's gitlink).
	require.Equal(t, "sub-v1\n", readFile(t, filepath.Join(subPath, "s.txt")),
		"precondition: submodule checkout at sub-v1 before the fast-forward")
	require.Empty(t, strings.TrimSpace(runGitOut(t, wt, "status", "--porcelain")),
		"precondition: tree must be clean after submodule sync")

	// First archive (clean, behind): the fast-forward advances the gitlink to
	// sub-v2 and the submodule update synchronizes the checkout so a retry
	// does not see ` M <submodule>`.
	_, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, originTip, branchTip(t, bare, "refs/heads/root/behind-sub"),
		"clean behind archive must not rewrite origin's tip")
	// The submodule checkout is now at sub-v2 (synchronized by the fix).
	assert.Equal(t, "sub-v2\n", readFile(t, filepath.Join(subPath, "s.txt")),
		"submodule checkout must be synchronized to origin's gitlink after the fast-forward")

	// Retry (the ArchiveSandbox reap-failure path re-issues the call): the
	// worktree must be clean (no ` M <submodule>`), so the retry takes the
	// no-op clean path and does NOT revert origin's submodule update.
	_, err = gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, "sub-v2\n", readFile(t, filepath.Join(subPath, "s.txt")),
		"retry must not revert the submodule checkout")
	assert.Equal(t, originTip, branchTip(t, bare, "refs/heads/root/behind-sub"),
		"retry must not rewrite origin's tip")
}

// readFile is defined in worktree_rebuild_hooks_test.go (shared test helper).

// TestSnapshotAndPushBranch_BehindCleanFetchesFromPushURL pins the push-URL
// reconciliation: when origin has a separate pushurl (an upstream/read-only
// source for fetch, the user's fork for push), the fetch must read from the
// push URL so the classification inspects the same server the push targets.
// Without the fix, the fetch reads from the fetch URL (the upstream) and the
// push writes to the push URL (the fork): a same-named branch on the upstream
// could report a false divergence or reset HEAD onto an unrelated tip the push
// then carries to the fork.
func TestSnapshotAndPushBranch_BehindCleanFetchesFromPushURL(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/pushurl")

	// Create a SECOND bare repo to act as the "upstream" (fetch URL) and
	// reconfigure origin's pushurl to point at the original bare (the fork).
	wt := gw.GetWorktreePath()
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	require.NoError(t, exec.Command("git", "clone", "--bare", bare, upstream).Run())
	// origin fetches from the upstream but pushes to the fork (bare).
	runGit(t, wt, "remote", "set-url", "--push", "origin", bare)
	runGit(t, wt, "remote", "set-url", "origin", upstream)

	// Advance the fork (push URL) so the worktree is behind the fork but not
	// behind the upstream — the fetch must read the fork to detect the
	// behind-only state.
	originTip, _ := advanceOriginAndResetHeadBehind(t, gw, bare, "root/pushurl")

	// The fetch from the push URL (the fork) detects the behind-only state
	// and fast-forwards HEAD onto the fork's tip. A fetch from the upstream
	// would see no advance (the upstream was just cloned from the fork's
	// pre-advance state) and classify HEAD as equal/ahead, skipping the
	// reconciliation.
	_, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, originTip, branchTip(t, wt, "HEAD"),
		"HEAD must fast-forward to the push URL's tip, not the fetch URL's")
}

// TestSnapshotAndPushBranch_BehindDirtyPreservesOutOfConePaths pins the
// sparse-checkout regression: when the worktree uses a sparse checkout and
// origin advanced a path outside the sparse cone, the behind-dirty snapshot
// must carry the user's in-cone changes on top of origin's tip WITHOUT
// reverting the out-of-cone advance. Without the fix, `add -A` cannot update
// the out-of-cone index entry (skip-worktree), so `write-tree` attaches the
// stale HEAD blob and the push silently reverts origin's out-of-cone change.
func TestSnapshotAndPushBranch_BehindDirtyPreservesOutOfConePaths(t *testing.T) {
	sandboxHome(t)
	gw, bare := pushTestRepo(t, "root/sparse")

	// Set up a sparse checkout: the cone contains only `sub/`, and
	// `outside.txt` is out-of-cone.
	wt := gw.GetWorktreePath()
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "sub", "in.txt"), []byte("in-v1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "outside.txt"), []byte("out-v1\n"), 0o644))
	runGit(t, wt, "add", "-A")
	runGit(t, wt, "commit", "-m", "init with sub/ and outside.txt")
	runGit(t, wt, "push", "origin", "root/sparse")
	runGit(t, wt, "sparse-checkout", "set", "--no-cone", "sub")
	// outside.txt is now out-of-cone: skip-worktree in the index, absent from
	// the working tree.
	require.NoFileExists(t, filepath.Join(wt, "outside.txt"))

	// Advance origin's out-of-cone path (outside.txt) from a separate clone
	// (the worktree's sparse cone excludes it, so the advance must not be
	// staged by `add -A`).
	advanceClone := filepath.Join(t.TempDir(), "advance")
	runGit(t, "", "clone", bare, advanceClone)
	runGit(t, advanceClone, "config", "user.email", "af@agent-factory.local")
	runGit(t, advanceClone, "config", "user.name", "Agent Factory")
	runGit(t, advanceClone, "checkout", "root/sparse")
	require.NoError(t, os.WriteFile(filepath.Join(advanceClone, "outside.txt"), []byte("out-v2\n"), 0o644))
	runGit(t, advanceClone, "add", "-A")
	runGit(t, advanceClone, "commit", "-m", "advance out-of-cone outside.txt")
	runGit(t, advanceClone, "push", "origin", "root/sparse")

	// The worktree is now behind origin: its HEAD is at the commit with
	// outside.txt=v1 (C0), while origin's root/sparse is at C1
	// (outside.txt=v2). The sparse cone is sub/, so outside.txt is
	// out-of-cone in the worktree (skip-worktree in the index, absent from
	// the working tree).

	// Leave in-cone dirty work (the user's change the snapshot must carry).
	require.NoError(t, os.WriteFile(filepath.Join(wt, "sub", "in.txt"), []byte("user-edit\n"), 0o644))
	require.NotEmpty(t, strings.TrimSpace(runGitOut(t, wt, "status", "--porcelain")),
		"precondition: in-cone tree must be dirty")

	_, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)

	// The push must carry the user's in-cone change AND preserve origin's
	// out-of-cone advance — not revert outside.txt to the stale HEAD blob.
	pushed := runGitOut(t, bare, "show", "refs/heads/root/sparse:outside.txt")
	assert.Contains(t, pushed, "out-v2",
		"out-of-cone path advanced by origin must NOT be reverted by the behind-dirty snapshot")
	pushedIn := runGitOut(t, bare, "show", "refs/heads/root/sparse:sub/in.txt")
	assert.Contains(t, pushedIn, "user-edit",
		"in-cone user change must be carried by the behind-dirty snapshot")
}

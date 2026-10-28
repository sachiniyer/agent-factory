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

// commitOnOrigin makes a commit on origin's branch from outside the sandbox
// (another clone), the way a teammate or an earlier session would.
func commitOnOrigin(t *testing.T, bare, branch string, files map[string]string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "other")
	require.NoError(t, exec.Command("git", "clone", "-q", "--branch", branch, bare, clone).Run())
	runGit(t, clone, "config", "user.email", "o@agent-factory.local")
	runGit(t, clone, "config", "user.name", "Other")
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(clone, name), []byte(body), 0644))
	}
	runGit(t, clone, "add", "-A")
	runGit(t, clone, "commit", "-m", "from origin")
	runGit(t, clone, "push", "origin", "HEAD:refs/heads/"+branch)
	return branchTip(t, clone, "HEAD")
}

func treeFiles(t *testing.T, repo, rev string) string {
	t.Helper()
	return runGitOut(t, repo, "ls-tree", "-r", "--name-only", rev)
}

// TestSnapshotAndPushBranch_BehindOnlyCleanHeadIsAlreadyDurable: a clean HEAD that
// is an ancestor of origin's session branch has nothing to push — origin already
// holds every commit — so the archive succeeds instead of aborting on a
// non-fast-forward rejection, and origin's tip never moves backwards (#5407).
func TestSnapshotAndPushBranch_BehindOnlyCleanHeadIsAlreadyDurable(t *testing.T) {
	sandboxHome(t)
	gw, bare, _, tipB := behindOriginRepo(t, "root/behind-clean")

	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err, "a behind-only HEAD is recoverable, not divergent")
	assert.Equal(t, "root/behind-clean", got)
	assert.Equal(t, tipB, branchTip(t, bare, "refs/heads/root/behind-clean"), "origin's tip must not move backwards")
}

// TestSnapshotAndPushBranch_BehindDirtyMergesWithoutTouchingTheWorktree: uncommitted
// work on a behind HEAD is captured in the snapshot FIRST, then merged with
// origin's tip in the object database and pushed as a fast-forward. The sandbox
// working tree is never rewritten — origin's b.txt does not appear in it (#5407).
func TestSnapshotAndPushBranch_BehindDirtyMergesWithoutTouchingTheWorktree(t *testing.T) {
	sandboxHome(t)
	gw, bare, _, tipB := behindOriginRepo(t, "root/behind-dirty")
	wt := gw.GetWorktreePath()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("uncommitted"), 0644))

	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, "root/behind-dirty", got)

	pushed := branchTip(t, bare, "refs/heads/root/behind-dirty")
	runGit(t, bare, "merge-base", "--is-ancestor", tipB, pushed) // fast-forward of origin's tip
	files := treeFiles(t, bare, pushed)
	assert.Contains(t, files, "dirty.txt", "the uncommitted work is in what origin holds")
	assert.Contains(t, files, "b.txt", "origin's own commits are kept")
	assert.NoFileExists(t, filepath.Join(wt, "b.txt"), "the archive must not rewrite the sandbox working tree")
}

// An untracked file that the INCOMING .gitignore would ignore must still be in the
// snapshot: the work is recorded under the worktree's current ignore rules before
// anything else happens (#5407).
func TestSnapshotAndPushBranch_UntrackedFileSurvivesAnIncomingIgnoreRule(t *testing.T) {
	sandboxHome(t)
	gw, bare, _, _ := behindOriginRepo(t, "root/ignore-rule")
	wt := gw.GetWorktreePath()
	commitOnOrigin(t, bare, "root/ignore-rule", map[string]string{".gitignore": "secret.txt\n"})
	require.NoError(t, os.WriteFile(filepath.Join(wt, "secret.txt"), []byte("my work"), 0644))

	_, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)

	pushed := branchTip(t, bare, "refs/heads/root/ignore-rule")
	assert.Contains(t, treeFiles(t, bare, pushed), "secret.txt", "the untracked file must be in the pushed snapshot")
	assert.Equal(t, "my work", strings.TrimSpace(runGitOut(t, bare, "show", pushed+":secret.txt")))
}

// With merge.autoStash / rebase.autoStash enabled the archive must still never
// write refs/stash — a stack shared by every linked worktree (CLAUDE.md "Git
// hygiene in a shared repo"). The path performs no merge or rebase on the working
// tree at all; a reference-transaction hook records any refs/stash update, even a
// transient push-then-pop.
func TestSnapshotAndPushBranch_NeverTouchesTheSharedStash(t *testing.T) {
	sandboxHome(t)
	gw, bare, _, _ := behindOriginRepo(t, "root/autostash")
	wt := gw.GetWorktreePath()
	runGit(t, wt, "config", "merge.autoStash", "true")
	runGit(t, wt, "config", "rebase.autoStash", "true")
	commitOnOrigin(t, bare, "root/autostash", map[string]string{"a.txt": "a remote"})

	hooks := filepath.Join(strings.TrimSpace(runGitOut(t, wt, "rev-parse", "--git-common-dir")), "hooks")
	if !filepath.IsAbs(hooks) {
		hooks = filepath.Join(wt, hooks)
	}
	require.NoError(t, os.MkdirAll(hooks, 0o755))
	trace := filepath.Join(t.TempDir(), "stash-writes")
	hook := "#!/bin/sh\nif grep -q 'refs/stash' ; then echo touched >> '" + trace + "'; fi\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(hook), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a local"), 0o644)) // conflicts with origin's edit
	_, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.NoFileExists(t, trace, "the archive must never write refs/stash")
	assert.Equal(t, "a local", strings.TrimSpace(mustRead(t, filepath.Join(wt, "a.txt"))), "the sandbox working tree is untouched")
}

// A divergence that does not combine cleanly (both sides edited a.txt) must not
// fail the archive and must not lose the work: HEAD goes to a side branch that
// nothing else writes, the session branch on origin is left exactly as it was,
// and the side branch's name is returned so the caller records it for restore.
func TestSnapshotAndPushBranch_ConflictingDivergenceGoesToASideBranch(t *testing.T) {
	sandboxHome(t)
	gw, bare, _, _ := behindOriginRepo(t, "root/conflict")
	wt := gw.GetWorktreePath()
	tipOrigin := commitOnOrigin(t, bare, "root/conflict", map[string]string{"a.txt": "a remote"})
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a local"), 0o644))

	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err, "a conflicting divergence must not fail the archive and strand the work")
	assert.True(t, strings.HasPrefix(got, "root/conflict-af-archive-"), "the returned branch names the side ref, got %q", got)
	assert.Equal(t, tipOrigin, branchTip(t, bare, "refs/heads/root/conflict"), "origin's session branch is left exactly as it was")
	side := branchTip(t, bare, "refs/heads/"+got)
	assert.Equal(t, branchTip(t, wt, "HEAD"), side)
	assert.Equal(t, "a local", strings.TrimSpace(runGitOut(t, bare, "show", side+":a.txt")), "the local work is on the side branch")
}

// A committed divergence that merges cleanly is pushed as a fast-forward of the
// session branch (no side branch).
func TestSnapshotAndPushBranch_CleanDivergenceMergesAndFastForwards(t *testing.T) {
	sandboxHome(t)
	gw, bare, _, tipB := behindOriginRepo(t, "root/diverged")
	wt := gw.GetWorktreePath()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "c.txt"), []byte("c"), 0644))
	runGit(t, wt, "add", "-A")
	runGit(t, wt, "commit", "-m", "C")

	got, err := gw.SnapshotAndPushBranch()
	require.NoError(t, err)
	assert.Equal(t, "root/diverged", got)
	pushed := branchTip(t, bare, "refs/heads/root/diverged")
	runGit(t, bare, "merge-base", "--is-ancestor", tipB, pushed)
	files := treeFiles(t, bare, pushed)
	assert.Contains(t, files, "b.txt")
	assert.Contains(t, files, "c.txt")
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	return string(b)
}

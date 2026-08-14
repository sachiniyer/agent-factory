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

// branchRepo builds a THROWAWAY repo in t.TempDir() with one commit and one
// extra local branch, and returns its root. Like resetRepoWithWorktree, it uses
// the git CLI directly: these tests exercise the factory reset's free-function
// branch-deletion path, which takes only a repo root and a branch name.
func branchRepo(t *testing.T, branch string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), string(out))
	}
	git("init", "-q", "-b", "master", ".")
	git("commit", "-q", "--allow-empty", "-m", "initial")
	git("branch", branch)
	return root
}

// refExists asks git directly (bypassing the code under test) whether the local
// branch resolves in the repo at root.
func refExists(t *testing.T, root, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
}

// TestDeleteLocalBranch_ExistingBranchIsDeleted is the happy-path guard: the
// #3243 disambiguation must not turn a normal deletion into a refusal.
func TestDeleteLocalBranch_ExistingBranchIsDeleted(t *testing.T) {
	root := branchRepo(t, "af-normal")

	deleted, err := DeleteLocalBranch(root, "af-normal")
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.False(t, refExists(t, root, "af-normal"))
}

// TestDeleteLocalBranch_MissingBranchIsCleanNoOp keeps the idempotence
// contract: a determinately absent ref is a silent no-op, not an error.
func TestDeleteLocalBranch_MissingBranchIsCleanNoOp(t *testing.T) {
	root := branchRepo(t, "af-once")

	deleted, err := DeleteLocalBranch(root, "af-once")
	require.NoError(t, err)
	require.True(t, deleted)

	deleted, err = DeleteLocalBranch(root, "af-once")
	require.NoError(t, err, "a second deletion must be a clean no-op")
	assert.False(t, deleted)
}

// TestDeleteLocalBranch_MissingRepoIsCleanNoOp pins determinate absence at the
// repo level: a repo the user deleted holds no local branches, so `af reset`
// over a stale record stays idempotent (same rule as
// TestRemoveWorktreeDir_DeletedRepoStillRemovesOrphanDir).
func TestDeleteLocalBranch_MissingRepoIsCleanNoOp(t *testing.T) {
	root := branchRepo(t, "af-orphan")
	require.NoError(t, os.RemoveAll(root))

	deleted, err := DeleteLocalBranch(root, "af-orphan")
	require.NoError(t, err, "a deleted repo is determinate absence, not a failed probe")
	assert.False(t, deleted)
}

// TestDeleteLocalBranch_DeGittedRepoIsCleanNoOp pins the de-git'd edge the same
// way RemoveWorktreeDir settles it (#2110): the directory survives but .git is
// gone, so no repo is left to hold the ref. Treating this as a probe failure
// would retain a session record no re-run could ever clear.
func TestDeleteLocalBranch_DeGittedRepoIsCleanNoOp(t *testing.T) {
	root := branchRepo(t, "af-degitted")
	require.NoError(t, os.RemoveAll(filepath.Join(root, ".git")))

	deleted, err := DeleteLocalBranch(root, "af-degitted")
	require.NoError(t, err, "a repo with no .git is determinate absence, not a failed probe")
	assert.False(t, deleted)
}

// TestDeleteLocalBranch_UnreadableRefsStorageIsErrorNotAbsence is the #3243
// regression lock. `git show-ref --verify --quiet` exits non-zero both for a
// missing ref (exit 1) and for an operational failure reading the refs storage
// (exit 128, e.g. EACCES on packed-refs), and the old `Run() == nil` collapsed
// the two. The factory reset then read "probe failed" as "branch absent",
// recorded no error, and deleted the session records that were the only durable
// pointer to the branch — orphaning a branch that still exists.
func TestDeleteLocalBranch_UnreadableRefsStorageIsErrorNotAbsence(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based EACCES is bypassed by root; this scenario needs a non-root uid")
	}
	root := branchRepo(t, "af-kept")
	// Pack the refs so the probe must read packed-refs, then make that file
	// unreadable: the branch still exists, but no ref in this repo can be read.
	pack := exec.Command("git", "-C", root, "pack-refs", "--all")
	out, err := pack.CombinedOutput()
	require.NoError(t, err, string(out))
	packed := filepath.Join(root, ".git", "packed-refs")
	require.NoError(t, os.Chmod(packed, 0o000))
	t.Cleanup(func() { _ = os.Chmod(packed, 0o644) })

	deleted, err := DeleteLocalBranch(root, "af-kept")
	require.Error(t, err,
		"a failed existence probe is UNKNOWN, not absent — it must surface as an error (#3243)")
	assert.False(t, deleted)
	assert.Contains(t, err.Error(), "af-kept", "the error must name the branch it could not check")
	assert.Contains(t, err.Error(), root, "the error must name the repo it could not check")

	// The branch is still there — the silent "absent" reading would have been a
	// fabricated negative.
	require.NoError(t, os.Chmod(packed, 0o644))
	assert.True(t, refExists(t, root, "af-kept"),
		"sanity: the branch the old code reported absent exists the whole time")
}

// TestDeleteLocalBranch_UnreadableGitDirIsErrorNotAbsence covers the discovery
// half of the same class: .git is present but unreadable, so git cannot even
// find the repo ("not a git repository", exit 128). Present-but-unreadable is a
// probe failure, not evidence of absence.
func TestDeleteLocalBranch_UnreadableGitDirIsErrorNotAbsence(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based EACCES is bypassed by root; this scenario needs a non-root uid")
	}
	root := branchRepo(t, "af-hidden")
	gitDir := filepath.Join(root, ".git")
	require.NoError(t, os.Chmod(gitDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })

	deleted, err := DeleteLocalBranch(root, "af-hidden")
	require.Error(t, err, "an unreadable .git is a failed probe, not a missing branch (#3243)")
	assert.False(t, deleted)

	require.NoError(t, os.Chmod(gitDir, 0o755))
	assert.True(t, refExists(t, root, "af-hidden"),
		"sanity: the branch exists the whole time")
}

// TestLocalBranchExists_DeterminateVersusUnknown pins the probe's contract
// directly: err == nil is the ONLY determinate surface, and a probe that could
// not run returns an error rather than a fabricated "false".
func TestLocalBranchExists_DeterminateVersusUnknown(t *testing.T) {
	root := branchRepo(t, "af-probe")

	exists, err := LocalBranchExists(root, "af-probe")
	require.NoError(t, err)
	assert.True(t, exists)

	exists, err = LocalBranchExists(root, "no-such-branch")
	require.NoError(t, err, "a silent show-ref exit 1 is determinate absence")
	assert.False(t, exists)

	exists, err = LocalBranchExists("", "af-probe")
	require.NoError(t, err, "empty arguments name no branch to act on")
	assert.False(t, exists)

	if os.Geteuid() != 0 {
		gitDir := filepath.Join(root, ".git")
		require.NoError(t, os.Chmod(gitDir, 0o000))
		t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })
		_, err = LocalBranchExists(root, "af-probe")
		require.Error(t, err, "an unrunnable probe must not answer (#3243)")
		require.NoError(t, os.Chmod(gitDir, 0o755))
	}
}

// TestDeleteLocalBranch_UnreadableLooseRefIsErrorNotAbsence closes the hole the
// PR review caught: git itself folds an EACCES on a LOOSE ref file into the
// same silent exit 1 as "no such ref" (git 2.43; git ≥2.45 grew
// `show-ref --exists` to tell them apart), so trusting the exit code alone
// still reports determinate absence while the branch sits unreadable on disk.
// Determinate absence must also be confirmed by direct observation: no loose
// ref file at the branch's path.
func TestDeleteLocalBranch_UnreadableLooseRefIsErrorNotAbsence(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based EACCES is bypassed by root; this scenario needs a non-root uid")
	}
	root := branchRepo(t, "af-kept")
	loose := filepath.Join(root, ".git", "refs", "heads", "af-kept")
	require.NoError(t, os.Chmod(loose, 0o000))
	t.Cleanup(func() { _ = os.Chmod(loose, 0o644) })

	deleted, err := DeleteLocalBranch(root, "af-kept")
	require.Error(t, err,
		"an unreadable loose ref is a failed probe, not a missing branch — git's silent exit 1 must not be trusted alone")
	assert.False(t, deleted)
	assert.Contains(t, err.Error(), "af-kept", "the error must name the branch it could not check")

	require.NoError(t, os.Chmod(loose, 0o644))
	assert.True(t, refExists(t, root, "af-kept"),
		"sanity: the branch exists the whole time")
}

// TestDeleteLocalBranch_CorruptLooseRefIsErrorNotAbsence pins the same
// direct-observation rule for a loose ref file git cannot parse: a file is
// present that git reports as no ref, which is a contradiction to surface, not
// an absence to act on.
func TestDeleteLocalBranch_CorruptLooseRefIsErrorNotAbsence(t *testing.T) {
	root := branchRepo(t, "af-normal")
	loose := filepath.Join(root, ".git", "refs", "heads", "af-broken")
	require.NoError(t, os.WriteFile(loose, []byte("not-a-sha\n"), 0o644))

	deleted, err := DeleteLocalBranch(root, "af-broken")
	require.Error(t, err, "a present-but-unparseable loose ref file must not read as absence")
	assert.False(t, deleted)
}

// TestDeleteLocalBranch_StaleRefHierarchyDirIsCleanNoOp guards the refinement
// against over-retaining: deleting a hierarchical branch (a/b) can leave an
// empty directory at refs/heads/a, and a DIRECTORY at the branch path can never
// be a loose ref — probing branch "a" must stay a clean, determinate no-op.
func TestDeleteLocalBranch_StaleRefHierarchyDirIsCleanNoOp(t *testing.T) {
	root := branchRepo(t, "af-normal")
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git", "refs", "heads", "stale"), 0o755))

	deleted, err := DeleteLocalBranch(root, "stale")
	require.NoError(t, err, "a stale ref-hierarchy directory is not evidence of a branch")
	assert.False(t, deleted)
}

// TestDeleteLocalBranch_NestedDeGittedRootNeverTouchesEnclosingRepo pins the
// sharpest consequence of probing with `git -C`: discovery walks UPWARD, so a
// record whose de-git'd root sits inside another repo would resolve — and then
// delete — a same-named branch in the ENCLOSING repo, which AF never created.
// Settling "no .git at the recorded root" as determinate absence before any git
// command runs is what keeps the parent repo out of reach.
func TestDeleteLocalBranch_NestedDeGittedRootNeverTouchesEnclosingRepo(t *testing.T) {
	parent := branchRepo(t, "af-shared-name")
	sub := filepath.Join(parent, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	deleted, err := DeleteLocalBranch(sub, "af-shared-name")
	require.NoError(t, err, "a root with no repo is determinate absence")
	assert.False(t, deleted)
	assert.True(t, refExists(t, parent, "af-shared-name"),
		"the enclosing repo's branch must never be deleted through a nested record root")
}

// linkedWorktreeOf adds a linked worktree of the repo at root (on its own new
// branch, so no branch under test is ever checked out there) and returns its
// path. In a linked worktree .git is a FILE pointing at the private gitdir, and
// the loose refs live in the main repo's common dir — the layout the loose-ref
// confirmation must resolve rather than assume.
func linkedWorktreeOf(t *testing.T, root string) string {
	t.Helper()
	lw := filepath.Join(t.TempDir(), "lw")
	runGit(t, root, "worktree", "add", "-q", "-b", "af-lw-holder", lw)
	return lw
}

// TestDeleteLocalBranch_LinkedWorktreeUnreadableLooseRefIsErrorNotAbsence pins
// the gitfile half of the loose-ref confirmation: from a linked-worktree root,
// <root>/.git is a file, so a naive Lstat under it can never find the loose
// ref — every probe would "confirm" absence while the unreadable ref sits in
// the main repo's common dir. The confirmation must resolve the common dir and
// observe the real file, and report the failed probe.
func TestDeleteLocalBranch_LinkedWorktreeUnreadableLooseRefIsErrorNotAbsence(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based EACCES is bypassed by root; this scenario needs a non-root uid")
	}
	main := branchRepo(t, "af-kept")
	lw := linkedWorktreeOf(t, main)
	loose := filepath.Join(main, ".git", "refs", "heads", "af-kept")
	require.NoError(t, os.Chmod(loose, 0o000))
	t.Cleanup(func() { _ = os.Chmod(loose, 0o644) })

	deleted, err := DeleteLocalBranch(lw, "af-kept")
	require.Error(t, err,
		"an unreadable loose ref must be a failed probe from a linked-worktree root too, not confirmed absence")
	assert.False(t, deleted)
	assert.Contains(t, err.Error(), "af-kept", "the error must name the branch it could not check")

	require.NoError(t, os.Chmod(loose, 0o644))
	assert.True(t, refExists(t, main, "af-kept"),
		"sanity: the branch exists the whole time")
}

// TestDeleteLocalBranch_SeparateGitDirUnreadableLooseRefIsErrorNotAbsence is
// the same rule for a MAIN worktree whose .git is a gitfile because the repo
// was made with --separate-git-dir: the refs live wherever that file points.
func TestDeleteLocalBranch_SeparateGitDirUnreadableLooseRefIsErrorNotAbsence(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based EACCES is bypassed by root; this scenario needs a non-root uid")
	}
	root := filepath.Join(t.TempDir(), "repo")
	gitdir := filepath.Join(t.TempDir(), "gitdir")
	require.NoError(t, os.MkdirAll(root, 0o755))
	runGit(t, root, "init", "-q", "-b", "master", "--separate-git-dir", gitdir, ".")
	runGit(t, root, "commit", "-q", "--allow-empty", "-m", "initial")
	runGit(t, root, "branch", "af-kept")
	loose := filepath.Join(gitdir, "refs", "heads", "af-kept")
	require.NoError(t, os.Chmod(loose, 0o000))
	t.Cleanup(func() { _ = os.Chmod(loose, 0o644) })

	deleted, err := DeleteLocalBranch(root, "af-kept")
	require.Error(t, err,
		"an unreadable loose ref behind a --separate-git-dir gitfile must be a failed probe, not confirmed absence")
	assert.False(t, deleted)

	require.NoError(t, os.Chmod(loose, 0o644))
	assert.True(t, refExists(t, root, "af-kept"),
		"sanity: the branch exists the whole time")
}

// TestDeleteLocalBranch_LinkedWorktreeMissingBranchIsCleanNoOp guards the
// resolution against over-retaining: a genuinely absent branch probed from a
// linked-worktree root must stay a determinate, silent no-op.
func TestDeleteLocalBranch_LinkedWorktreeMissingBranchIsCleanNoOp(t *testing.T) {
	main := branchRepo(t, "af-once")
	lw := linkedWorktreeOf(t, main)

	deleted, err := DeleteLocalBranch(lw, "af-never-existed")
	require.NoError(t, err, "a determinately missing branch is a clean no-op from any root")
	assert.False(t, deleted)
}

// TestDeleteLocalBranch_LinkedWorktreeExistingBranchIsDeleted keeps the happy
// path working through a gitfile root: refs are shared, so deleting an
// AF-created branch recorded against a linked-worktree root must still work.
func TestDeleteLocalBranch_LinkedWorktreeExistingBranchIsDeleted(t *testing.T) {
	main := branchRepo(t, "af-normal")
	lw := linkedWorktreeOf(t, main)

	deleted, err := DeleteLocalBranch(lw, "af-normal")
	require.NoError(t, err)
	assert.True(t, deleted)
	assert.False(t, refExists(t, main, "af-normal"))
}

package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClassifyRemoveAllFailure pins the #2531 three-way decision directly. When
// os.RemoveAll of a worktree directory fails and git may still own the path — the
// worktree is registered, or its registration could not be determined — the error
// must wrap ErrWorktreeStillRegistered so the factory reset RETAINS the session
// record for a re-run rather than dropping it and orphaning a git-registered
// worktree and branch. Only a confirmed-deregistered path stays a plain error. The
// end-to-end TestRemoveWorktreeDir_CorruptedPointer* tests prove this branch is
// actually reachable from production (it was dead code until stderr was captured).
func TestClassifyRemoveAllFailure(t *testing.T) {
	rmErr := errors.New("permission denied")
	const wt = "/x/wt-orphan"

	// Registered → retain (wrap the sentinel).
	if err := classifyRemoveAllFailure(wt, true, nil, rmErr); !errors.Is(err, ErrWorktreeStillRegistered) {
		t.Errorf("a registered worktree whose removal failed must be retained, got %v", err)
	}
	// Registration undetermined (probe errored) → retain.
	if err := classifyRemoveAllFailure(wt, false, errors.New("probe failed"), rmErr); !errors.Is(err, ErrWorktreeStillRegistered) {
		t.Errorf("an undetermined registration whose removal failed must be retained, got %v", err)
	}
	// Confirmed deregistered → plain error (a filesystem orphan, not a lost registration).
	err := classifyRemoveAllFailure(wt, false, nil, rmErr)
	if errors.Is(err, ErrWorktreeStillRegistered) {
		t.Errorf("a confirmed-deregistered path must not be reported as still-registered, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), wt) {
		t.Errorf("the error must still name the worktree, got %v", err)
	}
	// The underlying os.RemoveAll error is preserved in every case.
	if !errors.Is(classifyRemoveAllFailure(wt, true, nil, rmErr), rmErr) {
		t.Error("the underlying os.RemoveAll error must be preserved for the sentinel case")
	}
	if !errors.Is(classifyRemoveAllFailure(wt, false, nil, rmErr), rmErr) {
		t.Error("the underlying os.RemoveAll error must be preserved for the plain case")
	}
}

// resetRepoWithWorktree builds a THROWAWAY repo in t.TempDir() with one linked
// worktree on its own branch, and returns (repoRoot, worktreePath, branch).
//
// It deliberately uses the git CLI directly rather than NewGitWorktree: these
// tests exercise the factory reset's free-function removal path, which takes
// only a repo root and a worktree path, and must not depend on an AF home.
func resetRepoWithWorktree(t *testing.T, branch string) (string, string, string) {
	t.Helper()

	base := t.TempDir()
	repoRoot := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(repoRoot, 0o755))

	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), string(out))
		return string(out)
	}

	git(repoRoot, "init", "-q", "-b", "master", ".")
	git(repoRoot, "commit", "-q", "--allow-empty", "-m", "initial")

	worktreePath := filepath.Join(base, "wt-"+branch)
	git(repoRoot, "worktree", "add", "-q", "-b", branch, worktreePath)

	return repoRoot, worktreePath, branch
}

// worktreeMetadataDir is the .git/worktrees/<id> admin directory git keeps for a
// linked worktree. Its survival is what blocks `git branch -D`.
func worktreeMetadataDir(repoRoot, worktreePath string) string {
	return filepath.Join(repoRoot, ".git", "worktrees", filepath.Base(worktreePath))
}

// TestRemoveWorktreeDir_LockedWorktreeIsReportedAsIncomplete is the #2110
// regression lock.
//
// `git worktree prune` REFUSES to prune a locked worktree's metadata and still
// exits 0 — "I ran" is not "the metadata is gone". Trusting that exit code made
// reset report success while `.git/worktrees/<id>` survived, which then blocked
// `git branch -D` and left the branch stuck with no recovery path.
func TestRemoveWorktreeDir_LockedWorktreeIsReportedAsIncomplete(t *testing.T) {
	repoRoot, worktreePath, branch := resetRepoWithWorktree(t, "locked")

	lock := exec.Command("git", "-C", repoRoot, "worktree", "lock", worktreePath, "--reason", "still in use")
	out, err := lock.CombinedOutput()
	require.NoError(t, err, string(out))

	_, err = RemoveWorktreeDir(repoRoot, worktreePath)

	// 1. The failure must be SURFACED, not swallowed behind prune's exit 0.
	require.Error(t, err,
		"RemoveWorktreeDir must report failure when the worktree metadata survives (#2110)")
	require.ErrorIs(t, err, ErrWorktreeStillRegistered,
		"the failure must be classifiable so reset can preserve the record")

	// 2. The error must be ACTIONABLE: it must name the real recovery.
	assert.Contains(t, err.Error(), "worktree unlock",
		"the error must tell the user the actual recovery command")
	assert.Contains(t, err.Error(), worktreePath,
		"the error must name the worktree it could not remove")

	// 3. The metadata git refused to prune is still there — that is the fact the
	//    old code inferred away.
	_, statErr := os.Stat(worktreeMetadataDir(repoRoot, worktreePath))
	assert.NoError(t, statErr, "sanity: git keeps a locked worktree's metadata")

	// 4. Ownership safety (#2110 History note): the directory is STILL a
	//    registered git worktree, so it is not ours to os.RemoveAll — the same
	//    rule Cleanup() applies via shouldRemoveWorktreeDir.
	_, statErr = os.Stat(worktreePath)
	assert.NoError(t, statErr,
		"RemoveWorktreeDir must not delete a directory git still registers as a worktree")

	// 5. The consequence the user actually hit: the branch cannot be deleted.
	_, err = DeleteLocalBranch(repoRoot, branch)
	assert.Error(t, err, "sanity: the surviving registration blocks branch deletion")
}

// TestRemoveWorktreeDir_UnlockRecoversTheWorktree proves the guidance in the
// error is REAL: `git worktree unlock` + a re-run finishes the job. Recovery is
// only possible because the failing run left the worktree directory intact.
func TestRemoveWorktreeDir_UnlockRecoversTheWorktree(t *testing.T) {
	repoRoot, worktreePath, branch := resetRepoWithWorktree(t, "recoverable")

	lock := exec.Command("git", "-C", repoRoot, "worktree", "lock", worktreePath, "--reason", "still in use")
	out, err := lock.CombinedOutput()
	require.NoError(t, err, string(out))

	_, err = RemoveWorktreeDir(repoRoot, worktreePath)
	require.Error(t, err)

	// Follow the error's own advice.
	unlock := exec.Command("git", "-C", repoRoot, "worktree", "unlock", worktreePath)
	out, err = unlock.CombinedOutput()
	require.NoError(t, err, string(out))

	removed, err := RemoveWorktreeDir(repoRoot, worktreePath)
	require.NoError(t, err, "the documented recovery must actually finish the removal")
	assert.True(t, removed)

	_, statErr := os.Stat(worktreePath)
	assert.True(t, os.IsNotExist(statErr), "worktree directory should be gone after unlock + retry")
	_, statErr = os.Stat(worktreeMetadataDir(repoRoot, worktreePath))
	assert.True(t, os.IsNotExist(statErr), "worktree metadata should be pruned after unlock + retry")

	deleted, err := DeleteLocalBranch(repoRoot, branch)
	require.NoError(t, err, "branch deletion must no longer be blocked")
	assert.True(t, deleted)
}

// TestRemoveWorktreeDir_UnlockedWorktreeStillRemovesCleanly is the happy-path
// regression guard: the verification added for #2110 must not turn a normal
// removal into a failure.
func TestRemoveWorktreeDir_UnlockedWorktreeStillRemovesCleanly(t *testing.T) {
	repoRoot, worktreePath, branch := resetRepoWithWorktree(t, "normal")

	removed, err := RemoveWorktreeDir(repoRoot, worktreePath)
	require.NoError(t, err)
	assert.True(t, removed, "an existing worktree directory was removed")

	_, statErr := os.Stat(worktreePath)
	assert.True(t, os.IsNotExist(statErr), "worktree directory should be gone")
	_, statErr = os.Stat(worktreeMetadataDir(repoRoot, worktreePath))
	assert.True(t, os.IsNotExist(statErr), "worktree metadata should be pruned")

	deleted, err := DeleteLocalBranch(repoRoot, branch)
	require.NoError(t, err)
	assert.True(t, deleted, "branch deletion must succeed after a clean worktree removal")
}

// TestRemoveWorktreeDir_MissingDirWithLockedMetadata covers the half-cleaned
// state an older AF version could leave behind (directory deleted, locked
// metadata retained). The verification must catch it there too — the missing
// directory is not evidence that the registration is gone.
func TestRemoveWorktreeDir_MissingDirWithLockedMetadata(t *testing.T) {
	repoRoot, worktreePath, _ := resetRepoWithWorktree(t, "halfcleaned")

	lock := exec.Command("git", "-C", repoRoot, "worktree", "lock", worktreePath, "--reason", "still in use")
	out, err := lock.CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.RemoveAll(worktreePath))

	_, err = RemoveWorktreeDir(repoRoot, worktreePath)
	require.Error(t, err, "a missing directory must not be read as a completed cleanup")
	require.ErrorIs(t, err, ErrWorktreeStillRegistered)
}

// TestRemoveWorktreeDir_SymlinkedRootWithMissingDir is the darwin-only bug this
// fix originally shipped with, pinned so it cannot come back on any platform.
//
// macOS puts temp/working roots behind a symlink (/var -> /private/var), and git
// reports the CANONICAL path in `worktree list --porcelain` while AF holds the
// spelling the session was created with. The registration probe compared the two
// raw strings. That works while the checkout exists — EvalSymlinks canonicalizes
// both sides — but every post-removal probe runs with the leaf DELETED, where
// plain EvalSymlinks fails on both sides and neither gets canonicalized. The
// probe then reported a still-locked worktree as "not registered": a fabricated
// negative, the exact failure mode the verification above exists to catch.
//
// The symlinked root is built INSIDE the test rather than taken from TMPDIR, so
// this reproduces on Linux CI too instead of only on the macOS runner (#2110).
func TestRemoveWorktreeDir_SymlinkedRootWithMissingDir(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(realRoot, 0o755))
	linkRoot := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(realRoot, linkRoot))

	// Everything below is addressed through the SYMLINK, exactly as an AF record
	// created under macOS /var would be; git canonicalizes to realRoot internally.
	repoRoot := filepath.Join(linkRoot, "repo")
	require.NoError(t, os.MkdirAll(repoRoot, 0o755))
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repoRoot}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), string(out))
	}
	git("init", "-q", "-b", "master", ".")
	git("commit", "-q", "--allow-empty", "-m", "initial")

	worktreePath := filepath.Join(linkRoot, "wt-symlinked")
	git("worktree", "add", "-q", "-b", "symlinked", worktreePath)
	git("worktree", "lock", worktreePath, "--reason", "still in use")

	// Sanity: git really does report a DIFFERENT spelling than the one AF holds.
	listed, err := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain").Output()
	require.NoError(t, err)
	require.NotContains(t, string(listed), linkRoot,
		"sanity: git should report the canonical (non-symlinked) path")

	// The half-cleaned state: directory gone, locked metadata retained. Neither
	// side of the comparison can be resolved by a plain EvalSymlinks now.
	require.NoError(t, os.RemoveAll(worktreePath))

	_, err = RemoveWorktreeDir(repoRoot, worktreePath)
	require.Error(t, err,
		"a still-registered worktree must be detected even when the record's path is spelled through a symlink")
	require.ErrorIs(t, err, ErrWorktreeStillRegistered)
}

// TestRemoveWorktreeDir_DeletedRepoStillRemovesOrphanDir guards the other side
// of the #2110 verification: a repo the user has deleted registers nothing, so
// the probe that cannot run must NOT be read as "still registered" and leak AF's
// orphaned worktree directory. `af reset` iterates records whose repo may be
// gone, moved, or unmounted.
func TestRemoveWorktreeDir_DeletedRepoStillRemovesOrphanDir(t *testing.T) {
	repoRoot, worktreePath, _ := resetRepoWithWorktree(t, "orphan")

	require.NoError(t, os.RemoveAll(repoRoot))

	removed, err := RemoveWorktreeDir(repoRoot, worktreePath)
	require.NoError(t, err, "a deleted repo is not a failed cleanup")
	assert.True(t, removed)

	_, statErr := os.Stat(worktreePath)
	assert.True(t, os.IsNotExist(statErr), "the orphaned worktree directory should be removed")
}

// TestRemoveWorktreeDir_DeGittedRepoStillRemovesOrphanDir pins the de-git'd repo
// edge: the directory survives but its .git is gone, so it registers nothing.
//
// The conservative reading ("the probe failed, so maybe it is still registered")
// would retain the record and tell the user to unlock a worktree that has no repo
// to be registered with — advice no re-run could ever satisfy. Since "not a git
// repo" is directly observable rather than inferred from a git error, reset
// settles it instead of leaving an unclearable record behind (#2110).
func TestRemoveWorktreeDir_DeGittedRepoStillRemovesOrphanDir(t *testing.T) {
	repoRoot, worktreePath, _ := resetRepoWithWorktree(t, "degitted")

	// The user blew away .git but kept the working tree.
	require.NoError(t, os.RemoveAll(filepath.Join(repoRoot, ".git")))

	removed, err := RemoveWorktreeDir(repoRoot, worktreePath)
	require.NoError(t, err, "a repo that registers nothing is not a failed cleanup")
	assert.True(t, removed)

	_, statErr := os.Stat(worktreePath)
	assert.True(t, os.IsNotExist(statErr), "the orphaned worktree directory should be removed")
}

// TestRemoveWorktreeDir_MissingDirAndMetadataIsCleanNoOp keeps the idempotence
// contract: a second `af reset` over an already-removed worktree is silent
// success, not a new error.
func TestRemoveWorktreeDir_MissingDirAndMetadataIsCleanNoOp(t *testing.T) {
	repoRoot, worktreePath, _ := resetRepoWithWorktree(t, "gone")

	removed, err := RemoveWorktreeDir(repoRoot, worktreePath)
	require.NoError(t, err)
	require.True(t, removed)

	removed, err = RemoveWorktreeDir(repoRoot, worktreePath)
	require.NoError(t, err, "a second removal must be a clean no-op")
	assert.False(t, removed)
}

// corruptWorktreePointer overwrites a linked worktree's .git file with garbage so
// `git worktree remove -f` fails with "validation failed" — the #726 corrupted-
// pointer case the shared ownership gate is meant to let AF clean up. Its
// registration in the main repo's .git/worktrees survives, so git still LISTS it.
func corruptWorktreePointer(t *testing.T, worktreePath string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, ".git"), []byte("garbage: not a gitdir pointer\n"), 0o644))
}

// TestRemoveWorktreeDir_CorruptedPointerIsCleanedNotDeadEnded is the #2531-review
// P1-b regression: reset ran `git worktree remove` via .Run(), discarding the
// stderr the shared ownership gate classifies on, so the #726 corrupted-pointer
// allowance NEVER fired for reset. The corrupted worktree then dead-ended on the
// unactionable "unlock a locked worktree" advice (it is not locked) with no way to
// clear it — the #2110 class through a different door. With stderr captured, reset
// cleans it up like Cleanup does.
func TestRemoveWorktreeDir_CorruptedPointerIsCleanedNotDeadEnded(t *testing.T) {
	repoRoot, worktreePath, _ := resetRepoWithWorktree(t, "corrupt")
	corruptWorktreePointer(t, worktreePath)

	removed, err := RemoveWorktreeDir(repoRoot, worktreePath)
	require.NoError(t, err,
		"a corrupted-pointer worktree must be cleaned up, not dead-ended on unlock advice (#2531 review)")
	assert.True(t, removed)
	_, statErr := os.Stat(worktreePath)
	assert.True(t, os.IsNotExist(statErr), "the corrupted worktree directory must be removed")
}

// TestRemoveWorktreeDir_CorruptedPointerRemovalFailureRetainsRecord is the #2531
// end-to-end lock — the scenario the direct-helper unit test could not prove
// reachable. A corrupted pointer routes to the os.RemoveAll fallback (reachable only
// once stderr is captured); a read-only subdir makes that os.RemoveAll fail EACCES,
// so the removal cannot complete and reset must RETAIN the record for a re-run.
func TestRemoveWorktreeDir_CorruptedPointerRemovalFailureRetainsRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based EACCES is bypassed by root; this scenario needs a non-root uid")
	}
	repoRoot, worktreePath, _ := resetRepoWithWorktree(t, "corrupt-blocked")

	// A file inside a read-only subdir: os.RemoveAll cannot unlink the child, so the
	// whole removal fails EACCES.
	blocked := filepath.Join(worktreePath, "blocked")
	require.NoError(t, os.MkdirAll(blocked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(blocked, "keep.txt"), []byte("x"), 0o644))
	corruptWorktreePointer(t, worktreePath)
	require.NoError(t, os.Chmod(blocked, 0o555))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) }) // let t.TempDir teardown succeed

	removed, err := RemoveWorktreeDir(repoRoot, worktreePath)
	require.Error(t, err, "a removal that could not complete must not be reported as done")
	assert.False(t, removed)
	require.ErrorIs(t, err, ErrWorktreeStillRegistered,
		"reset must RETAIN the record so a re-run can finish the removal (#2531)")
	// Prove it reached the os.RemoveAll fallback (only reachable after the stderr
	// fix), not the pre-existing early return — the message is about the failed
	// removal, never the wrong locked-worktree "unlock" advice.
	assert.NotContains(t, err.Error(), "unlock",
		"a corrupted pointer must not be reported as a locked worktree")
	_, statErr := os.Stat(worktreePath)
	assert.NoError(t, statErr, "the directory really is still there — the failure is not crying wolf")
}

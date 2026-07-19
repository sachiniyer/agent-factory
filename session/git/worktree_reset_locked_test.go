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

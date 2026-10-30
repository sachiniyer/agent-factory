package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "protocol.file.allow=always"}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func pruneWorktree(t *testing.T) (repo, wt string) {
	t.Helper()
	repo = newPruneGuardRepo(t)
	wt = filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", "-b", "af/p", wt)
	return repo, wt
}

// BLOCKING (#5189, rounds 8 and 15): a file modified under assume-unchanged or
// skip-worktree reads clean to `git status`; the probe must still refuse, name the
// path, and leave the index untouched.
func TestUnrecoverableContentSeesHidingFlags(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			_, wt := pruneWorktree(t)
			gitIn(t, wt, "update-index", flag, "a.txt")
			require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("only copy"), 0o644))

			n, err := WorktreeDirtyFiles(wt)
			require.NoError(t, err)
			require.Zero(t, n, "premise: git status cannot see it")

			before := gitIn(t, wt, "ls-files", "-v")
			reasons, err := WorktreeUnrecoverableContent(wt)
			require.NoError(t, err)
			require.NotEmpty(t, reasons)
			assert.Contains(t, strings.Join(reasons, " "), "a.txt", "the refusal names the path")
			assert.Equal(t, before, gitIn(t, wt, "ls-files", "-v"), "the index flags are untouched")
		})
	}
	t.Run("clean tree", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		reasons, err := WorktreeUnrecoverableContent(wt)
		require.NoError(t, err)
		assert.Empty(t, reasons)
	})
}

// BLOCKING (round 9): an ignored file inside an initialized submodule is invisible
// to the superproject's status.
func TestUnrecoverableContentSeesIgnoredFilesInSubmodules(t *testing.T) {
	sub := newPruneGuardRepo(t)
	repo, wt := pruneWorktree(t)
	gitIn(t, wt, "submodule", "add", sub, "mod")
	gitIn(t, wt, "commit", "-qm", "add submodule")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "mod", ".gitignore"), []byte("secret.env\n"), 0o644))
	gitIn(t, filepath.Join(wt, "mod"), "add", ".gitignore")
	gitIn(t, filepath.Join(wt, "mod"), "commit", "-qm", "ignore")
	gitIn(t, wt, "add", "mod")
	gitIn(t, wt, "commit", "-qm", "bump")
	_ = repo

	reasons, err := WorktreeUnrecoverableContent(wt)
	require.NoError(t, err)
	require.Empty(t, reasons, "premise: a clean submodule is reclaimable")

	require.NoError(t, os.WriteFile(filepath.Join(wt, "mod", "secret.env"), []byte("only copy"), 0o644))
	n, err := WorktreeDirtyFiles(wt)
	require.NoError(t, err)
	require.Zero(t, n, "premise: the superproject's status does not see an ignored file in the submodule")

	reasons, err = WorktreeUnrecoverableContent(wt)
	require.NoError(t, err)
	require.NotEmpty(t, reasons)
	assert.Contains(t, strings.Join(reasons, " "), "submodule mod")
	assert.Contains(t, strings.Join(reasons, " "), "secret.env")
}

// BLOCKING (round 9): the occupant is bound by an af-owned nonce. A worktree
// re-created at the same path with the same branch has the same repo, path and
// branch, and — on a coarse-timestamp filesystem — the same registration mtime; it
// must not verify.
func TestArchiveOccupantNonceRejectsARecreatedWorktree(t *testing.T) {
	repo, wt := pruneWorktree(t)
	nonce, err := StampArchiveNonce(wt)
	require.NoError(t, err)
	archivedAt := time.Now()
	require.NoError(t, VerifyArchiveOccupant(wt, nonce, archivedAt), "the original verifies")

	// Re-create: same path, same branch.
	gitIn(t, repo, "worktree", "remove", "--force", wt)
	gitIn(t, repo, "worktree", "add", wt, "af/p")
	// Coarse timestamps: the new leaf shares the original's mtime.
	leaf, err := registrationLeaf(wt)
	require.NoError(t, err)
	old := archivedAt.Add(-time.Hour)
	require.NoError(t, os.Chtimes(leaf, old, old))

	require.Error(t, VerifyArchiveOccupant(wt, nonce, archivedAt), "the nonce the original carried is gone")

	// The legacy (nonce-less) rule: a leaf inside one timestamp bucket of the
	// archive time does not verify; one well before it does.
	require.NoError(t, os.Chtimes(leaf, archivedAt, archivedAt))
	require.Error(t, VerifyArchiveOccupant(wt, "", archivedAt), "same-bucket registration must not verify")
	require.NoError(t, os.Chtimes(leaf, old, old))
	require.NoError(t, VerifyArchiveOccupant(wt, "", archivedAt))
}

func TestWorktreeHeadMatchesBranch(t *testing.T) {
	repo, wt := pruneWorktree(t)
	head, err := WorktreeHeadMatchesBranch(wt, repo, "af/p")
	require.NoError(t, err)
	assert.NotEmpty(t, head)

	gitIn(t, wt, "checkout", "--detach")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "b.txt"), []byte("y"), 0o644))
	gitIn(t, wt, "add", "b.txt")
	gitIn(t, wt, "commit", "-qm", "detached work")
	_, err = WorktreeHeadMatchesBranch(wt, repo, "af/p")
	require.Error(t, err, "a commit only the detached HEAD references is not on the kept branch")
}

// Only the archived worktree's own leaf is removed; another session's stale
// registration (which `git worktree prune` would drop) stays.
func TestRemoveArchivedRegistrationLeafIsScoped(t *testing.T) {
	repo, wt := pruneWorktree(t)
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, repo, "worktree", "add", "-b", "af/other", other)
	require.NoError(t, os.RemoveAll(other)) // now stale, as an unmounted worktree would be

	moved := wt + "-moved"
	require.NoError(t, os.Rename(wt, moved))
	require.NoError(t, RemoveArchivedRegistrationLeaf(repo, wt))
	list := gitIn(t, repo, "worktree", "list", "--porcelain")
	assert.NotContains(t, list, wt)
	assert.Contains(t, list, other, "another session's registration is not ours to drop")
	require.NoError(t, RemoveArchivedRegistrationLeaf(repo, wt), "a second removal is a no-op")

	locked := filepath.Join(t.TempDir(), "locked")
	gitIn(t, repo, "worktree", "add", "-b", "af/locked", locked)
	gitIn(t, repo, "worktree", "lock", locked)
	require.NoError(t, os.Rename(locked, locked+"-moved"))
	require.Error(t, RemoveArchivedRegistrationLeaf(repo, locked), "a locked registration is refused")
}

// A bare origin keeps refs at its root: the branch probe must find a live branch
// there instead of reporting it absent (round 13).
func TestLocalBranchExistsInABareOrigin(t *testing.T) {
	repo := newPruneGuardRepo(t)
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, repo, "clone", "--bare", repo, bare)
	gitIn(t, bare, "branch", "af/kept", "main")
	ok, err := LocalBranchExists(bare, "af/kept")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = LocalBranchExists(bare, "af/missing")
	require.NoError(t, err)
	assert.False(t, ok)
}

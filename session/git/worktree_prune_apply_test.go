package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
// to the superproject's status, so each submodule is probed itself.
func TestUnrecoverableContentSeesIgnoredFilesInSubmodules(t *testing.T) {
	sub := newPruneGuardRepo(t)
	_, wt := pruneWorktree(t)
	gitIn(t, wt, "submodule", "add", sub, "mod")
	gitIn(t, wt, "commit", "-qm", "add submodule")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "mod", "secret.env"), []byte("only copy"), 0o644))

	reasons, err := WorktreeUnrecoverableContent(wt)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(reasons, " "), "submodule mod")
	assert.Contains(t, strings.Join(reasons, " "), "secret.env")
}

// P1 (Codex on #6186): in a linked worktree a submodule's object database lives
// under the registration leaf (.git/worktrees/<id>/modules/…). The kept branch
// holds only the gitlink hash, so removing the leaf would destroy submodule
// commits that exist nowhere else. A clean worktree with such a submodule must be
// refused, naming the submodule.
func TestUnrecoverableContentRefusesASubmoduleWhoseObjectsLiveInTheRegistration(t *testing.T) {
	sub := newPruneGuardRepo(t)
	_, wt := pruneWorktree(t)
	gitIn(t, wt, "submodule", "add", sub, "mod")
	// A submodule commit that exists nowhere else.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "mod", "local-only.txt"), []byte("only here"), 0o644))
	gitIn(t, filepath.Join(wt, "mod"), "add", "local-only.txt")
	gitIn(t, filepath.Join(wt, "mod"), "commit", "-qm", "submodule-only commit")
	gitIn(t, wt, "add", "mod")
	gitIn(t, wt, "commit", "-qm", "bump submodule")

	n, err := WorktreeDirtyFiles(wt)
	require.NoError(t, err)
	require.Zero(t, n, "premise: the worktree reads clean")
	gitDir := gitIn(t, filepath.Join(wt, "mod"), "rev-parse", "--absolute-git-dir")
	leaf, err := registrationLeaf(wt)
	require.NoError(t, err)
	require.True(t, within(gitDir, leaf), "premise: the submodule's objects live under the registration leaf: %s", gitDir)

	reasons, err := WorktreeUnrecoverableContent(wt)
	require.NoError(t, err)
	require.NotEmpty(t, reasons)
	assert.Contains(t, strings.Join(reasons, " "), "submodule mod")
	assert.Contains(t, strings.Join(reasons, " "), "pruning deletes")
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

// Only the recorded leaf is removed, and only while it still carries the recorded
// nonce: another session's stale registration (which `git worktree prune` would
// drop) stays, and a replacement that now occupies the same path is left alone.
func TestRemoveArchivedRegistrationLeafIsBoundToTheRecordedOccupant(t *testing.T) {
	repo, wt := pruneWorktree(t)
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, repo, "worktree", "add", "-b", "af/other", other)
	require.NoError(t, os.RemoveAll(other)) // now stale, as an unmounted worktree would be

	leafName, nonce, err := StampPruneNonce(wt)
	require.NoError(t, err)
	moved := wt + "-moved"
	require.NoError(t, os.Rename(wt, moved))
	require.NoError(t, RemoveArchivedRegistrationLeaf(repo, leafName, nonce))
	list := gitIn(t, repo, "worktree", "list", "--porcelain")
	assert.NotContains(t, list, wt)
	assert.Contains(t, list, other, "another session's registration is not ours to drop")
	require.NoError(t, RemoveArchivedRegistrationLeaf(repo, leafName, nonce), "a leaf already gone is done")

	// A replacement now occupies the original path and the same leaf name: it is
	// not the recorded occupant, so it is left alone.
	gitIn(t, repo, "worktree", "add", wt, "af/p")
	err = RemoveArchivedRegistrationLeaf(repo, leafName, nonce)
	require.ErrorIs(t, err, ErrRegistrationNotOurs)
	assert.Contains(t, gitIn(t, repo, "worktree", "list", "--porcelain"), wt, "the replacement stays registered")

	locked := filepath.Join(t.TempDir(), "locked")
	gitIn(t, repo, "worktree", "add", "-b", "af/locked", locked)
	lname, lnonce, err := StampPruneNonce(locked)
	require.NoError(t, err)
	gitIn(t, repo, "worktree", "lock", locked)
	require.NoError(t, os.Rename(locked, locked+"-moved"))
	require.Error(t, RemoveArchivedRegistrationLeaf(repo, lname, lnonce), "a locked registration is refused")
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

// One predicate, every shape (Codex P1 round 2 on #6186): a submodule's real git
// directory must lie outside BOTH roots prune deletes — the worktree tree and the
// registration leaf — or the worktree is refused.
func TestSubmoduleObjectsMustLiveOutsideEveryDeletedRoot(t *testing.T) {
	commitSub := func(t *testing.T, wt string) {
		require.NoError(t, os.WriteFile(filepath.Join(wt, "mod", "only-here.txt"), []byte("x"), 0o644))
		gitIn(t, filepath.Join(wt, "mod"), "add", "only-here.txt")
		gitIn(t, filepath.Join(wt, "mod"), "commit", "-qm", "submodule-only commit")
		gitIn(t, wt, "add", "mod")
		gitIn(t, wt, "commit", "-qm", "bump")
	}
	refused := func(t *testing.T, wt string, want ...string) {
		t.Helper()
		reasons, err := WorktreeUnrecoverableContent(wt)
		require.NoError(t, err)
		joined := strings.Join(reasons, " ")
		require.NotEmpty(t, reasons)
		for _, w := range want {
			assert.Contains(t, joined, w)
		}
	}

	t.Run("absorbed under the registration leaf", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		gitIn(t, wt, "submodule", "add", newPruneGuardRepo(t), "mod")
		commitSub(t, wt)
		refused(t, wt, "submodule mod")
	})

	t.Run("not absorbed: .git directory inside the tree", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		src := newPruneGuardRepo(t)
		gitIn(t, wt, "clone", src, "mod") // a real .git directory under the worktree
		gitIn(t, wt, "submodule", "add", src, "mod")
		fi, err := os.Stat(filepath.Join(wt, "mod", ".git"))
		require.NoError(t, err)
		require.True(t, fi.IsDir(), "premise: the submodule's .git is a directory inside the tree")
		commitSub(t, wt)
		refused(t, wt, "submodule mod")
	})

	t.Run("nested submodule", func(t *testing.T) {
		inner := newPruneGuardRepo(t)
		outer := newPruneGuardRepo(t)
		gitIn(t, outer, "submodule", "add", inner, "inner")
		gitIn(t, outer, "commit", "-qm", "add inner")
		_, wt := pruneWorktree(t)
		gitIn(t, wt, "submodule", "add", outer, "mod")
		gitIn(t, wt, "submodule", "update", "--init", "--recursive")
		gitIn(t, wt, "commit", "-qm", "add outer")
		refused(t, wt, "submodule mod", "mod/inner")
	})

	t.Run("outside both roots is allowed", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		gitIn(t, wt, "submodule", "add", newPruneGuardRepo(t), "mod")
		gitIn(t, wt, "commit", "-qm", "add submodule")
		// Relocate the submodule's git directory outside the tree and the leaf.
		old := gitIn(t, filepath.Join(wt, "mod"), "rev-parse", "--absolute-git-dir")
		outside := filepath.Join(t.TempDir(), "mod.git")
		require.NoError(t, os.Rename(old, outside))
		require.NoError(t, os.WriteFile(filepath.Join(wt, "mod", ".git"), []byte("gitdir: "+outside+"\n"), 0o644))
		gitIn(t, wt, "config", "--file", filepath.Join(outside, "config"), "core.worktree", filepath.Join(wt, "mod"))

		reasons, err := WorktreeUnrecoverableContent(wt)
		require.NoError(t, err)
		assert.Empty(t, reasons, "a submodule whose objects survive the deletion is reclaimable")
	})

	t.Run("an unresolvable git directory refuses", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		gitIn(t, wt, "submodule", "add", newPruneGuardRepo(t), "mod")
		gitIn(t, wt, "commit", "-qm", "add submodule")
		require.NoError(t, os.WriteFile(filepath.Join(wt, "mod", ".git"), []byte("gitdir: /nonexistent/elsewhere\n"), 0o644))
		_, err := WorktreeUnrecoverableContent(wt)
		require.Error(t, err, "unknown is never reclaimable")
	})
}

// Stamping the prune nonce must not alter the evidence a legacy (nonce-less)
// archive is judged by: the leaf's mtime. A resume that abandons and re-judges
// would otherwise find a freshly "re-created" leaf (Codex on #6186).
func TestStampingPruneNonceKeepsTheLegacyOccupantProof(t *testing.T) {
	_, wt := pruneWorktree(t)
	leaf, err := registrationLeaf(wt)
	require.NoError(t, err)
	archivedAt := time.Now()
	old := archivedAt.Add(-time.Hour)
	require.NoError(t, os.Chtimes(leaf, old, old))
	require.NoError(t, VerifyArchiveOccupant(wt, "", archivedAt), "premise: the legacy proof passes")

	_, _, err = StampPruneNonce(wt)
	require.NoError(t, err)
	st, err := os.Stat(leaf)
	require.NoError(t, err)
	assert.WithinDuration(t, old, st.ModTime(), time.Second, "the leaf's mtime is unchanged by the stamp")
	require.NoError(t, VerifyArchiveOccupant(wt, "", archivedAt), "and so is the proof")
}

// Concurrent verifies share the flight's RESULT, not just its error: every caller
// gets the resolved leaf (Codex on #6186).
func TestRegistrationLeafFlightSharesTheResolvedPath(t *testing.T) {
	_, wt := pruneWorktree(t)
	want, err := registrationLeaf(wt)
	require.NoError(t, err)
	require.NotEmpty(t, want)

	var wg sync.WaitGroup
	results := make([]string, 16)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := registrationLeaf(wt)
			if err == nil {
				results[i] = got
			}
		}()
	}
	wg.Wait()
	for _, got := range results {
		assert.Equal(t, want, got)
	}
}

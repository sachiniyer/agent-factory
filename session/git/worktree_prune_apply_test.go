package git

import (
	"fmt"
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
	wt = filepath.Join(tempRoot(t), "wt")
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
	other := filepath.Join(tempRoot(t), "other")
	gitIn(t, repo, "worktree", "add", "-b", "af/other", other)
	require.NoError(t, os.RemoveAll(other)) // now stale, as an unmounted worktree would be

	leafName, nonce, err := StampPruneNonce(wt)
	require.NoError(t, err)
	moved := wt + "-moved"
	require.NoError(t, os.Rename(wt, moved))
	require.NoError(t, RemoveArchivedRegistrationLeaf(repo, leafName, nonce))
	list := gitIn(t, repo, "worktree", "list", "--porcelain")
	assert.NotContains(t, list, canonPath(t, wt))
	assert.Contains(t, list, canonPath(t, other), "another session's registration is not ours to drop")
	require.NoError(t, RemoveArchivedRegistrationLeaf(repo, leafName, nonce), "a leaf already gone is done")

	// A replacement now occupies the original path and the same leaf name: it is
	// not the recorded occupant, so it is left alone.
	gitIn(t, repo, "worktree", "add", wt, "af/p")
	err = RemoveArchivedRegistrationLeaf(repo, leafName, nonce)
	require.ErrorIs(t, err, ErrRegistrationNotOurs)
	assert.Contains(t, gitIn(t, repo, "worktree", "list", "--porcelain"), canonPath(t, wt), "the replacement stays registered")

	locked := filepath.Join(tempRoot(t), "locked")
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
	bare := filepath.Join(tempRoot(t), "origin.git")
	gitIn(t, repo, "clone", "--bare", repo, bare)
	gitIn(t, bare, "branch", "af/kept", "main")
	ok, err := LocalBranchExists(bare, "af/kept")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = LocalBranchExists(bare, "af/missing")
	require.NoError(t, err)
	assert.False(t, ok)
}

// Prune never deletes a worktree whose index contains a gitlink (#5189): the kept
// branch holds only the submodule's commit hash, and its objects and local files
// can sit in the tree, in the registration, or where status never looks. Every
// shape is refused with the submodule path named; nothing is probed for "safety".
func TestWorktreeWithAnySubmoduleIsRefused(t *testing.T) {
	refused := func(t *testing.T, wt string, want ...string) {
		t.Helper()
		reasons, err := WorktreeUnrecoverableContent(wt)
		require.NoError(t, err)
		require.NotEmpty(t, reasons)
		joined := strings.Join(reasons, " ")
		assert.Contains(t, joined, "submodule")
		for _, w := range want {
			assert.Contains(t, joined, w)
		}
	}

	t.Run("uninitialized submodule directory holding a local file", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		gitIn(t, wt, "submodule", "add", newPruneGuardRepo(t), "mod")
		gitIn(t, wt, "commit", "-qm", "add submodule")
		gitIn(t, wt, "submodule", "deinit", "-f", "mod")
		require.NoError(t, os.MkdirAll(filepath.Join(wt, "mod"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(wt, "mod", "local-only.txt"), []byte("only copy"), 0o644))
		n, err := WorktreeDirtyFiles(wt)
		require.NoError(t, err)
		require.Zero(t, n, "premise: status never reports a file in an uninitialized submodule path")
		refused(t, wt, "mod")
	})

	t.Run("initialized, absorbed", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		gitIn(t, wt, "submodule", "add", newPruneGuardRepo(t), "mod")
		gitIn(t, wt, "commit", "-qm", "add submodule")
		refused(t, wt, "mod")
	})

	t.Run("initialized, not absorbed (.git directory inside the tree)", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		src := newPruneGuardRepo(t)
		gitIn(t, wt, "clone", src, "mod")
		gitIn(t, wt, "submodule", "add", src, "mod")
		gitIn(t, wt, "commit", "-qm", "add submodule")
		refused(t, wt, "mod")
	})

	t.Run("nested", func(t *testing.T) {
		inner := newPruneGuardRepo(t)
		outer := newPruneGuardRepo(t)
		gitIn(t, outer, "submodule", "add", inner, "inner")
		gitIn(t, outer, "commit", "-qm", "add inner")
		_, wt := pruneWorktree(t)
		gitIn(t, wt, "submodule", "add", outer, "mod")
		gitIn(t, wt, "submodule", "update", "--init", "--recursive")
		gitIn(t, wt, "commit", "-qm", "add outer")
		refused(t, wt, "mod")
	})

	t.Run("a worktree without submodules is not refused for them", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		reasons, err := WorktreeUnrecoverableContent(wt)
		require.NoError(t, err)
		assert.Empty(t, reasons)
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

// The predicate is about what prune DELETES, not how the storage got there: neither
// the worktree tree nor its registration leaf may hold git object storage (Codex
// round 5 on #6186).
func TestGitObjectStorageUnderTheDeletedRoots(t *testing.T) {
	t.Run("a clean linked worktree passes", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		found, err := gitObjectStorageUnder(wt)
		require.NoError(t, err)
		assert.Empty(t, found, "its objects live in the main repository, not under either root")
	})

	t.Run("a submodule database retained under the leaf after the gitlink was removed", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		gitIn(t, wt, "submodule", "add", newPruneGuardRepo(t), "mod")
		gitIn(t, wt, "commit", "-qm", "add submodule")
		gitIn(t, wt, "rm", "-f", "mod")
		gitIn(t, wt, "commit", "-qm", "remove submodule")
		leaf, err := registrationLeaf(wt)
		require.NoError(t, err)
		require.DirExists(t, filepath.Join(leaf, "modules", "mod", "objects"), "premise: git keeps the database after removal")
		subs, err := gitlinkPaths(wt)
		require.NoError(t, err)
		require.Empty(t, subs, "premise: no gitlink remains for the gitlink rule to see")

		found, err := gitObjectStorageUnder(wt)
		require.NoError(t, err)
		require.NotEmpty(t, found)
		assert.Contains(t, strings.Join(found, " "), filepath.Join("modules", "mod"))
		reasons, err := WorktreeUnrecoverableContent(wt)
		require.NoError(t, err)
		assert.Contains(t, strings.Join(reasons, " "), "git object storage")
	})

	t.Run("a nested independent repository inside the tree", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		nested := filepath.Join(wt, "vendor", "lib")
		require.NoError(t, os.MkdirAll(nested, 0o755))
		gitIn(t, nested, "init", "-q")
		found, err := gitObjectStorageUnder(wt)
		require.NoError(t, err)
		require.NotEmpty(t, found)
		assert.Contains(t, strings.Join(found, " "), filepath.Join("vendor", "lib", ".git"))
	})

	t.Run("a gitfile elsewhere in the tree pointing inside a deleted root", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		leaf, err := registrationLeaf(wt)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(wt, "x"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(wt, "x", ".git"), []byte("gitdir: "+leaf+"\n"), 0o644))
		found, err := gitObjectStorageUnder(wt)
		require.NoError(t, err)
		assert.NotEmpty(t, found)
	})

	t.Run("a directory merely named objects is not storage", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		require.NoError(t, os.MkdirAll(filepath.Join(wt, "src", "objects"), 0o755))
		found, err := gitObjectStorageUnder(wt)
		require.NoError(t, err)
		assert.Empty(t, found)
	})
}

// A locked registration is refused by the verdict, before any mutation (a later
// removal would refuse it after the rename and strand a half-done prune).
func TestLockedRegistrationIsRefusedByTheVerdict(t *testing.T) {
	repo, wt := pruneWorktree(t)
	gitIn(t, repo, "worktree", "lock", wt)
	reasons, err := WorktreeUnrecoverableContent(wt)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(reasons, " "), "locked")
}

// The object-storage scan is bounded in entries and in time; exceeding either is
// unknown, which refuses (it is an error, never an empty "nothing found").
func TestObjectStorageScanIsBounded(t *testing.T) {
	_, wt := pruneWorktree(t)
	for i := 0; i < 10; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(wt, "f"+strings.Repeat("x", i)+".txt"), []byte("x"), 0o644))
	}
	t.Run("entries", func(t *testing.T) {
		prev := objectStorageScanMaxEntries
		objectStorageScanMaxEntries = 3
		t.Cleanup(func() { objectStorageScanMaxEntries = prev })
		_, err := gitObjectStorageUnder(wt)
		require.Error(t, err)
		_, err = WorktreeUnrecoverableContent(wt)
		require.Error(t, err, "an unbounded tree is unknown and refuses")
	})
	t.Run("time", func(t *testing.T) {
		prevT, prevH := objectStorageScanTimeout, objectStorageScanHook
		objectStorageScanTimeout = 50 * time.Millisecond
		objectStorageScanHook = func() { time.Sleep(300 * time.Millisecond) } // a stalled directory read
		t.Cleanup(func() { objectStorageScanTimeout, objectStorageScanHook = prevT, prevH })
		start := time.Now()
		_, err := gitObjectStorageUnder(wt)
		require.Error(t, err)
		assert.Less(t, time.Since(start), 250*time.Millisecond, "the caller does not wait on the parked read")
	})
}

// The evidence that proves a registration is ours is removed LAST, so a removal
// that fails partway leaves a leaf a resume can still prove and finish (Codex on
// #6186): a plain RemoveAll can delete the nonce early and strand the row.
func TestLeafRemovalFailingMidwayCanBeResumedToCompletion(t *testing.T) {
	repo, wt := pruneWorktree(t)
	leafName, nonce, err := StampPruneNonce(wt)
	require.NoError(t, err)
	leaf, err := registrationLeaf(wt)
	require.NoError(t, err)
	require.NoError(t, os.Rename(wt, wt+"-moved"))

	// Fail while removing the LAST ordinary entry, so every other entry — whatever
	// order they sort in relative to the nonce — has already been removed.
	all, err := os.ReadDir(leaf)
	require.NoError(t, err)
	last := ""
	for _, e := range all {
		if e.Name() != pruneNonceFile {
			last = e.Name()
		}
	}
	require.NotEmpty(t, last)
	prev := leafRemovalFailure
	leafRemovalFailure = func(entry string) error {
		if entry == last {
			return fmt.Errorf("injected failure removing %s", entry)
		}
		return nil
	}
	t.Cleanup(func() { leafRemovalFailure = prev })

	err = RemoveArchivedRegistrationLeaf(repo, leafName, nonce)
	require.Error(t, err)
	assert.FileExists(t, filepath.Join(leaf, pruneNonceFile), "the identity evidence is still there after a partial removal")
	left, _ := os.ReadDir(leaf)
	assert.NotEmpty(t, left)

	// Resume: the leaf is provable again and the removal completes.
	leafRemovalFailure = prev
	require.NoError(t, RemoveArchivedRegistrationLeaf(repo, leafName, nonce))
	assert.NoDirExists(t, leaf)
}

// P1 (Codex on #6186): an object store referenced through the origin's
// objects/info/alternates can live inside the registration leaf under ANY name, and
// pruning would destroy the kept branch's objects. The rule is a dependency: every
// store the repository depends on, through the whole alternates chain, lies outside
// both deleted roots.
func TestObjectStoresInsideDeletedRootsAreRefusedWhateverTheyAreNamed(t *testing.T) {
	// moveOriginObjects moves the origin's objects into dest and leaves the origin
	// pointing at them through alternates (git keeps working).
	moveOriginObjects := func(t *testing.T, repo, dest, entry string) {
		t.Helper()
		objects := filepath.Join(repo, ".git", "objects")
		require.NoError(t, os.Rename(objects, dest))
		require.NoError(t, os.MkdirAll(filepath.Join(objects, "info"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(objects, "info", "alternates"), []byte(entry+"\n"), 0o644))
	}

	t.Run("a clean worktree depends only on stores outside", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		deps, err := objectStoresInsideDeletedRoots(wt)
		require.NoError(t, err)
		assert.Empty(t, deps)
	})

	t.Run("origin objects moved into the leaf, absolute alternates entry", func(t *testing.T) {
		repo, wt := pruneWorktree(t)
		leaf, err := registrationLeaf(wt)
		require.NoError(t, err)
		store := filepath.Join(leaf, "pack-store") // not named objects, no HEAD beside it
		moveOriginObjects(t, repo, store, store)
		gitIn(t, wt, "status", "--porcelain") // premise: git still works through the alternate

		deps, err := objectStoresInsideDeletedRoots(wt)
		require.NoError(t, err)
		require.NotEmpty(t, deps)
		reasons, err := WorktreeUnrecoverableContent(wt)
		require.NoError(t, err)
		assert.Contains(t, strings.Join(reasons, " "), "object store")
		assert.Contains(t, strings.Join(reasons, " "), "pack-store")
	})

	t.Run("a relative alternates entry is resolved against the listing store", func(t *testing.T) {
		repo, wt := pruneWorktree(t)
		leaf, err := registrationLeaf(wt)
		require.NoError(t, err)
		store := filepath.Join(leaf, "store")
		// The entry is relative to the objects directory's CANONICAL location (where
		// git resolves it): computing it from the as-written path builds an entry that
		// points nowhere whenever a temp root is reached through a symlink (macOS).
		realObjects, err := filepath.EvalSymlinks(filepath.Join(repo, ".git", "objects"))
		require.NoError(t, err)
		realLeaf, err := filepath.EvalSymlinks(leaf)
		require.NoError(t, err)
		rel, err := filepath.Rel(realObjects, filepath.Join(realLeaf, "store"))
		require.NoError(t, err)
		moveOriginObjects(t, repo, store, rel)
		gitIn(t, wt, "rev-parse", "--verify", "HEAD") // premise: git resolves the relative entry
		require.Contains(t, gitIn(t, wt, "count-objects", "-v"), "alternate:", "premise: git lists it")
		deps, err := objectStoresInsideDeletedRoots(wt)
		require.NoError(t, err)
		assert.NotEmpty(t, deps)
	})

	t.Run("a short relative entry (../worktrees/<leaf>/store)", func(t *testing.T) {
		repo, wt := pruneWorktree(t)
		leaf, err := registrationLeaf(wt)
		require.NoError(t, err)
		store := filepath.Join(leaf, "store")
		moveOriginObjects(t, repo, store, "../worktrees/"+filepath.Base(leaf)+"/store")
		gitIn(t, wt, "rev-parse", "--verify", "HEAD") // premise: git resolves it
		require.Contains(t, gitIn(t, wt, "count-objects", "-v"), "alternate:")
		deps, err := objectStoresInsideDeletedRoots(wt)
		require.NoError(t, err)
		assert.NotEmpty(t, deps)
	})

	t.Run("transitively: an outside store whose alternates list a store inside", func(t *testing.T) {
		repo, wt := pruneWorktree(t)
		leaf, err := registrationLeaf(wt)
		require.NoError(t, err)
		inner := filepath.Join(leaf, "deep")
		outer := filepath.Join(tempRoot(t), "outer-objects")
		moveOriginObjects(t, repo, inner, outer)
		require.NoError(t, os.MkdirAll(filepath.Join(outer, "info"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(outer, "info", "alternates"), []byte(inner+"\n"), 0o644))
		deps, err := objectStoresInsideDeletedRoots(wt)
		require.NoError(t, err)
		assert.NotEmpty(t, deps)
	})

	t.Run("an alternate outside both roots is allowed", func(t *testing.T) {
		repo, wt := pruneWorktree(t)
		outside := filepath.Join(tempRoot(t), "shared-objects")
		moveOriginObjects(t, repo, outside, outside)
		deps, err := objectStoresInsideDeletedRoots(wt)
		require.NoError(t, err)
		assert.Empty(t, deps)
	})

	t.Run("an unreadable alternates file refuses", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("permission bits are bypassed by root")
		}
		repo, wt := pruneWorktree(t)
		moveOriginObjects(t, repo, filepath.Join(tempRoot(t), "o"), filepath.Join(tempRoot(t), "o"))
		alt := filepath.Join(repo, ".git", "objects", "info", "alternates")
		require.NoError(t, os.Chmod(alt, 0o000))
		t.Cleanup(func() { _ = os.Chmod(alt, 0o644) })
		_, err := objectStoresInsideDeletedRoots(wt)
		require.Error(t, err, "unknown refuses")
	})
}

// An EMPTY leaf holds nothing to lose and nothing to prove: a crash between
// removing the nonce and the final rmdir must leave a removal a resume can finish
// (Codex on #6186), while a non-empty leaf that is not provably ours is still left
// alone.
func TestResumeFinishesAnEmptyLeafLeftAfterTheNonceWasRemoved(t *testing.T) {
	repo, wt := pruneWorktree(t)
	leafName, nonce, err := StampPruneNonce(wt)
	require.NoError(t, err)
	leaf, err := registrationLeaf(wt)
	require.NoError(t, err)
	require.NoError(t, os.Rename(wt, wt+"-moved"))

	prev := leafRemovalFailure
	leafRemovalFailure = func(entry string) error {
		if entry == "." { // after the nonce is gone, before the final rmdir
			return fmt.Errorf("injected crash before rmdir")
		}
		return nil
	}
	t.Cleanup(func() { leafRemovalFailure = prev })
	require.Error(t, RemoveArchivedRegistrationLeaf(repo, leafName, nonce))
	entries, err := os.ReadDir(leaf)
	require.NoError(t, err)
	require.Empty(t, entries, "premise: only an empty directory remains, with no identity left to prove")

	leafRemovalFailure = prev
	require.NoError(t, RemoveArchivedRegistrationLeaf(repo, leafName, nonce), "resume removes the empty directory")
	assert.NoDirExists(t, leaf)
}

func TestANonEmptyUnprovenLeafIsStillLeftAlone(t *testing.T) {
	repo, wt := pruneWorktree(t)
	leafName, _, err := StampPruneNonce(wt)
	require.NoError(t, err)
	leaf, err := registrationLeaf(wt)
	require.NoError(t, err)
	err = RemoveArchivedRegistrationLeaf(repo, leafName, "a-different-nonce")
	require.ErrorIs(t, err, ErrRegistrationNotOurs)
	assert.DirExists(t, leaf)
}

// P1 (Codex on #6186): an alternates line may be C-quoted. The dependency chain is
// asked of git itself, so a quoted entry with spaces and escapes that resolves
// inside the leaf is found; and the decoder for git's output is strict.
func TestQuotedAlternateInsideTheLeafIsRefused(t *testing.T) {
	repo, wt := pruneWorktree(t)
	leaf, err := registrationLeaf(wt)
	require.NoError(t, err)
	store := filepath.Join(leaf, "my store\twith tab") // spaces and a TAB in the name
	objects := filepath.Join(repo, ".git", "objects")
	require.NoError(t, os.Rename(objects, store))
	require.NoError(t, os.MkdirAll(filepath.Join(objects, "info"), 0o755))
	// Written the way git reads a quoted entry: escapes, not the raw characters.
	quoted := `"` + leaf + `/my store\twith\040tab"`
	require.NoError(t, os.WriteFile(filepath.Join(objects, "info", "alternates"), []byte(quoted+"\n"), 0o644))
	gitIn(t, wt, "status", "--porcelain") // premise: git resolves the quoted entry and works

	deps, err := objectStoresInsideDeletedRoots(wt)
	require.NoError(t, err)
	require.NotEmpty(t, deps, "the quoted entry resolves inside the leaf")
	reasons, err := WorktreeUnrecoverableContent(wt)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(reasons, " "), "object store")
}

func TestUnquoteGitPathDecodesLikeGit(t *testing.T) {
	good := map[string]string{
		`/plain/path`:    `/plain/path`,
		`"/a b/c"`:       `/a b/c`,
		`"/tab\there"`:   "/tab\there",
		`"/nl\nx"`:       "/nl\nx",
		`"/quote\"x"`:    `/quote"x`,
		`"/back\\slash"`: `/back\slash`,
		`"/sp\040ace"`:   `/sp ace`,
		`"/\303\251"`:    "/\u00e9",
	}
	for in, want := range good {
		got, err := unquoteGitPath(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{`"/unterminated`, `"/bad\q"`, `"/oct\04"`, `"/x"trailing`, `"/dangling\`, `"/oct\477x"`} {
		_, err := unquoteGitPath(bad)
		assert.Error(t, err, bad)
	}
}

// P2 (Codex on #6186): a store reached THROUGH a symlink that lives inside a
// deletion root passes a destination-only check, yet deleting the root deletes the
// link and the repository can no longer reach its store. Every prefix and hop of the
// lookup path must lie outside both roots.
func TestObjectStorePathThroughASymlinkInsideTheLeafIsRefused(t *testing.T) {
	repo, wt := pruneWorktree(t)
	leaf, err := registrationLeaf(wt)
	require.NoError(t, err)

	// The real store lives OUTSIDE both roots ...
	outside := filepath.Join(tempRoot(t), "real-objects")
	objects := filepath.Join(repo, ".git", "objects")
	require.NoError(t, os.Rename(objects, outside))
	// ... but the repository reaches it through a symlink INSIDE the leaf.
	link := filepath.Join(leaf, "objects-link")
	require.NoError(t, os.Symlink(outside, link))
	require.NoError(t, os.MkdirAll(filepath.Join(objects, "info"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(objects, "info", "alternates"), []byte(link+"\n"), 0o644))
	gitIn(t, wt, "status", "--porcelain") // premise: git reads objects through the link

	// The destination alone is outside both roots (the old check passed this).
	real, err := filepath.EvalSymlinks(link)
	require.NoError(t, err)
	realOutside, err := filepath.EvalSymlinks(outside)
	require.NoError(t, err)
	require.Equal(t, realOutside, real)

	deps, err := objectStoresInsideDeletedRoots(wt)
	require.NoError(t, err)
	require.NotEmpty(t, deps, "the path goes through the leaf")
	assert.Contains(t, strings.Join(deps, " "), "objects-link")
	reasons, err := WorktreeUnrecoverableContent(wt)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(reasons, " "), "object store")

	t.Run("a relative entry through a symlink inside the worktree tree", func(t *testing.T) {
		repo2, wt2 := pruneWorktree(t)
		out2 := filepath.Join(tempRoot(t), "real-objects")
		obj2 := filepath.Join(repo2, ".git", "objects")
		require.NoError(t, os.Rename(obj2, out2))
		link2 := filepath.Join(wt2, "vendor-objects")
		require.NoError(t, os.Symlink(out2, link2))
		require.NoError(t, os.MkdirAll(filepath.Join(obj2, "info"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(obj2, "info", "alternates"), []byte(link2+"\n"), 0o644))
		deps, err := objectStoresInsideDeletedRoots(wt2)
		require.NoError(t, err)
		assert.NotEmpty(t, deps)
	})

	t.Run("a path that never touches a root passes", func(t *testing.T) {
		repo3, wt3 := pruneWorktree(t)
		out3 := filepath.Join(tempRoot(t), "real-objects")
		obj3 := filepath.Join(repo3, ".git", "objects")
		require.NoError(t, os.Rename(obj3, out3))
		elsewhere := filepath.Join(tempRoot(t), "link-outside")
		require.NoError(t, os.Symlink(out3, elsewhere))
		require.NoError(t, os.MkdirAll(filepath.Join(obj3, "info"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(obj3, "info", "alternates"), []byte(elsewhere+"\n"), 0o644))
		deps, err := objectStoresInsideDeletedRoots(wt3)
		require.NoError(t, err)
		assert.Empty(t, deps, "a symlink outside both roots is fine")
	})
}

func TestWalkPhysicalAppliesDotDotToTheResolvedPrefix(t *testing.T) {
	base := tempRoot(t)
	real, err := filepath.EvalSymlinks(base)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(real, "a", "b"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(real, "elsewhere", "x"), 0o755))
	// a/link -> ../elsewhere ; so a/link/x/.. is elsewhere, not a.
	require.NoError(t, os.Symlink("../elsewhere", filepath.Join(real, "a", "link")))
	got, hit, err := walkPhysical(real+"/a/link/x/..", []string{filepath.Join(real, "a", "b")})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(real, "elsewhere"), got)
	assert.Empty(t, hit)
	_, hit, err = walkPhysical(real+"/a/b/deeper", []string{filepath.Join(real, "a", "b")})
	require.NoError(t, err)
	assert.NotEmpty(t, hit)
	_, _, err = walkPhysical("relative/path", nil)
	assert.Error(t, err)
}

// Every path read while computing the dependency set is walked, not only the stores
// an alternates file lists (Codex on #6186): the alternates FILE itself can be
// reached through a symlink inside the registration leaf. Deleting the leaf deletes
// the file the link points to, and the repository silently loses the store it lists.
func TestAlternatesFileReachedThroughASymlinkInsideTheLeafIsRefused(t *testing.T) {
	repo, wt := pruneWorktree(t)
	leaf, err := registrationLeaf(wt)
	require.NoError(t, err)

	// The real store lives outside both roots and is listed by an alternates file ...
	outside := filepath.Join(tempRoot(t), "real-objects")
	objects := filepath.Join(repo, ".git", "objects")
	require.NoError(t, os.Rename(objects, outside))
	// ... that sits INSIDE the leaf, reached through objects/info -> leaf/info-real.
	infoReal := filepath.Join(leaf, "info-real")
	require.NoError(t, os.MkdirAll(infoReal, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(infoReal, "alternates"), []byte(outside+"\n"), 0o644))
	require.NoError(t, os.MkdirAll(objects, 0o755))
	require.NoError(t, os.Symlink(infoReal, filepath.Join(objects, "info")))
	gitIn(t, wt, "status", "--porcelain") // premise: git follows it and works

	deps, err := objectStoresInsideDeletedRoots(wt)
	require.NoError(t, err)
	require.NotEmpty(t, deps, "the alternates file's lookup path goes through the leaf")
	joined := strings.Join(deps, " ")
	assert.Contains(t, joined, "alternates")
	assert.Contains(t, joined, "info")
	reasons, err := WorktreeUnrecoverableContent(wt)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(reasons, " "), "object store")
}

// The discovery is proven against REAL git, not a model of its output: ordinary
// repos (no alternates) must pass, and the alternate shapes git itself accepts —
// plain, C-quoted with spaces and an escape, and an unquoted path ending in a
// space — must be read exactly (Codex on #6186; the parser had rules of its own and
// rejected every repo whose git printed a key it did not know).
func TestObjectStoreDiscoveryAgainstRealGit(t *testing.T) {
	withAlternate := func(t *testing.T, entryFor func(store string) string, storeName string) (wt, store string) {
		t.Helper()
		repo, wt := pruneWorktree(t)
		store = filepath.Join(tempRoot(t), storeName)
		objects := filepath.Join(repo, ".git", "objects")
		require.NoError(t, os.Rename(objects, store))
		require.NoError(t, os.MkdirAll(filepath.Join(objects, "info"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(objects, "info", "alternates"), []byte(entryFor(store)+"\n"), 0o644))
		gitIn(t, wt, "status", "--porcelain")         // git itself reads the alternate and works
		gitIn(t, wt, "rev-parse", "--verify", "HEAD") // and can resolve an object through it
		return wt, store
	}
	discover := func(t *testing.T, wt string) []string {
		t.Helper()
		deps, err := objectStoresInsideDeletedRoots(wt)
		require.NoError(t, err, "discovery must accept what real git produces")
		reasons, err := WorktreeUnrecoverableContent(wt)
		require.NoError(t, err)
		assert.NotContains(t, strings.Join(reasons, " "), "object store")
		return deps
	}

	t.Run("no alternates at all", func(t *testing.T) {
		_, wt := pruneWorktree(t)
		assert.Empty(t, discover(t, wt))
	})
	t.Run("a plain alternate outside both roots", func(t *testing.T) {
		wt, _ := withAlternate(t, func(s string) string { return s }, "shared-objects")
		assert.Empty(t, discover(t, wt))
	})
	t.Run("a quoted alternate with spaces and an escape", func(t *testing.T) {
		wt, _ := withAlternate(t, func(s string) string {
			// "<dir>/my store\040two": spaces, and \040 for one of them.
			return `"` + filepath.Dir(s) + `/my\040store two"`
		}, "my store two")
		assert.Empty(t, discover(t, wt))
	})
	t.Run("an unquoted alternate ending in a space", func(t *testing.T) {
		wt, store := withAlternate(t, func(s string) string { return s }, "trailing ")
		require.True(t, strings.HasSuffix(store, " "))
		assert.Empty(t, discover(t, wt))
	})
	t.Run("the same shapes INSIDE the leaf are refused", func(t *testing.T) {
		repo, wt := pruneWorktree(t)
		leaf, err := registrationLeaf(wt)
		require.NoError(t, err)
		store := filepath.Join(leaf, "in leaf ")
		objects := filepath.Join(repo, ".git", "objects")
		require.NoError(t, os.Rename(objects, store))
		require.NoError(t, os.MkdirAll(filepath.Join(objects, "info"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(objects, "info", "alternates"), []byte(store+"\n"), 0o644))
		gitIn(t, wt, "status", "--porcelain")
		deps, err := objectStoresInsideDeletedRoots(wt)
		require.NoError(t, err)
		assert.NotEmpty(t, deps)
	})
}

func TestParseGitAlternateListings(t *testing.T) {
	// count-objects -v: only exact "alternate: " lines matter; every other line (any
	// key a git version prints, any warning) is ignored; values are verbatim.
	out := "count: 0\nsize: 0\nin-pack: 3\npacks: 1\nsize-pack: 1\nprune-packable: 0\ngarbage: 0\nsize-garbage: 0\n" +
		"some-future-key: 7\nwarning: something odd\n" +
		"alternate: /plain/path\nalternate: /ends/with/space \nalternate: \"/quoted/a\\040b\"\nnotalternate: /x\n"
	got, err := parseCountObjectsAlternates(out)
	require.NoError(t, err)
	assert.Equal(t, []string{"/plain/path", "/ends/with/space ", "/quoted/a b"}, got)

	none, err := parseCountObjectsAlternates("count: 0\nsize: 0\n")
	require.NoError(t, err)
	assert.Empty(t, none, "a repo with no alternates passes")

	_, err = parseCountObjectsAlternates("alternate: \"/unterminated\n")
	assert.Error(t, err, "a quoted value that does not decode refuses")

	// An alternates file: skip blank and # lines, trim nothing.
	entries, err := parseAlternatesFile("# comment\n\n/a/b \n\"/q uoted\"\n  /leading/spaces\n")
	require.NoError(t, err)
	assert.Equal(t, []string{"/a/b ", "/q uoted", "  /leading/spaces"}, entries)
	_, err = parseAlternatesFile("\"/bad\\q\"\n")
	assert.Error(t, err)
}

// tempRoot is t.TempDir(), or — when tempRootViaSymlink is set — a path that REACHES
// a fresh temp directory through a symlink, so the as-written and the canonical
// spellings of every test path differ, exactly as on macOS where /var and /tmp are
// symlinks into /private (master's Test (macOS) failure after #6186).
var tempRootViaSymlink bool

func tempRoot(t *testing.T) string {
	t.Helper()
	real := t.TempDir()
	if !tempRootViaSymlink {
		return real
	}
	link := filepath.Join(t.TempDir(), "via-link")
	require.NoError(t, os.Symlink(real, link))
	return link
}

// Every prune containment test, again, with every temp path reached through a
// symlink: both sides of each containment comparison must be canonical, or a store
// inside a deletion root does not look inside it.
func TestPruneContainmentWhenTheTempRootIsReachedThroughASymlink(t *testing.T) {
	prev := tempRootViaSymlink
	tempRootViaSymlink = true
	t.Cleanup(func() { tempRootViaSymlink = prev })
	for name, fn := range map[string]func(*testing.T){
		"object stores":                TestObjectStoresInsideDeletedRootsAreRefusedWhateverTheyAreNamed,
		"quoted alternate":             TestQuotedAlternateInsideTheLeafIsRefused,
		"store path through symlink":   TestObjectStorePathThroughASymlinkInsideTheLeafIsRefused,
		"alternates file through link": TestAlternatesFileReachedThroughASymlinkInsideTheLeafIsRefused,
		"discovery against real git":   TestObjectStoreDiscoveryAgainstRealGit,
		"object storage shape":         TestGitObjectStorageUnderTheDeletedRoots,
		"any submodule":                TestWorktreeWithAnySubmoduleIsRefused,
		"hiding flags":                 TestUnrecoverableContentSeesHidingFlags,
		"occupant nonce":               TestArchiveOccupantNonceRejectsARecreatedWorktree,
		"leaf scoped removal":          TestRemoveArchivedRegistrationLeafIsBoundToTheRecordedOccupant,
		"empty leaf":                   TestResumeFinishesAnEmptyLeafLeftAfterTheNonceWasRemoved,
		"locked registration":          TestLockedRegistrationIsRefusedByTheVerdict,
	} {
		t.Run(name, fn)
	}
}

// canonPath is path with its directory resolved through symlinks, the form git
// prints in `worktree list` (the entry itself may no longer exist).
func canonPath(t *testing.T, path string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	require.NoError(t, err)
	return filepath.Join(dir, filepath.Base(path))
}

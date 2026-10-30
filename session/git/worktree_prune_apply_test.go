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
		rel, err := filepath.Rel(filepath.Join(repo, ".git", "objects"), store)
		require.NoError(t, err)
		moveOriginObjects(t, repo, store, rel)
		deps, err := objectStoresInsideDeletedRoots(wt)
		require.NoError(t, err)
		assert.NotEmpty(t, deps)
	})

	t.Run("transitively: an outside store whose alternates list a store inside", func(t *testing.T) {
		repo, wt := pruneWorktree(t)
		leaf, err := registrationLeaf(wt)
		require.NoError(t, err)
		inner := filepath.Join(leaf, "deep")
		outer := filepath.Join(t.TempDir(), "outer-objects")
		moveOriginObjects(t, repo, inner, outer)
		require.NoError(t, os.MkdirAll(filepath.Join(outer, "info"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(outer, "info", "alternates"), []byte(inner+"\n"), 0o644))
		deps, err := objectStoresInsideDeletedRoots(wt)
		require.NoError(t, err)
		assert.NotEmpty(t, deps)
	})

	t.Run("an alternate outside both roots is allowed", func(t *testing.T) {
		repo, wt := pruneWorktree(t)
		outside := filepath.Join(t.TempDir(), "shared-objects")
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
		moveOriginObjects(t, repo, filepath.Join(t.TempDir(), "o"), filepath.Join(t.TempDir(), "o"))
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

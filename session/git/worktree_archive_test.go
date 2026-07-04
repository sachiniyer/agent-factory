package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archiveTestWorktree creates a real git repo with one commit and a registered
// linked worktree on branch arch/branch, and returns a GitWorktree bound to it
// plus the repo root. The worktree carries an uncommitted file so callers can
// assert dirty-tree preservation across a move.
func archiveTestWorktree(t *testing.T) (gw *GitWorktree, repoRoot, wtPath string) {
	t.Helper()
	sandboxHome(t)
	repoRoot = createGitRepo(t)
	runGitInPlaceTest(t, repoRoot, "commit", "--allow-empty", "-m", "init")
	wtPath = filepath.Join(filepath.Dir(repoRoot), "repo-arch-src")
	runGitInPlaceTest(t, repoRoot, "worktree", "add", "-b", "arch/branch", wtPath)

	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted work"), 0644))

	var err error
	gw, err = NewGitWorktreeFromStorage(repoRoot, wtPath, "arch", "arch/branch", "", false, true)
	require.NoError(t, err)
	return gw, repoRoot, wtPath
}

// assertLiveWorktreeAt asserts that gw's worktree is a valid, registered git
// worktree at path, on branch arch/branch, with its uncommitted file intact.
func assertLiveWorktreeAt(t *testing.T, gw *GitWorktree, path string) {
	t.Helper()
	assert.Equal(t, path, gw.GetWorktreePath(), "worktree path must be updated to the new location")
	assert.True(t, pathExists(path), "the worktree directory must exist at the new location")

	registered, err := gw.isWorktreeRegistered()
	require.NoError(t, err)
	assert.True(t, registered, "git must still list the worktree at its new path")

	assert.Equal(t, "arch/branch",
		runGitInPlaceTest(t, path, "rev-parse", "--abbrev-ref", "HEAD"),
		"the branch must survive the move")

	dirty, err := os.ReadFile(filepath.Join(path, "dirty.txt"))
	require.NoError(t, err, "uncommitted file must survive the move")
	assert.Equal(t, "uncommitted work", string(dirty))
}

// TestMoveWorktree_FastPathPreservesTreeAndReregisters: the `git worktree move`
// fast path relocates the directory, keeps the branch + uncommitted changes, and
// leaves git's registration pointing at the new path.
func TestMoveWorktree_FastPathPreservesTreeAndReregisters(t *testing.T) {
	gw, _, srcPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived", "repoid", "arch")

	require.NoError(t, gw.MoveWorktree(dest))

	assert.False(t, pathExists(srcPath), "the source directory must be gone after a move")
	assertLiveWorktreeAt(t, gw, dest)
}

// TestMoveWorktree_FallbackRepairsRegistration forces the fast path to fail (as
// a cross-device EXDEV would) and asserts the manual-move + `git worktree
// repair` fallback still lands a valid, registered worktree with its dirty tree.
func TestMoveWorktree_FallbackRepairsRegistration(t *testing.T) {
	prev := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		return errors.New("forced fast-path failure (simulating EXDEV)")
	}
	t.Cleanup(func() { worktreeMoveFast = prev })

	gw, _, srcPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived", "repoid", "arch")

	require.NoError(t, gw.MoveWorktree(dest))

	assert.False(t, pathExists(srcPath), "the source directory must be gone after the fallback move")
	assertLiveWorktreeAt(t, gw, dest)
}

// TestSiblingWorktreePath_DefaultCollisionAndSanitize: the restore-side path
// computation returns {repoParent}/{repoName}-{safeTitle}, appends a numeric
// suffix when that path is occupied, and sanitizes the title into a single safe
// segment — mirroring NewGitWorktree's layout so restore lands the worktree
// where a fresh session's would live (#1028).
func TestSiblingWorktreePath_DefaultCollisionAndSanitize(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t) // {tmp}/repo
	parent := filepath.Dir(repoRoot)

	p, err := SiblingWorktreePath(repoRoot, "feature-x")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(parent, "repo-feature-x"), p)

	// Collision: occupy the default path, expect the "-2" suffix.
	require.NoError(t, os.MkdirAll(p, 0755))
	p2, err := SiblingWorktreePath(repoRoot, "feature-x")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(parent, "repo-feature-x-2"), p2)

	// Sanitize: "/" -> "-", ".." stripped.
	ps, err := SiblingWorktreePath(repoRoot, "a/b..c")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(parent, "repo-a-bc"), ps)
}

// TestMoveWorktree_RepairFailureStillCommitsLocation (#1028 Greptile P1): in the
// cross-filesystem fallback, when the byte-move succeeds but `git worktree
// repair` fails, the worktree object must already point at dest — where the
// bytes now live — never at the removed src. Otherwise the caller (the archive
// move-failure path, which marks the instance Lost) would be stranded pointing
// at an empty path while the files sit safely at dest.
func TestMoveWorktree_RepairFailureStillCommitsLocation(t *testing.T) {
	prevMove := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		return errors.New("forced fast-path failure (simulating EXDEV)")
	}
	t.Cleanup(func() { worktreeMoveFast = prevMove })
	prevRepair := worktreeRepair
	worktreeRepair = func(*GitWorktree, string) error {
		return errors.New("forced repair failure")
	}
	t.Cleanup(func() { worktreeRepair = prevRepair })

	gw, _, srcPath := archiveTestWorktree(t)
	dest := filepath.Join(t.TempDir(), "archived", "repoid", "arch")

	err := gw.MoveWorktree(dest)
	require.Error(t, err, "a repair failure must surface to the caller")

	assert.Equal(t, dest, gw.GetWorktreePath(),
		"even on repair failure, worktreePath must point at dest (where the bytes are), never the removed src")
	assert.True(t, pathExists(dest), "the bytes must be recoverable at dest")
	assert.False(t, pathExists(srcPath), "the src must have been moved away")

	dirty, rerr := os.ReadFile(filepath.Join(dest, "dirty.txt"))
	require.NoError(t, rerr, "uncommitted work must survive the byte move")
	assert.Equal(t, "uncommitted work", string(dirty))
}

// TestRestoreWorktreeTo_RoundTripPreservesUncommitted archives then restores a
// worktree and asserts the uncommitted tree survives BOTH moves and the final
// location is a valid, registered worktree.
func TestRestoreWorktreeTo_RoundTripPreservesUncommitted(t *testing.T) {
	gw, _, _ := archiveTestWorktree(t)
	archiveDest := filepath.Join(t.TempDir(), "archived", "repoid", "arch")
	require.NoError(t, gw.MoveWorktree(archiveDest))
	assertLiveWorktreeAt(t, gw, archiveDest)

	restoreDest := filepath.Join(t.TempDir(), "restored", "repo-arch")
	require.NoError(t, gw.RestoreWorktreeTo(restoreDest))

	assert.False(t, pathExists(archiveDest), "the archive directory must be gone after restore")
	assertLiveWorktreeAt(t, gw, restoreDest)
}

// TestRestoreWorktreeTo_RepoGone: when the origin repo has been deleted, restore
// returns ErrRepoGone and leaves the archived worktree intact for manual
// salvage.
func TestRestoreWorktreeTo_RepoGone(t *testing.T) {
	gw, repoRoot, _ := archiveTestWorktree(t)
	archiveDest := filepath.Join(t.TempDir(), "archived", "repoid", "arch")
	require.NoError(t, gw.MoveWorktree(archiveDest))

	require.NoError(t, os.RemoveAll(repoRoot), "simulate the origin repo being deleted")

	err := gw.RestoreWorktreeTo(filepath.Join(t.TempDir(), "restored", "repo-arch"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRepoGone), "a deleted origin repo must surface as ErrRepoGone, got %v", err)
	assert.True(t, pathExists(archiveDest), "the archived worktree must be left intact when the repo is gone")
	assert.Equal(t, archiveDest, gw.GetWorktreePath(), "a failed restore must not move the worktree path")
}

// TestMoveWorktree_RejectsExternalWorktree: an in-place/external worktree is
// user-owned and must never be relocated.
func TestMoveWorktree_RejectsExternalWorktree(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGitInPlaceTest(t, repoRoot, "commit", "--allow-empty", "-m", "init")

	gw, err := NewGitWorktreeFromStorage(repoRoot, repoRoot, "inplace", "master", "", true /*external*/, false)
	require.NoError(t, err)

	err = gw.MoveWorktree(filepath.Join(t.TempDir(), "nope"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "external")
	assert.True(t, pathExists(repoRoot), "the user's in-place tree must be untouched")
}

// TestMoveWorktree_RejectsExistingDestination: relocation must refuse to clobber
// an existing destination.
func TestMoveWorktree_RejectsExistingDestination(t *testing.T) {
	gw, _, srcPath := archiveTestWorktree(t)
	dest := t.TempDir() // already exists
	err := gw.MoveWorktree(dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	assert.True(t, pathExists(srcPath), "a rejected move must leave the source in place")
}

// TestCopyTree_PreservesModesAndSymlinks unit-tests the cross-device copy engine
// (the EXDEV fallback path can't be forced with a real second filesystem in a
// hermetic test): file contents, permission bits, nested dirs, and symlinks must
// all round-trip.
func TestCopyTree_PreservesModesAndSymlinks(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0640))
	require.NoError(t, os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("beta"), 0600))
	require.NoError(t, os.Symlink("a.txt", filepath.Join(src, "link")))

	dest := filepath.Join(t.TempDir(), "dest")
	require.NoError(t, copyTree(src, dest))

	a, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "alpha", string(a))
	b, err := os.ReadFile(filepath.Join(dest, "sub", "b.txt"))
	require.NoError(t, err)
	assert.Equal(t, "beta", string(b))

	aInfo, err := os.Stat(filepath.Join(dest, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0640), aInfo.Mode().Perm(), "file permission bits must be preserved")

	linkTarget, err := os.Readlink(filepath.Join(dest, "link"))
	require.NoError(t, err, "symlink must be copied as a link, not followed")
	assert.Equal(t, "a.txt", linkTarget)
}

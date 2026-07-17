package git

import (
	"bytes"
	"errors"
	stdlog "log"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	aflog "github.com/sachiniyer/agent-factory/log"
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

// archiveTestWorktreeWithSubmodule creates a linked worktree whose nested
// submodules are initialized. Git refuses to move this shape via `git worktree
// move`, so it exercises the archive fallback path that raw-moves bytes and
// repairs gitdirs at every submodule depth.
func archiveTestWorktreeWithSubmodule(t *testing.T) (gw *GitWorktree, repoRoot, wtPath string) {
	t.Helper()
	sandboxHome(t)

	nestedRoot := createGitRepo(t)
	runGitInPlaceTest(t, nestedRoot, "commit", "--allow-empty", "-m", "nested submodule init")

	subRoot := createGitRepo(t)
	runGitInPlaceTest(t, subRoot, "commit", "--allow-empty", "-m", "submodule init")
	runGitInPlaceTest(t, subRoot, "-c", "protocol.file.allow=always", "submodule", "add", nestedRoot, "nested/child")
	runGitInPlaceTest(t, subRoot, "commit", "-m", "add nested submodule")

	repoRoot = createGitRepo(t)
	runGitInPlaceTest(t, repoRoot, "commit", "--allow-empty", "-m", "init")
	runGitInPlaceTest(t, repoRoot, "-c", "protocol.file.allow=always", "submodule", "add", subRoot, "deps/sub")
	runGitInPlaceTest(t, repoRoot, "commit", "-m", "add submodule")

	wtPath = filepath.Join(filepath.Dir(repoRoot), "repo-arch-sub-src")
	runGitInPlaceTest(t, repoRoot, "worktree", "add", "-b", "arch/branch", wtPath)
	runGitInPlaceTest(t, wtPath, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")

	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted work"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "deps", "sub", "dirty-sub.txt"), []byte("submodule work"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "deps", "sub", "nested", "child", "dirty-nested.txt"), []byte("nested work"), 0644))

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

// assertSubmoduleIntactAt asserts the initialized submodule still has a live
// gitdir pointer after an archive/restore move and preserved its dirty file.
func assertSubmoduleIntactAt(t *testing.T, path string) {
	t.Helper()
	subPath := filepath.Join(path, "deps", "sub")

	assert.Equal(t, subPath,
		runGitInPlaceTest(t, subPath, "rev-parse", "--show-toplevel"),
		"the submodule gitdir must point at this moved submodule")
	assert.Contains(t,
		runGitInPlaceTest(t, subPath, "status", "--short"),
		"dirty-sub.txt",
		"uncommitted submodule work must survive the move")

	dirty, err := os.ReadFile(filepath.Join(subPath, "dirty-sub.txt"))
	require.NoError(t, err, "submodule dirty file must survive the move")
	assert.Equal(t, "submodule work", string(dirty))

	nestedPath := filepath.Join(subPath, "nested", "child")
	assert.Equal(t, nestedPath,
		runGitInPlaceTest(t, nestedPath, "rev-parse", "--show-toplevel"),
		"the nested submodule gitdir must point at this moved nested submodule")
	assert.Contains(t,
		runGitInPlaceTest(t, nestedPath, "status", "--short"),
		"dirty-nested.txt",
		"uncommitted nested submodule work must survive the move")

	nestedDirty, err := os.ReadFile(filepath.Join(nestedPath, "dirty-nested.txt"))
	require.NoError(t, err, "nested submodule dirty file must survive the move")
	assert.Equal(t, "nested work", string(nestedDirty))
}

// TestMoveWorktree_FastPathPreservesTreeAndReregisters: the `git worktree move`
// fast path relocates the directory, keeps the branch + uncommitted changes, and
// leaves git's registration pointing at the new path.
func TestMoveWorktree_FastPathPreservesTreeAndReregisters(t *testing.T) {
	gw, _, srcPath := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

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
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	require.NoError(t, gw.MoveWorktree(dest))

	assert.False(t, pathExists(srcPath), "the source directory must be gone after the fallback move")
	assertLiveWorktreeAt(t, gw, dest)
}

// TestMoveWorktree_CrossDeviceCopyCleanupFailureCommitsCopiedLocation covers a
// copy-then-remove failure in the cross-device fallback. Before #1475, the
// error returned before worktreePath was updated, so callers persisted the
// partially deleted source while the complete copy at dest was orphaned.
func TestMoveWorktree_CrossDeviceCopyCleanupFailureCommitsCopiedLocation(t *testing.T) {
	prevMove := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		return errors.New("forced fast-path failure")
	}
	t.Cleanup(func() { worktreeMoveFast = prevMove })

	prevRename := renamePath
	renamePath = func(_, _ string) error {
		return syscall.EXDEV
	}
	t.Cleanup(func() { renamePath = prevRename })

	gw, _, srcPath := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	cleanupErr := errors.New("forced source cleanup failure")
	prevRemoveAll := removeAllPath
	removeAllPath = func(path string) error {
		if path == srcPath {
			return cleanupErr
		}
		return os.RemoveAll(path)
	}
	t.Cleanup(func() { removeAllPath = prevRemoveAll })

	err := gw.MoveWorktree(dest)
	require.Error(t, err)
	assert.ErrorIs(t, err, cleanupErr)
	assert.Contains(t, err.Error(), "worktree copied and registered")
	assert.True(t, pathExists(srcPath), "the source cleanup failure leaves the original for manual cleanup")
	assertLiveWorktreeAt(t, gw, dest)
}

// TestRestoreWorktreeTo_FallbackRepairsSubmoduleGitdirs archives and restores an
// initialized submodule worktree through the manual-move fallback. Before #1459,
// `git worktree repair` fixed the superproject but left deps/sub/.git pointing
// at the old relative path, so the archived worktree was not a valid git repo.
func TestRestoreWorktreeTo_FallbackRepairsSubmoduleGitdirs(t *testing.T) {
	prev := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		return errors.New("forced fast-path failure")
	}
	t.Cleanup(func() { worktreeMoveFast = prev })

	gw, _, srcPath := archiveTestWorktreeWithSubmodule(t)
	archiveDest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, gw.MoveWorktree(archiveDest))

	assert.False(t, pathExists(srcPath), "the source directory must be gone after archive")
	assertLiveWorktreeAt(t, gw, archiveDest)
	assertSubmoduleIntactAt(t, archiveDest)

	restoreDest := filepath.Join(testguard.CanonicalTempDir(t), "restored", "repo-arch-sub-restored")
	require.NoError(t, gw.RestoreWorktreeTo(restoreDest))

	assert.False(t, pathExists(archiveDest), "the archive directory must be gone after restore")
	assertLiveWorktreeAt(t, gw, restoreDest)
	assertSubmoduleIntactAt(t, restoreDest)
}

// TestRestoreWorktreeTo_SubmoduleRepairFailureIsBestEffort proves the
// submodule-gitdir repair cannot strand an archive/restore after the byte move
// and superproject registration repair already succeeded. The worktree bytes and
// git registration are at dest, so the only safe outcome is a warning plus nil.
func TestRestoreWorktreeTo_SubmoduleRepairFailureIsBestEffort(t *testing.T) {
	prevMove := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		return errors.New("forced fast-path failure")
	}
	t.Cleanup(func() { worktreeMoveFast = prevMove })
	prevSubmoduleRepair := worktreeRepairSubmodules
	worktreeRepairSubmodules = func(*GitWorktree, string) error {
		return errors.New("forced submodule repair failure")
	}
	t.Cleanup(func() { worktreeRepairSubmodules = prevSubmoduleRepair })
	var warnings bytes.Buffer
	origWarning := aflog.WarningLog
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	t.Cleanup(func() { aflog.WarningLog = origWarning })

	gw, _, _ := archiveTestWorktree(t)
	archiveDest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, gw.MoveWorktree(archiveDest))
	assertLiveWorktreeAt(t, gw, archiveDest)

	restoreDest := filepath.Join(testguard.CanonicalTempDir(t), "restored", "repo-arch-restored")
	require.NoError(t, gw.RestoreWorktreeTo(restoreDest))

	assert.False(t, pathExists(archiveDest), "the archive directory must be gone after restore")
	assertLiveWorktreeAt(t, gw, restoreDest)
	assert.Contains(t, warnings.String(), "submodule gitdir repair failed after moving worktree")
	// The advice is a command a human pastes, so it goes through the shellsuggest
	// seam (#1978). It used to be built with %q — Go quoting, which renders a
	// double-quoted string a shell still expands `$` and backticks inside, so it
	// LOOKED quoted and was not.
	assert.Contains(t, warnings.String(), shellsuggest.Command("git", "-C", restoreDest, "submodule", "absorbgitdirs"))
	assert.Contains(t, warnings.String(), shellsuggest.Command("git", "-C", restoreDest, "submodule", "update", "--init", "--recursive"))
}

// TestRestoreWorktreePath_SiblingCollisionAndSanitize: in the default sibling
// mode the restore-side path computation returns {repoParent}/{repoName}-
// {safeTitle}, appends a numeric suffix when that path is occupied, and
// sanitizes the title into a single safe segment — mirroring NewGitWorktree's
// layout so restore lands the worktree where a fresh session's would live (#1028).
// The branch name is ignored in sibling mode.
func TestRestoreWorktreePath_SiblingCollisionAndSanitize(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t) // {tmp}/repo
	parent := filepath.Dir(repoRoot)

	p, err := RestoreWorktreePath(repoRoot, "feature-x", "af/feature-x")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(parent, "repo-feature-x"), p)

	// Collision: occupy the default path, expect the "-2" suffix.
	require.NoError(t, os.MkdirAll(p, 0755))
	p2, err := RestoreWorktreePath(repoRoot, "feature-x", "af/feature-x")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(parent, "repo-feature-x-2"), p2)

	// Sanitize: "/" -> "-", ".." stripped.
	ps, err := RestoreWorktreePath(repoRoot, "a/b..c", "af/ab..c")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(parent, "repo-a-bc"), ps)

	spaced, err := RestoreWorktreePath(repoRoot, "Review Threads", "af/review")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(parent, "repo-Review-Threads"), spaced)
	assert.NotContains(t, filepath.Base(spaced), " ")
}

// TestRestoreWorktreePath_SubdirectoryHonorsWorktreeRoot is the #1540 regression:
// for a user with worktree_root=subdirectory, the restore destination must live
// under $AF_HOME/worktrees/<branch> — exactly where NewGitWorktree creates it —
// not stranded beside the repo. A collision still suffixes.
func TestRestoreWorktreePath_SubdirectoryHonorsWorktreeRoot(t *testing.T) {
	sandboxHome(t)
	cfg := config.DefaultConfig()
	cfg.WorktreeRoot = config.WorktreeRootSubdirectory
	require.NoError(t, config.SaveConfig(cfg))

	repoRoot := createGitRepo(t)

	configDir, err := config.GetConfigDir()
	require.NoError(t, err)
	worktreesDir := filepath.Join(configDir, "worktrees")

	// Restore must match NewGitWorktree's subdirectory layout: {worktrees}/{branch}.
	created, _, err := NewGitWorktree(repoRoot, "feature-x")
	require.NoError(t, err)
	branch := created.GetBranchName()
	assert.Equal(t, filepath.Join(worktreesDir, branch), created.GetWorktreePath(),
		"sanity: creation places the worktree under the subdirectory root")

	p, err := RestoreWorktreePath(repoRoot, "feature-x", branch)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(worktreesDir, branch), p,
		"restore must land the worktree under the subdirectory root, honoring worktree_root")

	// Collision under the subdirectory root suffixes, not falls back to sibling.
	require.NoError(t, os.MkdirAll(p, 0755))
	p2, err := RestoreWorktreePath(repoRoot, "feature-x", branch)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(worktreesDir, branch+"-2"), p2)
}

// TestRestoreWorktreePath_SubdirectoryEmptyBranchFallsBackToTitle is the
// Greptile P1 on #1540: a legacy/edge archived record with an EMPTY persisted
// branch must still restore under subdirectory mode. With no branch, branch-based
// placement would resolve to the worktrees root itself and fail the strict-inside
// guard — a regression, since the old title-based sibling path restored such
// records fine. The destination must fall back to the sanitized title leaf.
func TestRestoreWorktreePath_SubdirectoryEmptyBranchFallsBackToTitle(t *testing.T) {
	sandboxHome(t)
	cfg := config.DefaultConfig()
	cfg.WorktreeRoot = config.WorktreeRootSubdirectory
	require.NoError(t, config.SaveConfig(cfg))

	repoRoot := createGitRepo(t)
	configDir, err := config.GetConfigDir()
	require.NoError(t, err)
	worktreesDir := filepath.Join(configDir, "worktrees")

	p, err := RestoreWorktreePath(repoRoot, "feature-x", "")
	require.NoError(t, err, "an empty-branch archive must still resolve a valid restore path")
	assert.Equal(t, filepath.Join(worktreesDir, "feature-x"), p,
		"an empty branch must fall back to the sanitized title leaf under the subdirectory root")
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
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

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
	archiveDest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, gw.MoveWorktree(archiveDest))
	assertLiveWorktreeAt(t, gw, archiveDest)

	restoreDest := filepath.Join(testguard.CanonicalTempDir(t), "restored", "repo-arch")
	require.NoError(t, gw.RestoreWorktreeTo(restoreDest))

	assert.False(t, pathExists(archiveDest), "the archive directory must be gone after restore")
	assertLiveWorktreeAt(t, gw, restoreDest)
}

// TestRestoreWorktreeTo_RepoGone: when the origin repo has been deleted, restore
// returns ErrRepoGone and leaves the archived worktree intact for manual
// salvage.
func TestRestoreWorktreeTo_RepoGone(t *testing.T) {
	gw, repoRoot, _ := archiveTestWorktree(t)
	archiveDest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, gw.MoveWorktree(archiveDest))

	require.NoError(t, os.RemoveAll(repoRoot), "simulate the origin repo being deleted")

	err := gw.RestoreWorktreeTo(filepath.Join(testguard.CanonicalTempDir(t), "restored", "repo-arch"))
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

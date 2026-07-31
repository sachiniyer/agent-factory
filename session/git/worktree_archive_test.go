package git

import (
	"bytes"
	"errors"
	"fmt"
	stdlog "log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

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

	var info, warnings bytes.Buffer
	previousInfo := aflog.InfoLog
	previousWarning := aflog.WarningLog
	aflog.InfoLog = stdlog.New(&info, "INFO: ", 0)
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	t.Cleanup(func() {
		aflog.InfoLog = previousInfo
		aflog.WarningLog = previousWarning
	})

	gw, _, srcPath := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	require.NoError(t, gw.MoveWorktree(dest))
	require.Contains(t, info.String(), "using manual move + repair",
		"the fast-path limitation remains visible as the reason for choosing the fallback")
	require.Empty(t, warnings.String(),
		"a successful designed fallback must not look like a failed archive")

	assert.False(t, pathExists(srcPath), "the source directory must be gone after the fallback move")
	assertLiveWorktreeAt(t, gw, dest)
}

// TestMoveWorktree_CrossDeviceCopyCleanupFailureCommitsCopiedLocation covers a
// copy-then-remove failure in the cross-device fallback. Two invariants stack
// here: worktreePath must commit to dest where the bytes live (the #1475 fix —
// the error used to return before worktreePath was updated, so callers persisted
// the partially deleted source while the complete copy at dest was orphaned),
// AND the copy+register success must NOT surface as an error (#2011 — an error
// return drives the caller into a retry that registers a second, orphaned
// worktree). The only correct outcome is nil + a warning naming the leftover.
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

	var warnings bytes.Buffer
	origWarning := aflog.WarningLog
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	t.Cleanup(func() { aflog.WarningLog = origWarning })

	gw, _, srcPath := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	cleanupErr := errors.New("forced source cleanup failure")
	prevRemoveTree := removeDirectoryTree
	var leftoverPath string
	removeDirectoryTree = func(parent *os.File, name, path string, directory *os.File, expected *copiedDirectory) error {
		if filepath.Dir(path) == filepath.Dir(srcPath) && strings.Contains(filepath.Base(path), ".af-source-") {
			leftoverPath = path
			return cleanupErr
		}
		return prevRemoveTree(parent, name, path, directory, expected)
	}
	t.Cleanup(func() { removeDirectoryTree = prevRemoveTree })

	require.NoError(t, gw.MoveWorktree(dest),
		"a copy+register success with only source cleanup failing must not error (#2011)")
	assert.NotEmpty(t, leftoverPath, "the test must intercept cleanup of the atomically secured source")
	assert.True(t, pathExists(leftoverPath), "the source cleanup failure leaves the secured original for manual cleanup")
	assert.False(t, pathExists(srcPath), "the original pathname must not remain live after the source is secured")
	assertLiveWorktreeAt(t, gw, dest) // worktreePath committed to dest (#1475), still valid + registered
	assert.Contains(t, warnings.String(), "failed to remove the leftover source directory")
	assert.Contains(t, warnings.String(), leftoverPath)
	assert.Contains(t, warnings.String(), shellsuggest.Command("rm", "-rf", leftoverPath),
		"manual cleanup advice must target the secured source that actually remains")
	assert.NotContains(t, warnings.String(), shellsuggest.Command("rm", "-rf", srcPath),
		"the original source pathname is absent after quarantine and must not be recommended")
}

// countBranchWorktrees returns how many registered worktrees in repoRoot are
// checked out on branch, per `git worktree list --porcelain`. Used to prove a
// relocate leaves exactly ONE — never an orphaned second registration.
func countBranchWorktrees(t *testing.T, repoRoot, branch string) int {
	t.Helper()
	out := runGitInPlaceTest(t, repoRoot, "worktree", "list", "--porcelain")
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "branch refs/heads/"+branch {
			n++
		}
	}
	return n
}

// TestRelocate_CopySucceedsCleanupFails_NoErrorNoRetryOrphan is the #2011
// regression. On a cross-device relocate where the byte copy AND `git worktree
// repair` both succeed but removing the SOURCE directory fails, relocate must
// return nil: the worktree is valid, registered, and usable at dest — a leftover
// source dir is a disk-reclamation nuisance, not a move failure. Returning an
// error here is what corrupts state: it drives the caller's archive-rollback /
// restore-retry logic even though a valid worktree already exists at dest, and
// the retry picks a fresh collision-suffixed dest, copies + registers a SECOND
// worktree, and orphans the first — corrupting `git worktree list` and branch
// exclusivity.
//
// The test drives the REAL MoveWorktree path (the same relocateWorktreeTo engine
// RestoreWorktreeTo uses) — NOT moveDirCrossDevice directly, which would skip the
// repair/registration gate that must have SUCCEEDED for this bug to apply — and
// models the caller's retry-on-error loop: a correct relocate returns nil on
// attempt 1, so the loop never advances to a second dest and no orphan is born.
func TestRelocate_CopySucceedsCleanupFails_NoErrorNoRetryOrphan(t *testing.T) {
	prevMove := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		return errors.New("forced fast-path failure (simulating EXDEV)")
	}
	t.Cleanup(func() { worktreeMoveFast = prevMove })

	prevRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = prevRename })

	var warnings bytes.Buffer
	origWarning := aflog.WarningLog
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	t.Cleanup(func() { aflog.WarningLog = origWarning })

	gw, repoRoot, srcPath := archiveTestWorktree(t)

	// Fail removal of whatever the CURRENT source directory is on every attempt: a
	// copy+register success paired with a persistent source-cleanup failure. Keying
	// on the atomically secured source sibling preserves the same failure shape
	// without reopening the original pathname.
	prevRemoveTree := removeDirectoryTree
	var leftoverPath string
	removeDirectoryTree = func(parent *os.File, name, path string, directory *os.File, expected *copiedDirectory) error {
		if filepath.Dir(path) == filepath.Dir(srcPath) && strings.Contains(filepath.Base(path), ".af-source-") {
			leftoverPath = path
			return errors.New("forced source cleanup failure")
		}
		return prevRemoveTree(parent, name, path, directory, expected)
	}
	t.Cleanup(func() { removeDirectoryTree = prevRemoveTree })

	// Model the restore/retry loop: the daemon recomputes a collision-suffixed dest
	// and retries whenever the move returns an error (RestoreArchived via
	// RestoreWorktreePath). A correct relocate stops the loop after one attempt.
	baseDest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	dest2 := baseDest + "-2"

	dest := baseDest
	attempts := 0
	var lastErr error
	for attempts < 2 {
		attempts++
		lastErr = gw.MoveWorktree(dest)
		if lastErr == nil {
			break
		}
		dest = dest2 // collision handling would pick the next free path on retry
	}

	require.NoError(t, lastErr,
		"copy+register success with a failed source cleanup must return nil, not error (else the caller retries into a second worktree)")
	assert.Equal(t, 1, attempts,
		"a nil return must stop the caller after one attempt — no retry")
	assert.False(t, pathExists(dest2),
		"no second (orphaned) worktree directory must be created by a retry")

	// The one valid worktree is registered at the first dest, branch + dirty tree intact.
	assertLiveWorktreeAt(t, gw, baseDest)

	// The un-removed source is a leftover directory for manual reclamation — NOT a
	// registered worktree. Git tracks exactly one worktree on the branch.
	assert.NotEmpty(t, leftoverPath, "the test must intercept cleanup of the atomically secured source")
	assert.True(t, pathExists(leftoverPath),
		"the source cleanup failure leaves the secured original directory for manual reclamation")
	assert.False(t, pathExists(srcPath),
		"the original pathname must not remain live after the source is secured")
	assert.Equal(t, 1, countBranchWorktrees(t, repoRoot, "arch/branch"),
		"git must track exactly one worktree for the branch — no orphan")

	// The cleanup failure is surfaced (visible, not swallowed) so the leftover disk is reclaimable.
	assert.Contains(t, warnings.String(), "failed to remove")
	assert.Contains(t, warnings.String(), leftoverPath)
}

// TestMoveWorktree_UnverifiedCleanupPathOmitsDeleteAdvice drives a quarantine
// endpoint replacement through the real relocate path. The destination remains
// the operational worktree, but the retained descriptor no longer reveals the
// original source pathname; warning text must not recommend deleting whatever
// uncopied directory now occupies that stale private name.
func TestMoveWorktree_UnverifiedCleanupPathOmitsDeleteAdvice(t *testing.T) {
	previousMove := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		return errors.New("forced fast-path failure")
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })

	previousRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = previousRename })

	var warnings bytes.Buffer
	originalWarning := aflog.WarningLog
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	t.Cleanup(func() { aflog.WarningLog = originalWarning })

	worktree, _, sourcePath := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	originalRemoveTree := removeDirectoryTree
	var replacementPath, movedOriginal string
	removeDirectoryTree = func(parent *os.File, name, path string, directory *os.File, expected *copiedDirectory) error {
		if filepath.Dir(path) != filepath.Dir(sourcePath) || !strings.Contains(filepath.Base(path), ".af-source-") {
			return originalRemoveTree(parent, name, path, directory, expected)
		}
		replacementPath = path
		movedOriginal = path + ".moved"
		if err := os.Rename(path, movedOriginal); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, "replacement.txt"), []byte("replacement"), 0644); err != nil {
			return err
		}
		return originalRemoveTree(parent, name, path, directory, expected)
	}
	t.Cleanup(func() { removeDirectoryTree = originalRemoveTree })

	require.NoError(t, worktree.MoveWorktree(dest),
		"a committed and repaired destination must not trigger a duplicate retry")
	assert.NotEmpty(t, replacementPath)
	assert.FileExists(t, filepath.Join(replacementPath, "replacement.txt"))
	assert.FileExists(t, filepath.Join(movedOriginal, "dirty.txt"))
	assert.NotContains(t, warnings.String(), shellsuggest.Command("rm", "-rf", replacementPath),
		"an unverified quarantine name must never become destructive manual advice")
	assert.Contains(t, warnings.String(), "could not determine")
}

// TestMoveWorktree_CleanupErrorRechecksQuarantineIdentity moves the secured
// root after cleanup's initial identity check, then makes the retained tree
// non-writable so the child claim returns an ordinary permission error. Every
// error exit must recheck the root name before deciding manual advice is safe.
func TestMoveWorktree_CleanupErrorRechecksQuarantineIdentity(t *testing.T) {
	previousMove := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		return errors.New("forced fast-path failure")
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })
	previousRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = previousRename })

	var warnings bytes.Buffer
	originalWarning := aflog.WarningLog
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	t.Cleanup(func() { aflog.WarningLog = originalWarning })

	worktree, _, sourcePath := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	previousHook := removeTreeBeforeEntryClaim
	var stalePath, movedOriginal string
	removeTreeBeforeEntryClaim = func(directory *os.File, path string) error {
		if stalePath != "" || filepath.Dir(path) != filepath.Dir(sourcePath) || !strings.Contains(filepath.Base(path), ".af-source-") {
			return nil
		}
		stalePath = path
		movedOriginal = path + ".moved"
		if err := os.Rename(path, movedOriginal); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, "replacement.txt"), []byte("replacement"), 0644); err != nil {
			return err
		}
		return unix.Fchmod(int(directory.Fd()), 0500)
	}
	t.Cleanup(func() { removeTreeBeforeEntryClaim = previousHook })
	t.Cleanup(func() {
		if movedOriginal != "" {
			_ = os.Chmod(movedOriginal, 0700)
		}
	})

	require.NoError(t, worktree.MoveWorktree(dest))
	assert.NotEmpty(t, stalePath)
	assert.FileExists(t, filepath.Join(stalePath, "replacement.txt"))
	assert.NotContains(t, warnings.String(), shellsuggest.Command("rm", "-rf", stalePath))
	assert.Contains(t, warnings.String(), "could not determine")
}

// TestRestoreWorktreeTo_FallbackRepairsSubmoduleGitdirs archives and restores an
// initialized submodule worktree through the manual-move fallback. Before #1459,
// `git worktree repair` fixed the superproject but left deps/sub/.git pointing
// at the old relative path, so the archived worktree was not a valid git repo.
func TestRestoreWorktreeTo_FallbackRepairsSubmoduleGitdirs(t *testing.T) {
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

// TestCopyTree_AllowsSymlinkedDestinationParent preserves configured layouts
// where (for example) $AGENT_FACTORY_HOME/worktrees is intentionally a symlink
// to another filesystem. The copy must anchor itself to the resolved directory
// descriptor without rejecting that parent symlink.
func TestCopyTree_AllowsSymlinkedDestinationParent(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "tracked.txt"), []byte("tracked"), 0644))

	realParent := filepath.Join(t.TempDir(), "real-parent")
	require.NoError(t, os.Mkdir(realParent, 0755))
	linkedParent := filepath.Join(t.TempDir(), "linked-parent")
	require.NoError(t, os.Symlink(realParent, linkedParent))
	dest := filepath.Join(linkedParent, "dest")

	require.NoError(t, copyTree(src, dest))
	contents, err := os.ReadFile(filepath.Join(realParent, "dest", "tracked.txt"))
	require.NoError(t, err)
	assert.Equal(t, "tracked", string(contents))
}

// TestMoveDirCrossDevice_SourceRootReplacementIsNotDeleted covers a worktree
// root renamed after copyTree opens it and replaced at the original pathname.
// The opened tree may be copied consistently, but cleanup must not recursively
// delete the different, uncopied directory now occupying src.
func TestMoveDirCrossDevice_SourceRootReplacementIsNotDeleted(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	movedOriginal := filepath.Join(parent, "moved-original")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "original.txt"), []byte("original"), 0644))
	dest := filepath.Join(t.TempDir(), "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalHook := copyTreeBeforeSourceOpen
	copyTreeBeforeSourceOpen = func(path string) error {
		if path != src {
			return nil
		}
		if err := os.Rename(src, movedOriginal); err != nil {
			return err
		}
		if err := os.Mkdir(src, 0755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(src, "replacement.txt"), []byte("replacement"), 0644)
	}
	t.Cleanup(func() { copyTreeBeforeSourceOpen = originalHook })

	err := moveDirCrossDevice(src, dest)
	require.Error(t, err, "cleanup must fail closed when src no longer identifies the copied root")
	assert.Contains(t, err.Error(), "source directory changed")
	replacement, readErr := os.ReadFile(filepath.Join(src, "replacement.txt"))
	require.NoError(t, readErr, "the uncopied replacement at src must not be deleted")
	assert.Equal(t, "replacement", string(replacement))
	original, readErr := os.ReadFile(filepath.Join(movedOriginal, "original.txt"))
	require.NoError(t, readErr, "the renamed original must remain recoverable")
	assert.Equal(t, "original", string(original))
	assert.NoDirExists(t, dest, "a failed identity check must clean the partial destination")
}

// TestMoveDirCrossDevice_DestinationReplacementDoesNotCommit covers the copied
// destination being renamed after its descriptor is opened and replaced at the
// pathname that relocateWorktreeTo would commit. The source must remain until
// the final destination is atomically proven to be the tree that was copied.
func TestMoveDirCrossDevice_DestinationReplacementDoesNotCommit(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "original.txt"), []byte("original"), 0644))
	dest := filepath.Join(t.TempDir(), "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalHook := moveDirBeforeDestCommit
	var movedCopy string
	moveDirBeforeDestCommit = func(openedDest string) error {
		movedCopy = openedDest + ".moved"
		if err := os.Rename(openedDest, movedCopy); err != nil {
			return err
		}
		if err := os.Mkdir(openedDest, 0755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(openedDest, "replacement.txt"), []byte("replacement"), 0644)
	}
	t.Cleanup(func() { moveDirBeforeDestCommit = originalHook })

	err := moveDirCrossDevice(src, dest)
	require.Error(t, err, "a replacement destination must not be committed")
	assert.Contains(t, err.Error(), "destination directory changed")
	assert.FileExists(t, filepath.Join(src, "original.txt"), "source must remain when destination identity is lost")
	assert.FileExists(t, filepath.Join(dest, "replacement.txt"), "the raced-in destination must not be deleted")
	assert.FileExists(t, filepath.Join(movedCopy, "original.txt"), "the copied tree must remain recoverable")
}

// TestMoveDirCrossDevice_CopiedDescendantReplacementDoesNotCommit covers a
// copied child being renamed out of the staging tree and replaced after its
// descriptor was validated. Publishing only the root identity would commit the
// replacement, strand the bytes that were actually copied, and delete source.
func TestMoveDirCrossDevice_CopiedDescendantReplacementDoesNotCommit(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "sub", "original.txt"), []byte("original"), 0644))
	dest := filepath.Join(t.TempDir(), "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalHook := moveDirBeforeDestCommit
	moveDirBeforeDestCommit = func(stagingPath string) error {
		stagedChild := filepath.Join(stagingPath, "sub")
		if err := os.Rename(stagedChild, filepath.Join(stagingPath, "stranded-copy")); err != nil {
			return err
		}
		if err := os.Mkdir(stagedChild, 0755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(stagedChild, "replacement.txt"), []byte("replacement"), 0644)
	}
	t.Cleanup(func() { moveDirBeforeDestCommit = originalHook })

	err := moveDirCrossDevice(src, dest)
	require.Error(t, err, "a replacement descendant must prevent destination commit")
	assert.Contains(t, err.Error(), "destination tree changed")
	assert.FileExists(t, filepath.Join(src, "sub", "original.txt"), "source must be restored intact")
	assert.NoDirExists(t, dest, "the replacement tree must not be published")
}

// TestMoveDirCrossDevice_SourceReplacementAtCleanupIsNotDeleted forces the
// replacement after the last identity check but before the old pathname-based
// RemoveAll. Cleanup must first claim the verified source endpoint atomically;
// it must never recursively reopen and delete whatever now occupies src.
func TestMoveDirCrossDevice_SourceReplacementAtCleanupIsNotDeleted(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	movedOriginal := filepath.Join(parent, "moved-original")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "original.txt"), []byte("original"), 0644))
	dest := filepath.Join(t.TempDir(), "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalHook := moveDirBeforeSourceCommit
	moveDirBeforeSourceCommit = func(path string) error {
		if err := os.Rename(path, movedOriginal); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(path, "replacement.txt"), []byte("replacement"), 0644)
	}
	t.Cleanup(func() { moveDirBeforeSourceCommit = originalHook })

	err := moveDirCrossDevice(src, dest)
	require.Error(t, err, "cleanup must fail closed when the source endpoint changes")
	assert.Contains(t, err.Error(), "source directory changed")
	assert.FileExists(t, filepath.Join(src, "replacement.txt"), "the uncopied replacement must not be deleted")
	assert.FileExists(t, filepath.Join(movedOriginal, "original.txt"), "the renamed original must remain recoverable")
}

// TestMoveDirCrossDevice_DestinationParentReopenFailureRestoresSource covers the
// destination parent changing after the source was atomically quarantined. A
// failed second parent open must restore src rather than strand the worktree at
// an unrecorded private name.
func TestMoveDirCrossDevice_DestinationParentReopenFailureRestoresSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "original.txt"), []byte("original"), 0644))
	destinationParent := filepath.Join(t.TempDir(), "destination-parent")
	require.NoError(t, os.Mkdir(destinationParent, 0755))
	dest := filepath.Join(destinationParent, "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalHook := moveDirBeforeDestParentOpen
	moveDirBeforeDestParentOpen = func(path string) error {
		return os.Rename(path, path+".moved")
	}
	t.Cleanup(func() { moveDirBeforeDestParentOpen = originalHook })

	err := moveDirCrossDevice(src, dest)
	require.Error(t, err, "the removed destination parent must abort the move")
	assert.FileExists(t, filepath.Join(src, "original.txt"), "source must be restored after destination-parent reopen fails")
}

// TestMoveDirCrossDevice_SourceParentReplacementInvalidatesRollback moves the
// source parent after its descriptor is retained, then forces a destination
// failure. Restoring through the fd is not enough: the textual source parent
// must still identify that descriptor before rollback can be reported intact.
func TestMoveDirCrossDevice_SourceParentReplacementInvalidatesRollback(t *testing.T) {
	root := t.TempDir()
	sourceParent := filepath.Join(root, "source-parent")
	require.NoError(t, os.Mkdir(sourceParent, 0755))
	src := filepath.Join(sourceParent, "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "original.txt"), []byte("original"), 0644))
	dest := filepath.Join(t.TempDir(), "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalHook := moveDirBeforeDestParentOpen
	movedParent := sourceParent + ".moved"
	moveDirBeforeDestParentOpen = func(string) error {
		if err := os.Rename(sourceParent, movedParent); err != nil {
			return err
		}
		if err := os.Mkdir(sourceParent, 0755); err != nil {
			return err
		}
		return errors.New("forced destination failure after source parent replacement")
	}
	t.Cleanup(func() { moveDirBeforeDestParentOpen = originalHook })

	err := moveDirCrossDevice(src, dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source parent path changed")
	assert.FileExists(t, filepath.Join(movedParent, "src", "original.txt"),
		"the original remains recoverable in the retained parent")
	assert.NoFileExists(t, filepath.Join(src, "original.txt"),
		"the replacement textual parent must not be reported as a successful rollback")
}

// TestMoveDirCrossDevice_ChangedPublishedDescendantIsNotDeleted replaces a
// copied child after publication but before post-commit validation. Rollback
// cleanup must use the copied destination identities, not snapshot and delete
// the replacement tree it has just discovered.
func TestMoveDirCrossDevice_ChangedPublishedDescendantIsNotDeleted(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "sub", "original.txt"), []byte("original"), 0644))
	dest := filepath.Join(t.TempDir(), "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalHook := moveDirAfterDestCommit
	var strandedCopy string
	moveDirAfterDestCommit = func(path string) error {
		strandedCopy = filepath.Join(path, "stranded-copy")
		if err := os.Rename(filepath.Join(path, "sub"), strandedCopy); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(path, "sub"), 0755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(path, "sub", "replacement.txt"), []byte("replacement"), 0644)
	}
	t.Cleanup(func() { moveDirAfterDestCommit = originalHook })

	err := moveDirCrossDevice(src, dest)
	require.Error(t, err)
	assert.FileExists(t, filepath.Join(src, "sub", "original.txt"), "source must be restored")
	assert.FileExists(t, filepath.Join(dest, "sub", "replacement.txt"),
		"uncopied replacement data must survive failed destination cleanup")
	assert.FileExists(t, filepath.Join(strandedCopy, "original.txt"),
		"the moved copied subtree must remain recoverable")
}

// TestMoveDirCrossDevice_FastRenameParentReplacementRollsBack changes the
// destination parent path after the fd-relative same-filesystem rename. The
// helper must reject the stale textual destination and restore source through
// the retained descriptors.
func TestMoveDirCrossDevice_FastRenameParentReplacementRollsBack(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "original.txt"), []byte("original"), 0644))
	destinationRoot := t.TempDir()
	destinationParent := filepath.Join(destinationRoot, "parent")
	require.NoError(t, os.Mkdir(destinationParent, 0755))
	dest := filepath.Join(destinationParent, "dest")
	movedParent := destinationParent + ".moved"

	originalHook := renamePathAfterCommit
	renamePathAfterCommit = func(string) error {
		if err := os.Rename(destinationParent, movedParent); err != nil {
			return err
		}
		return os.Mkdir(destinationParent, 0755)
	}
	t.Cleanup(func() { renamePathAfterCommit = originalHook })

	err := moveDirCrossDevice(src, dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "destination parent path changed")
	assert.FileExists(t, filepath.Join(src, "original.txt"), "failed fast rename must restore source")
	assert.NoDirExists(t, dest)
	assert.NoDirExists(t, filepath.Join(movedParent, "dest"), "rolled-back worktree must not remain stranded")
}

// TestMoveDirCrossDevice_SecuredSourceOpenFailureDoesNotStrandSource changes
// source permissions after the copy but before quarantine. The move may finish
// through a descriptor retained from the copy; if it cannot, it must restore
// the source pathname instead of leaving only an unrecorded private sibling.
func TestMoveDirCrossDevice_SecuredSourceOpenFailureDoesNotStrandSource(t *testing.T) {
	sourceParent := t.TempDir()
	src := filepath.Join(sourceParent, "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "original.txt"), []byte("original"), 0644))
	dest := filepath.Join(t.TempDir(), "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalHook := moveDirBeforeSourceCommit
	moveDirBeforeSourceCommit = func(path string) error { return os.Chmod(path, 0) }
	t.Cleanup(func() { moveDirBeforeSourceCommit = originalHook })
	t.Cleanup(func() {
		matches, _ := filepath.Glob(filepath.Join(sourceParent, ".af-source-*"))
		for _, match := range matches {
			_ = os.Chmod(match, 0700)
		}
		_ = os.Chmod(src, 0700)
	})

	err := moveDirCrossDevice(src, dest)
	if err != nil {
		_, statErr := os.Lstat(src)
		require.NoError(t, statErr, "a failed move must restore the secured source pathname")
		require.NoError(t, os.Chmod(src, 0700))
		assert.FileExists(t, filepath.Join(src, "original.txt"),
			"the restored source must retain its contents")
		return
	}
	assert.FileExists(t, filepath.Join(dest, "original.txt"),
		"a retained source descriptor may allow the move to complete safely")
	assert.NoDirExists(t, src)
}

// TestMoveDirCrossDevice_PrePublicationFailureCleansPrivateStaging proves the
// staging lifetime covers failures after a successful copy, not only failures
// encountered during traversal.
func TestMoveDirCrossDevice_PrePublicationFailureCleansPrivateStaging(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "original.txt"), []byte("original"), 0644))
	destinationParent := t.TempDir()
	dest := filepath.Join(destinationParent, "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalHook := moveDirBeforeSourceCommit
	moveDirBeforeSourceCommit = func(string) error { return errors.New("forced failure after copy") }
	t.Cleanup(func() { moveDirBeforeSourceCommit = originalHook })

	err := moveDirCrossDevice(src, dest)
	require.Error(t, err)
	entries, readErr := os.ReadDir(destinationParent)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "every pre-publication exit must remove private staging")
	assert.FileExists(t, filepath.Join(src, "original.txt"), "failed move must leave source intact")
}

func TestMoveDirCrossDevice_ChangedStagingDescendantIsNotDeleted(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "sub", "original.txt"), []byte("original"), 0644))
	destinationParent := t.TempDir()
	dest := filepath.Join(destinationParent, "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalHook := moveDirBeforeSourceCommit
	var stagingPath, strandedCopy string
	moveDirBeforeSourceCommit = func(string) error {
		entries, err := os.ReadDir(destinationParent)
		if err != nil || len(entries) != 1 {
			return fmt.Errorf("locate staging directory: entries=%d: %w", len(entries), err)
		}
		stagingPath = filepath.Join(destinationParent, entries[0].Name())
		strandedCopy = filepath.Join(stagingPath, "stranded-copy")
		if err := os.Rename(filepath.Join(stagingPath, "sub"), strandedCopy); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(stagingPath, "sub"), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(stagingPath, "sub", "replacement.txt"), []byte("replacement"), 0644); err != nil {
			return err
		}
		return errors.New("forced failure after staging replacement")
	}
	t.Cleanup(func() { moveDirBeforeSourceCommit = originalHook })

	require.Error(t, moveDirCrossDevice(src, dest))
	assert.FileExists(t, filepath.Join(stagingPath, "sub", "replacement.txt"))
	assert.FileExists(t, filepath.Join(strandedCopy, "original.txt"))
	assert.FileExists(t, filepath.Join(src, "sub", "original.txt"))
}

func TestMoveDirCrossDevice_SourceCleanupParentReplacementIsUnverified(t *testing.T) {
	root := t.TempDir()
	sourceParent := filepath.Join(root, "source-parent")
	require.NoError(t, os.Mkdir(sourceParent, 0755))
	src := filepath.Join(sourceParent, "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "original.txt"), []byte("original"), 0644))
	dest := filepath.Join(t.TempDir(), "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalRemoveTree := removeDirectoryTree
	movedParent := sourceParent + ".moved"
	removeDirectoryTree = func(parent *os.File, name, path string, directory *os.File, expected *copiedDirectory) error {
		if filepath.Dir(path) != sourceParent || !strings.Contains(filepath.Base(path), ".af-source-") {
			return originalRemoveTree(parent, name, path, directory, expected)
		}
		require.NoError(t, os.Rename(sourceParent, movedParent))
		require.NoError(t, os.Mkdir(sourceParent, 0755))
		return errors.New("forced cleanup failure after source parent replacement")
	}
	t.Cleanup(func() { removeDirectoryTree = originalRemoveTree })

	err := moveDirCrossDevice(src, dest)
	var cleanupErr *copiedWorktreeSourceCleanupError
	require.ErrorAs(t, err, &cleanupErr)
	assert.False(t, cleanupErr.cleanupPathVerified,
		"a retained descriptor does not verify a pathname through a replaced parent")
}

// TestMoveDirCrossDevice_SourceCleanupReplacementIsNotDeleted injects a
// replacement at the private quarantine pathname immediately before the old
// path-based cleanup. Cleanup must stay bound to the opened source object and
// must never recursively delete an uncopied replacement.
func TestMoveDirCrossDevice_SourceCleanupReplacementIsNotDeleted(t *testing.T) {
	sourceParent := t.TempDir()
	src := filepath.Join(sourceParent, "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "original.txt"), []byte("original"), 0644))
	dest := filepath.Join(t.TempDir(), "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalRemoveTree := removeDirectoryTree
	var replacementPath, movedOriginal string
	removeDirectoryTree = func(parent *os.File, name, path string, directory *os.File, expected *copiedDirectory) error {
		if filepath.Dir(path) != sourceParent || !strings.Contains(filepath.Base(path), ".af-source-") {
			return originalRemoveTree(parent, name, path, directory, expected)
		}
		replacementPath = path
		movedOriginal = path + ".moved"
		if err := os.Rename(path, movedOriginal); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, "replacement.txt"), []byte("replacement"), 0644); err != nil {
			return err
		}
		return originalRemoveTree(parent, name, path, directory, expected)
	}
	t.Cleanup(func() { removeDirectoryTree = originalRemoveTree })

	err := moveDirCrossDevice(src, dest)
	assert.NotEmpty(t, replacementPath, "the test must reach secured-source cleanup")
	assert.Error(t, err, "losing the secured endpoint must be reported as an indeterminate cleanup")
	assert.FileExists(t, filepath.Join(replacementPath, "replacement.txt"),
		"the uncopied replacement at the quarantine name must not be deleted")
	assert.FileExists(t, filepath.Join(movedOriginal, "original.txt"),
		"the copied source must remain recoverable when its name changes")
}

// TestMoveDirCrossDevice_CopyFailureCleansPrivateStaging ensures an unsupported
// node cannot leak a nearly complete random staging tree on every archive retry.
func TestMoveDirCrossDevice_CopyFailureCleansPrivateStaging(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "regular.txt"), []byte("regular"), 0644))
	require.NoError(t, syscall.Mkfifo(filepath.Join(src, "unsupported.fifo"), 0600))
	destinationParent := t.TempDir()
	dest := filepath.Join(destinationParent, "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })

	err := moveDirCrossDevice(src, dest)
	require.Error(t, err)
	entries, readErr := os.ReadDir(destinationParent)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "failed copy must remove its private staging tree")
	assert.FileExists(t, filepath.Join(src, "regular.txt"), "copy failure must leave source untouched")
}

// TestMoveDirCrossDevice_InitialStagingOpenFailureCleansCreatedName covers the
// narrow window after mkdirat succeeds but before a descriptor owns staging.
// A restrictive umask makes the new directory unopenable deterministically.
func TestMoveDirCrossDevice_InitialStagingOpenFailureCleansCreatedName(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a mode-000 directory regardless; this scenario needs a non-root uid")
	}
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "tracked.txt"), []byte("tracked"), 0644))
	destinationParent := t.TempDir()
	dest := filepath.Join(destinationParent, "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalUmask := syscall.Umask(0777)
	t.Cleanup(func() { syscall.Umask(originalUmask) })
	err := moveDirCrossDevice(src, dest)
	syscall.Umask(originalUmask)

	require.Error(t, err)
	entries, readErr := os.ReadDir(destinationParent)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "failed initial staging open must remove the created private name")
	assert.FileExists(t, filepath.Join(src, "tracked.txt"))
}

// TestMoveDirCrossDevice_BoundsDescriptorsAcrossDeepTree lowers RLIMIT_NOFILE
// after constructing a valid nested tree. Copy, validation, and cleanup must
// use a fixed descriptor budget rather than retaining fds for every ancestor.
func TestMoveDirCrossDevice_BoundsDescriptorsAcrossDeepTree(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	deepest := src
	for range 24 {
		deepest = filepath.Join(deepest, "d")
		require.NoError(t, os.Mkdir(deepest, 0755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(deepest, "tracked.txt"), []byte("tracked"), 0644))
	dest := filepath.Join(t.TempDir(), "dest")
	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })

	var originalLimit unix.Rlimit
	require.NoError(t, unix.Getrlimit(unix.RLIMIT_NOFILE, &originalLimit))
	limited := originalLimit
	if limited.Cur > 32 {
		limited.Cur = 32
	}
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &limited))
	t.Cleanup(func() { _ = unix.Setrlimit(unix.RLIMIT_NOFILE, &originalLimit) })
	err := moveDirCrossDevice(src, dest)
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &originalLimit))

	require.NoError(t, err, "valid deep moves must not exhaust descriptors")
	assert.FileExists(t, filepath.Join(dest, strings.Repeat("d/", 24), "tracked.txt"))
}

// TestMoveDirCrossDevice_UnsupportedNoReplaceFailsClosed requires an explicit,
// prompt error when the platform cannot provide an atomic no-replace rename.
// Reservation and link/unlink emulations have unavoidable replacement races.
func TestMoveDirCrossDevice_UnsupportedNoReplaceFailsClosed(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "tracked.txt"), []byte("tracked"), 0644))
	dest := filepath.Join(t.TempDir(), "dest")

	originalRename := renamePath
	renamePath = func(_, _ string) error { return unix.ENOSYS }
	t.Cleanup(func() { renamePath = originalRename })

	err := moveDirCrossDevice(src, dest)
	require.ErrorIs(t, err, unix.ENOSYS)
	assert.FileExists(t, filepath.Join(src, "tracked.txt"), "unsupported atomic rename must preserve source")
	assert.NoDirExists(t, dest, "unsupported atomic rename must not create destination")
}

// TestMoveDirCrossDevice_LongLeafFitsPrivateNames covers a valid 239-byte leaf,
// the placement limit that reserves 16 bytes below NAME_MAX. Private staging
// and source names must not append enough text to make that valid path fail.
func TestMoveDirCrossDevice_LongLeafFitsPrivateNames(t *testing.T) {
	leaf := strings.Repeat("w", 239)
	src := filepath.Join(t.TempDir(), leaf)
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "tracked.txt"), []byte("tracked"), 0644))
	dest := filepath.Join(t.TempDir(), leaf)

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })

	require.NoError(t, moveDirCrossDevice(src, dest))
	assert.FileExists(t, filepath.Join(dest, "tracked.txt"))
	assert.NoDirExists(t, src)
}

// TestCopyTree_RejectsNamedPipeWithoutBlocking is the #2654 regression. The
// cross-device move fallback copies a worktree node by node; opening a FIFO as
// though it were a regular file waits for a writer forever. Special files must
// instead fail promptly so ArchiveSession can release its operation/kill guards.
//
// PRE-FIX: copyTree does not return before the deadline. The bounded, nonblocking
// cleanup below attempts to release it solely so the test can report the hang.
func TestCopyTree_RejectsNamedPipeWithoutBlocking(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(src, 0755))
	fifo := filepath.Join(src, "events.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0600))

	dest := filepath.Join(t.TempDir(), "dest")
	assertNamedPipeCopyFailsPromptly(t, fifo, func() error {
		return copyTree(src, dest)
	})
}

// TestCopyFile_RejectsNamedPipeRaceWithoutBlocking closes the Lstat/open race in
// #2689's first fix. A worktree process can replace a path after copyTree sees a
// regular file but before copyFile opens it; copyFile must validate the object it
// actually opened without ever making a blocking FIFO open.
//
// PRE-FIX: copyFile blocks until the helper supplies a writer, then returns nil.
func TestCopyFile_RejectsNamedPipeRaceWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "events.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0600))
	dest := filepath.Join(t.TempDir(), "dest")

	assertNamedPipeCopyFailsPromptly(t, fifo, func() error {
		return copyFile(fifo, dest)
	})
}

// TestCopyTree_RejectsDirectoryToNamedPipeRaceWithoutBlocking covers the
// traversal side of the metadata/open race. filepath.Walk inspects a directory
// and then opens that pathname with a blocking call before invoking its callback.
// The seam forces the equivalent inspect/open boundary in the descriptor walker:
// replacing the directory with a FIFO there must be rejected without blocking.
func TestCopyTree_RejectsDirectoryToNamedPipeRaceWithoutBlocking(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dir := filepath.Join(src, "sub")
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked"), 0644))

	originalHook := copyTreeBeforeSourceOpen
	t.Cleanup(func() { copyTreeBeforeSourceOpen = originalHook })
	swapped := false
	copyTreeBeforeSourceOpen = func(path string) error {
		if path != dir || swapped {
			return nil
		}
		swapped = true
		if err := os.Rename(dir, dir+".original"); err != nil {
			return err
		}
		return syscall.Mkfifo(dir, 0600)
	}

	dest := filepath.Join(t.TempDir(), "dest")
	assertNamedPipeCopyFailsPromptly(t, dir, func() error {
		return copyTree(src, dest)
	})
}

// TestCopyFile_DoesNotFollowReplacementSymlink covers a regular path replaced
// by a symlink between traversal metadata and open. The source open must reject
// the link rather than archiving contents from outside the worktree.
func TestCopyFile_DoesNotFollowReplacementSymlink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside-secret")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0600))
	src := filepath.Join(t.TempDir(), "raced-source")
	require.NoError(t, os.Symlink(outside, src))
	dest := filepath.Join(t.TempDir(), "dest")

	err := copyFile(src, dest)
	require.Error(t, err, "a replacement symlink must not be followed")
	assert.Contains(t, err.Error(), src)
	assert.NoFileExists(t, dest)
}

// TestCopyFile_RejectsRacedInDestinationNodes proves a destination node that
// appears after the initial absence check is never opened. A symlink must not
// redirect/truncate another file, and a FIFO must not block waiting for a reader.
func TestCopyFile_RejectsRacedInDestinationNodes(t *testing.T) {
	src := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(src, []byte("new contents"), 0644))

	t.Run("symlink", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "outside")
		require.NoError(t, os.WriteFile(outside, []byte("keep me"), 0600))
		dest := filepath.Join(t.TempDir(), "raced-destination")
		require.NoError(t, os.Symlink(outside, dest))

		err := copyFile(src, dest)
		require.Error(t, err, "a raced-in destination symlink must be rejected")
		contents, readErr := os.ReadFile(outside)
		require.NoError(t, readErr)
		assert.Equal(t, "keep me", string(contents), "the symlink target must not be truncated")
	})

	t.Run("named pipe", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "raced-destination.fifo")
		require.NoError(t, syscall.Mkfifo(dest, 0600))
		assertNamedPipeDestinationFailsPromptly(t, dest, func() error {
			return copyFile(src, dest)
		})
	})
}

func assertNamedPipeCopyFailsPromptly(t *testing.T, fifo string, copyFn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- copyFn()
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a FIFO cannot be copied as a regular file")
		assert.Contains(t, err.Error(), "cannot move worktree across filesystems")
		assert.Contains(t, err.Error(), fifo)
	case <-time.After(500 * time.Millisecond):
		fd, unblockErr := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if unblockErr == nil {
			unblockErr = syscall.Close(fd)
		}
		select {
		case eventualErr := <-done:
			t.Fatalf("HUNG: copy blocked opening named pipe %s; nonblocking cleanup returned %v and the copy eventually returned %v", fifo, unblockErr, eventualErr)
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("HUNG: copy blocked opening named pipe %s and did not return after bounded nonblocking cleanup (%v)", fifo, unblockErr)
		}
	}
}

func assertNamedPipeDestinationFailsPromptly(t *testing.T, fifo string, copyFn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- copyFn()
	}()

	select {
	case err := <-done:
		require.Error(t, err, "an existing destination FIFO must be rejected")
		assert.Contains(t, err.Error(), fifo)
	case <-time.After(500 * time.Millisecond):
		fd, unblockErr := syscall.Open(fifo, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if unblockErr != nil {
			t.Fatalf("HUNG: copy blocked opening destination named pipe %s and bounded cleanup could not open a reader: %v", fifo, unblockErr)
		}
		defer syscall.Close(fd)
		select {
		case eventualErr := <-done:
			t.Fatalf("HUNG: copy blocked opening destination named pipe %s; after bounded cleanup supplied a reader, copy returned %v", fifo, eventualErr)
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("HUNG: copy blocked opening destination named pipe %s and did not return after bounded cleanup", fifo)
		}
	}
}

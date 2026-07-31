package git

import (
	"bytes"
	stdlog "log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	aflog "github.com/sachiniyer/agent-factory/log"
)

// Which relocate path an archive/restore takes, and what it does when a bounded
// step is cut off. Split out of worktree_archive_test.go for the file-length
// limit (#1145); the round-trip and cleanup-failure cases stay there.

// archiveTestWorktreeWithUninitializedSubmodule creates a linked worktree whose
// declared submodule was never checked out — the shape any clone without
// --recursive produces. `git submodule status` still prints a line for it,
// marked with a leading '-', but `git worktree move` handles this worktree
// natively, so the fast path must NOT be skipped for it.
func archiveTestWorktreeWithUninitializedSubmodule(t *testing.T) (gw *GitWorktree, repoRoot, wtPath string) {
	t.Helper()
	sandboxHome(t)

	subRoot := createGitRepo(t)
	runGitInPlaceTest(t, subRoot, "commit", "--allow-empty", "-m", "submodule init")

	repoRoot = createGitRepo(t)
	runGitInPlaceTest(t, repoRoot, "commit", "--allow-empty", "-m", "init")
	runGitInPlaceTest(t, repoRoot, "-c", "protocol.file.allow=always", "submodule", "add", subRoot, "deps/sub")
	runGitInPlaceTest(t, repoRoot, "commit", "-m", "add submodule")

	wtPath = filepath.Join(filepath.Dir(repoRoot), "repo-arch-uninit-src")
	// No `submodule update --init`: the gitlink is declared, never initialized.
	runGitInPlaceTest(t, repoRoot, "worktree", "add", "-b", "arch/branch", wtPath)

	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted work"), 0644))

	var err error
	gw, err = NewGitWorktreeFromStorage(repoRoot, wtPath, "arch", "arch/branch", "", false, true)
	require.NoError(t, err)
	return gw, repoRoot, wtPath
}
func TestMoveWorktree_SubmodulesUseDesignedFallbackWithoutWarning(t *testing.T) {
	realFastMove := worktreeMoveFast
	fastMoveCalls := 0
	worktreeMoveFast = func(g *GitWorktree, src, dest string) error {
		fastMoveCalls++
		return realFastMove(g, src, dest)
	}
	t.Cleanup(func() { worktreeMoveFast = realFastMove })

	var warnings bytes.Buffer
	previousWarning := aflog.WarningLog
	aflog.WarningLog = stdlog.New(&warnings, "WARNING: ", 0)
	t.Cleanup(func() { aflog.WarningLog = previousWarning })

	gw, _, srcPath := archiveTestWorktreeWithSubmodule(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	require.NoError(t, gw.MoveWorktree(dest))
	require.Zero(t, fastMoveCalls,
		"git worktree move is documented to reject linked worktrees with submodules")
	require.NotContains(t, warnings.String(), "git worktree move",
		"the designed manual move must not report its skipped unsupported fast path as a failure")
	assert.False(t, pathExists(srcPath))
	assertLiveWorktreeAt(t, gw, dest)
	assertSubmoduleIntactAt(t, dest)
}

// TestMoveWorktree_UninitializedSubmoduleKeepsTheFastPath is the other half of
// the submodule pre-check. "Declared" is not "initialized": a clone without
// --recursive records the gitlink but checks nothing out, and `git submodule
// status` still prints a line for it — marked with a leading '-'. Git moves that
// worktree natively (verified on 2.43), so treating every status line as an
// initialized submodule traded an atomic rename for a byte-move plus a repair
// that can time out or leave a stale registration.
func TestMoveWorktree_UninitializedSubmoduleKeepsTheFastPath(t *testing.T) {
	realFastMove := worktreeMoveFast
	fastMoveCalls := 0
	worktreeMoveFast = func(g *GitWorktree, src, dest string) error {
		fastMoveCalls++
		return realFastMove(g, src, dest)
	}
	t.Cleanup(func() { worktreeMoveFast = realFastMove })

	gw, _, srcPath := archiveTestWorktreeWithUninitializedSubmodule(t)

	// Premise: git really does print a line for the uninitialized entry, so a
	// nonempty-output check WOULD have diverted. If git ever stops, this test is
	// vacuous and should fail here rather than pass for the wrong reason.
	status := runGitInPlaceTest(t, srcPath, "submodule", "status")
	require.NotEmpty(t, status, "premise: an uninitialized submodule still prints a status line")
	require.True(t, strings.HasPrefix(status, "-"),
		"premise: git marks an uninitialized submodule with a leading '-'")

	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, gw.MoveWorktree(dest))
	require.Equal(t, 1, fastMoveCalls,
		"a merely DECLARED submodule does not block `git worktree move`, so the fast path must run")
	assert.False(t, pathExists(srcPath))
	assertLiveWorktreeAt(t, gw, dest)
}

// TestRelocate_DoesNotWedgeOnStalledGit applies the #1917 runner contract to the
// archive/restore path. relocateWorktreeTo now probes for submodules BEFORE the
// fast path, and that probe reads an initialized submodule's gitdir — precisely
// the I/O that stalls on a hung mount. Unbounded, the new pre-check would add a
// fresh way to hang teardown: the daemon holds the session in its optimistic
// Archiving/Restoring state across this call, so a wedge here is permanent.
//
// Every git call on the path runs under localGitTimeout, so a git making no
// progress yields an actionable timeout within the bound. The assertion is
// deliberately on the ERROR, not just on returning: an archive that gives up
// must say so rather than report success it never established.
func TestRelocate_DoesNotWedgeOnStalledGit(t *testing.T) {
	gw, _, _ := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	// Only wedge git AFTER the fixture is built with real git.
	stallingGitOnPath(t)
	shortenLocalTimeout(t, 200*time.Millisecond)

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- gw.MoveWorktree(dest) }()

	select {
	case err := <-done:
		require.Error(t, err, "a stalled git must surface a timeout, never a silent success")
		require.Contains(t, err.Error(), "timed out",
			"the error must name the bound so the operator knows the archive was abandoned, not completed")
		// Bounding created an outcome the unbounded runner could not produce: git
		// was SIGKILLed partway, so what it did is UNESTABLISHED. The error must
		// say so, because a teardown that reads this as an ordinary failed move
		// finalizes over a half-done relocate (session/teardown.go).
		require.ErrorIs(t, err, ErrRelocateStateUnknown,
			"a relocation cut off by its deadline must be distinguishable from one that failed cleanly")
		require.Less(t, time.Since(start), 30*time.Second,
			"each git step is killed at its own deadline, not left waiting for the stalled git to exit")
	case <-time.After(60 * time.Second):
		t.Fatal("relocateWorktreeTo hung on a stalled git (#1917): the session would stay " +
			"wedged in Archiving/Restoring for the daemon's lifetime")
	}
}

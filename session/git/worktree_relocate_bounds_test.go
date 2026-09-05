package git

import (
	"context"
	"fmt"
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
	"github.com/sachiniyer/agent-factory/log/logtest"
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

	var warnings logtest.Buffer
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

// The submodule probe is read-only, so a deadline there leaves the original
// worktree intact. Starting git worktree move or the manual copy fallback after
// that evidence of a stalled filesystem can only make the outcome less certain;
// the cross-device path eventually enters an unbounded recursive source delete.
// Stop at the first deadline and latch it for later Cleanup attempts (#3135).
func TestRelocate_InspectionDeadlineStopsBeforeMoveFallback(t *testing.T) {
	gw, _, src := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) {
		return false, context.DeadlineExceeded
	}
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })

	previousMove := worktreeMoveFast
	moveAttempted := false
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		moveAttempted = true
		return nil
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })

	err := gw.MoveWorktree(dest)

	require.ErrorIs(t, err, ErrRelocateStateUnknown)
	assert.False(t, moveAttempted,
		"no mutating relocation step may start after the read-only probe times out")
	assert.True(t, gw.cleanupHasStalled(),
		"the deadline must latch on the worktree so a later Cleanup also refuses destruction")
	assert.DirExists(t, src, "the original worktree and its uncommitted data stay in place")
	assert.NoDirExists(t, dest)
}

// The identity snapshot is metadata I/O against the same filesystem as the
// worktree. If that mount stalls after the git submodule probe answered, a
// synchronous open/fstat would wedge immediately before the bounded move.
func TestRelocate_SourceIdentityProbeIsBounded(t *testing.T) {
	gw, _, src := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return false, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })

	previousIdentity := relocationPathIdentity
	sourceIdentity, err := previousIdentity(src)
	require.NoError(t, err)
	release := make(chan struct{})
	probeDone := make(chan struct{})
	relocationPathIdentity = func(path string) (pathIdentity, error) {
		if path == src {
			<-release
			close(probeDone)
			return sourceIdentity, nil
		}
		return previousIdentity(path)
	}
	t.Cleanup(func() {
		close(release)
		<-probeDone
		relocationPathIdentity = previousIdentity
	})
	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 100 * time.Millisecond
	t.Cleanup(func() { relocationIdentityTimeout = previousTimeout })

	done := make(chan error, 1)
	go func() { done <- gw.MoveWorktree(dest) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.True(t, gw.cleanupHasStalled())
	case <-time.After(500 * time.Millisecond):
		t.Fatal("the pre-move source identity probe ignored the relocation bound")
	}
}

// A timeout in git worktree move is even less safe to recover automatically:
// git may have renamed bytes, changed registration, done both, or done neither.
// The manual fallback must not perform another move or source deletion on top of
// that unknown state.
func TestRelocate_FastMoveDeadlineStopsBeforeManualFallback(t *testing.T) {
	gw, _, src := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return false, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })

	previousMove := worktreeMoveFast
	worktreeMoveFast = func(_ *GitWorktree, src, dest string) error {
		// Model git's partial-success shape: the directory rename commits, then
		// registration work stalls until the command is cut off.
		require.NoError(t, os.Rename(src, dest))
		return context.DeadlineExceeded
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })

	previousRename := renamePath
	fallbackAttempted := false
	renamePath = func(string, string) error {
		fallbackAttempted = true
		return nil
	}
	t.Cleanup(func() { renamePath = previousRename })

	err := gw.MoveWorktree(dest)

	require.ErrorIs(t, err, ErrRelocateStateUnknown)
	assert.Equal(t, dest, gw.GetWorktreePath(),
		"a verified partial move must preserve the destination as the durable retry location")
	assert.FileExists(t, filepath.Join(dest, "dirty.txt"),
		"the destination contains the user's uncommitted work after the partial move")
	assert.NoDirExists(t, src)
	assert.False(t, fallbackAttempted,
		"the manual move must not run after git worktree move was cut off mid-operation")
	assert.True(t, gw.cleanupHasStalled())
}

func TestRelocate_FastMoveDeadlineWithInconclusiveProbeRetainsDestination(t *testing.T) {
	gw, _, src := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return false, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })

	previousMove := worktreeMoveFast
	worktreeMoveFast = func(_ *GitWorktree, src, dest string) error {
		require.NoError(t, os.Rename(src, dest))
		return context.DeadlineExceeded
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })

	previousIdentity := relocationPathIdentity
	relocationPathIdentity = func(path string) (pathIdentity, error) {
		if path == dest {
			return pathIdentity{}, context.DeadlineExceeded
		}
		return previousIdentity(path)
	}
	t.Cleanup(func() { relocationPathIdentity = previousIdentity })

	err := gw.MoveWorktree(dest)
	require.ErrorIs(t, err, ErrRelocateStateUnknown)
	assert.Equal(t, dest, gw.GetWorktreePath(),
		"the possible destination must remain a durable recovery candidate when verification cannot answer")
	recovery, ok := gw.GetRelocationRecovery()
	require.True(t, ok, "the alternate source handle must survive an inconclusive destination probe")
	assert.Equal(t, src, recovery.AlternatePath)
	assert.FileExists(t, filepath.Join(dest, "dirty.txt"))
	assert.NoDirExists(t, src)
}

func TestRelocate_RetryCompletesTimedOutMoveAlreadyAtDestination(t *testing.T) {
	gw, _, _ := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return false, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })

	previousMove := worktreeMoveFast
	worktreeMoveFast = func(_ *GitWorktree, src, dest string) error {
		require.NoError(t, os.Rename(src, dest))
		return context.DeadlineExceeded
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })

	require.ErrorIs(t, gw.MoveWorktree(dest), ErrRelocateStateUnknown)
	require.NoError(t, gw.MoveWorktree(dest),
		"a retry must repair and complete a move whose bytes already reached the requested destination")
	assert.Equal(t, dest, gw.GetWorktreePath())
	assert.False(t, gw.HasUnresolvedRelocation())
	assert.FileExists(t, filepath.Join(dest, "dirty.txt"))
}

func TestRelocate_RetryUsesOriginalSourceWhenTimedOutMoveDidNothing(t *testing.T) {
	gw, _, src := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return false, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })

	previousMove := worktreeMoveFast
	moveCalls := 0
	worktreeMoveFast = func(g *GitWorktree, src, dest string) error {
		moveCalls++
		if moveCalls == 1 {
			return context.DeadlineExceeded
		}
		return previousMove(g, src, dest)
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })

	require.ErrorIs(t, gw.MoveWorktree(dest), ErrRelocateStateUnknown)
	assert.DirExists(t, src, "the first move did nothing")
	assert.NoDirExists(t, dest)
	require.NoError(t, gw.MoveWorktree(dest))
	assert.False(t, gw.HasUnresolvedRelocation())
	assert.NoDirExists(t, src)
	assert.FileExists(t, filepath.Join(dest, "dirty.txt"))
}

func TestRelocate_RetryCanFallbackAfterSelectingOriginalSource(t *testing.T) {
	gw, _, src := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return false, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })

	previousMove := worktreeMoveFast
	moveCalls := 0
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		moveCalls++
		if moveCalls == 1 {
			return context.DeadlineExceeded
		}
		return fmt.Errorf("cross-device fast move unavailable")
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })

	previousRepair := worktreeRepair
	worktreeRepair = func(*GitWorktree, string) error { return nil }
	t.Cleanup(func() { worktreeRepair = previousRepair })
	previousSubmoduleRepair := worktreeRepairSubmodules
	worktreeRepairSubmodules = func(*GitWorktree, string) error { return nil }
	t.Cleanup(func() { worktreeRepairSubmodules = previousSubmoduleRepair })

	require.ErrorIs(t, gw.MoveWorktree(dest), ErrRelocateStateUnknown)
	assert.DirExists(t, src, "the interrupted fast move did nothing")

	require.NoError(t, gw.MoveWorktree(dest),
		"once identity selects the original source, the supported manual fallback must remain available")
	assert.False(t, gw.HasUnresolvedRelocation())
	assert.NoDirExists(t, src)
	assert.FileExists(t, filepath.Join(dest, "dirty.txt"))
}

func TestRelocate_RetryReturnsClaimWhenLaterGateRefuses(t *testing.T) {
	gw, repo, src := archiveTestWorktree(t)
	firstDest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "first")
	secondDest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "second")

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return false, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })

	previousMove := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		return context.DeadlineExceeded
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })

	require.ErrorIs(t, gw.MoveWorktree(firstDest), ErrRelocateStateUnknown)
	assert.DirExists(t, src, "the interrupted move left the bytes at the alternate source")
	require.NoError(t, os.MkdirAll(secondDest, 0o755))
	require.Error(t, gw.MoveWorktree(secondDest), "the occupied retry destination must stop the move")

	assert.Equal(t, src, gw.GetWorktreePath())
	recovery, ok := gw.GetRelocationRecovery()
	require.True(t, ok,
		"a later gate refusal must return the consumed claim instead of making recovery disappear")
	assert.Equal(t, RelocationRecoveryClaimStale, recovery.State)
	sourceIdentity, identityErr := inspectRelocationPathIdentity(src)
	require.NoError(t, identityErr)
	assert.True(t, recovery.identity().same(sourceIdentity),
		"the returned claim must still identify the conclusively selected source")

	reloaded, err := NewGitWorktreeFromStorage(
		repo, gw.GetWorktreePath(), "arch", gw.GetBranchName(), gw.GetBaseCommitSHA(), false, true,
	)
	require.NoError(t, err)
	require.NoError(t, reloaded.RestoreRelocationRecovery(recovery))
	assert.Equal(t, src, reloaded.GetWorktreePath(),
		"after a later gate refusal, the selected identity must remain loadable across restart")
}

func TestRelocate_RetryRevalidatesDestinationBeforeRegistrationRepair(t *testing.T) {
	gw, _, _ := archiveTestWorktree(t)
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	previousInspect := worktreeContainsSubmodules
	worktreeContainsSubmodules = func(*GitWorktree, string) (bool, error) { return false, nil }
	t.Cleanup(func() { worktreeContainsSubmodules = previousInspect })

	previousMove := worktreeMoveFast
	worktreeMoveFast = func(_ *GitWorktree, src, dest string) error {
		require.NoError(t, os.Rename(src, dest))
		return context.DeadlineExceeded
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })
	require.ErrorIs(t, gw.MoveWorktree(dest), ErrRelocateStateUnknown)

	previousIdentity := relocationPathIdentity
	identityCalls := 0
	movedAside := dest + "-moved-aside"
	relocationPathIdentity = func(path string) (pathIdentity, error) {
		identity, err := previousIdentity(path)
		if path == dest && err == nil {
			identityCalls++
			if identityCalls == 1 {
				require.NoError(t, os.Rename(dest, movedAside))
				require.NoError(t, os.Mkdir(dest, 0o755))
			}
		}
		return identity, err
	}
	t.Cleanup(func() { relocationPathIdentity = previousIdentity })

	previousRepair := worktreeRepair
	repairAttempted := false
	worktreeRepair = func(*GitWorktree, string) error {
		repairAttempted = true
		return nil
	}
	t.Cleanup(func() { worktreeRepair = previousRepair })

	err := gw.MoveWorktree(dest)
	require.ErrorIs(t, err, ErrRelocateStateUnknown)
	assert.False(t, repairAttempted,
		"registration repair must not consume a destination name that changed after candidate resolution")
	assert.True(t, gw.HasUnresolvedRelocation(),
		"both recovery handles must remain durable after destination revalidation fails")
	assert.FileExists(t, filepath.Join(movedAside, "dirty.txt"))
}

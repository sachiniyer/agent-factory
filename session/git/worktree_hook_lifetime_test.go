package git

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCleanupJoinsHooksBeforeInspectingWorktree(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	commitInitial(t, repoRoot)

	gw, _, err := NewGitWorktree(repoRoot, "cleanup-joins-hooks", branchPrefixForTest(t))
	require.NoError(t, err)
	require.NoError(t, gw.Setup())

	hooksDone := make(chan struct{})
	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	gw.hooksDone = hooksDone
	gw.hooksCancel = func() {
		cancelOnce.Do(func() { close(cancelled) })
	}

	previousStat := cleanupWorktreeStat
	statSawJoinedHook := make(chan bool, 1)
	cleanupWorktreeStat = func(path string) (os.FileInfo, error) {
		joined := false
		select {
		case <-hooksDone:
			joined = true
		default:
		}
		statSawJoinedHook <- joined
		return os.Stat(path)
	}
	t.Cleanup(func() { cleanupWorktreeStat = previousStat })

	type cleanupResult struct {
		state CleanupState
		err   error
	}
	result := make(chan cleanupResult, 1)
	go func() {
		state, cleanupErr := gw.Cleanup()
		result <- cleanupResult{state: state, err: cleanupErr}
	}()

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		close(hooksDone)
		t.Fatal("Cleanup never cancelled the active hook run")
	}

	var joined bool
	select {
	case joined = <-statSawJoinedHook:
		close(hooksDone)
	case <-time.After(time.Second):
		// A correct Cleanup is blocked joining the hook runner, so let that
		// runner finish and allow teardown to continue.
		close(hooksDone)
		joined = <-statSawJoinedHook
	}

	cleanup := <-result
	require.NoError(t, cleanup.err)
	require.Equal(t, CleanupSettled, cleanup.state)
	require.True(t, joined,
		"Cleanup inspected and removed the worktree before the cancelled hook runner exited")
}

func TestRebuildRefusesWhenPriorHookRunCannotBeJoined(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	commitInitial(t, repoRoot)

	gw, _, err := NewGitWorktree(repoRoot, "rebuild-joins-hooks", branchPrefixForTest(t))
	require.NoError(t, err)
	require.NoError(t, gw.Setup())

	hooksDone := make(chan struct{})
	gw.hooksDone = hooksDone
	gw.hooksCancel = func() {}

	previousTimeout := hookStopTimeout
	hookStopTimeout = 50 * time.Millisecond
	t.Cleanup(func() { hookStopTimeout = previousTimeout })

	rebuildErr := gw.RebuildFromExistingBranch()
	close(hooksDone)
	state, cleanupErr := gw.Cleanup()
	require.NoError(t, cleanupErr)
	require.Equal(t, CleanupSettled, state)

	require.Error(t, rebuildErr,
		"a rebuild must not modify the checkout while the prior hook runner may still be alive")
	require.Contains(t, rebuildErr.Error(), "did not exit")
}

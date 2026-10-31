//go:build !windows

package git

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A read that never returns (a FIFO with no writer stands in for a stalled mount)
// must not hold the verdict: the archive-nonce read, and every other prune-path
// read, answers "unknown" at the bound, which refuses (Codex on #6186).
func TestPruneFilesystemReadsAreBounded(t *testing.T) {
	_, wt := pruneWorktree(t)
	nonce, err := StampArchiveNonce(wt)
	require.NoError(t, err)
	leaf, err := registrationLeaf(wt)
	require.NoError(t, err)

	// Replace the nonce file with a FIFO nobody writes: opening it for read blocks.
	nonceFile := filepath.Join(leaf, archiveNonceFile)
	require.NoError(t, os.Remove(nonceFile))
	require.NoError(t, syscall.Mkfifo(nonceFile, 0o600))
	t.Cleanup(func() { // release the abandoned reader
		if f, err := os.OpenFile(nonceFile, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})

	prev := pruneFSOpTimeout
	pruneFSOpTimeout = 100 * time.Millisecond
	t.Cleanup(func() { pruneFSOpTimeout = prev })

	start := time.Now()
	err = VerifyArchiveOccupant(wt, nonce, time.Now())
	require.Error(t, err, "unknown must refuse")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second, "the caller was not held by the stalled read")
}

func TestBoundedFSOpTimesOutAsUnknown(t *testing.T) {
	prev := pruneFSOpTimeout
	pruneFSOpTimeout = 50 * time.Millisecond
	t.Cleanup(func() { pruneFSOpTimeout = prev })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	_, err := BoundedFSOp("stalled", func() (int, error) { <-release; return 1, nil })
	require.ErrorIs(t, err, context.DeadlineExceeded)
	v, err := BoundedFSOp("fine", func() (int, error) { return 7, nil })
	require.NoError(t, err)
	assert.Equal(t, 7, v)
}

// A bounded mutation that times out has an UNKNOWN outcome: it keeps running, and
// nothing acts on the same paths again until it provably finished (Codex on #6186).
func TestTimedOutMutationBlocksActingAgainUntilItFinishes(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	err := BoundedMutationWithin(50*time.Millisecond, "renaming a", []string{"/p/a", "/p/b"}, func() error {
		<-release
		close(finished)
		return nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	for _, key := range []string{"/p/a", "/p/b"} {
		require.Error(t, MutationInFlight(key), "still running: the outcome is unknown")
	}
	require.NoError(t, MutationInFlight("/p/unrelated"))
	require.Error(t, BoundedMutationWithin(time.Second, "second", []string{"/p/a"}, func() error { return nil }),
		"a new mutation on the same path refuses while the abandoned one runs")

	close(release)
	<-finished
	require.Eventually(t, func() bool { return MutationInFlight("/p/a", "/p/b") == nil }, time.Second, 5*time.Millisecond,
		"once it provably finished, the paths are free again")
}

// Under the start-up resume budget every bounded operation uses min(its own bound,
// what remains), and once the budget is spent an operation does not even start
// (Codex on #6186): one stalled row cannot overrun the budget.
func TestOperationBudgetCapsEveryBoundedOperation(t *testing.T) {
	prev := pruneFSOpTimeout
	pruneFSOpTimeout = 10 * time.Second
	t.Cleanup(func() { pruneFSOpTimeout = prev })

	t.Run("a spent budget refuses to start the operation", func(t *testing.T) {
		clear := SetOperationBudget(time.Now().Add(-time.Second))
		defer clear()
		ran := false
		_, err := BoundedFSOp("op", func() (int, error) { ran = true; return 1, nil })
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.False(t, ran, "the operation was not started")
		err = BoundedMutationWithin(time.Minute, "mutation", []string{"/b/spent"}, func() error { ran = true; return nil })
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.False(t, ran)
		_, err = BoundedPathExists(t.TempDir())
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("the remaining budget caps a longer own bound", func(t *testing.T) {
		clear := SetOperationBudget(time.Now().Add(150 * time.Millisecond))
		defer clear()
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		start := time.Now()
		_, err := BoundedFSOp("stalled", func() (int, error) { <-release; return 1, nil })
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(start), 2*time.Second, "capped at the remaining budget, not the 10s own bound")
	})

	t.Run("no budget leaves the own bound alone", func(t *testing.T) {
		d, spent := RemainingBudget(7 * time.Second)
		assert.False(t, spent)
		assert.Equal(t, 7*time.Second, d)
	})
}

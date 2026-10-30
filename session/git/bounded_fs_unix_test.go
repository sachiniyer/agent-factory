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

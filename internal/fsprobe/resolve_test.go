package fsprobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// waitForResolveFlight waits until no ResolveForCompare flight for path is
// still running, so a test's seam is never restored under a live worker.
func waitForResolveFlight(t *testing.T, path string) {
	t.Helper()
	require.Eventually(t, func() bool {
		resolveForCompareFlights.Lock()
		defer resolveForCompareFlights.Unlock()
		return resolveForCompareFlights.byDir[path] == nil
	}, 5*time.Second, time.Millisecond)
}

// A bounded walk that answers is pathutil.ResolveForCompare's answer: a
// missing leaf behind a symlinked ancestor resolves the ancestor and keeps the
// leaf, so both spellings of one path compare equal (#2110), and a path with
// nothing resolvable is still cleaned.
func TestResolveForCompare_ReportsTheAnswer(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.Mkdir(real, 0o755))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(real, link))

	viaLink, err := ResolveForCompare(filepath.Join(link, "gone"))
	require.NoError(t, err)
	viaReal, err := ResolveForCompare(filepath.Join(real, "gone"))
	require.NoError(t, err)
	require.Equal(t, viaReal, viaLink, "both spellings of a missing leaf compare equal")
	require.Equal(t, pathutil.ResolveForCompare(filepath.Join(link, "gone")), viaLink, "the walk is pathutil's")
	require.Equal(t, "gone", filepath.Base(viaLink), "the missing leaf is kept")

	nowhere := filepath.Join(string(filepath.Separator), "definitely", "not", "here", "..", "here", "x")
	got, err := ResolveForCompare(nowhere)
	require.NoError(t, err)
	require.Equal(t, filepath.Clean(nowhere), got, "a path with nothing resolvable is cleaned")
}

// #7272: the launch dir's resolution on a wedged mount surfaces the same
// ErrProbeTimedOut + context.DeadlineExceeded pair as every probe here, within
// the bound, with no resolution; a retry while the stuck walk is still blocked
// is refused rather than started again.
func TestResolveForCompare_BoundsAWedgedWalk(t *testing.T) {
	release := make(chan struct{})
	dir := t.TempDir()
	restore := SetEvalSymlinksForTest(func(string) (string, error) {
		<-release
		return "", os.ErrNotExist
	})
	restoreTimeout := SetProbeTimeoutForTest(50 * time.Millisecond)
	defer func() {
		close(release)
		waitForResolveFlight(t, dir)
		restore()
		restoreTimeout()
	}()

	start := time.Now()
	resolved, err := ResolveForCompare(dir)
	require.Empty(t, resolved, "a walk that did not finish resolves nothing")
	require.True(t, errors.Is(err, context.DeadlineExceeded), "a wedged walk must surface typed deadline evidence: %v", err)
	require.True(t, errors.Is(err, ErrProbeTimedOut), "the deadline must carry the fsprobe sentinel: %v", err)
	require.Less(t, time.Since(start), 2*time.Second, "the caller was not held by the stalled walk")

	start = time.Now()
	_, err = ResolveForCompare(dir + string(filepath.Separator))
	require.True(t, errors.Is(err, ErrProbeTimedOut), "a retry of the same path while the walk is stuck is refused: %v", err)
	require.Less(t, time.Since(start), 40*time.Millisecond, "the refusal does not wait out another bound")
}

// The bound is one flight for the whole ancestor walk, not one per component:
// a mount slow enough that each lookup answers inside the bound, but the walk
// up a missing remainder does not, is a walk that did not answer.
func TestResolveForCompare_BoundsTheWholeWalk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c", "d")
	restore := SetEvalSymlinksForTest(func(string) (string, error) {
		time.Sleep(30 * time.Millisecond)
		return "", os.ErrNotExist
	})
	restoreTimeout := SetProbeTimeoutForTest(50 * time.Millisecond)
	defer func() {
		waitForResolveFlight(t, path)
		restore()
		restoreTimeout()
	}()

	_, err := ResolveForCompare(path)
	require.True(t, errors.Is(err, ErrProbeTimedOut),
		"each lookup answers within the bound, but the walk does not: %v", err)
}

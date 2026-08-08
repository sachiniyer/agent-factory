//go:build !windows

package git

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Retained hard-link readers must be bounded by the process's real descriptor
// limit, not only by a constant that may sit above it. The copy still has to
// retain readers (otherwise #3063 regresses), while leaving enough descriptors
// for traversal, source/destination opens, and the daemon's unrelated work.
func TestCopyTree_RetainedReadersHonorNoFileLimit(t *testing.T) {
	var original unix.Rlimit
	require.NoError(t, unix.Getrlimit(unix.RLIMIT_NOFILE, &original))
	if original.Cur < 64 {
		t.Skipf("RLIMIT_NOFILE=%d is too small for the bounded pressure fixture", original.Cur)
	}
	const limitedFDs = 64
	if alreadyOpen := countOpenFileDescriptors(limitedFDs); alreadyOpen > 20 {
		t.Skipf("test process already holds %d descriptors; cannot create a stable low-limit fixture", alreadyOpen)
	}

	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0o755))
	for index := range limitedFDs {
		first := filepath.Join(src, fmt.Sprintf("a-%03d", index))
		require.NoError(t, os.WriteFile(first, []byte("shared"), 0o600))
		require.NoError(t, os.Link(first, filepath.Join(src, fmt.Sprintf("z-%03d", index))))
	}
	require.NoError(t, os.WriteFile(filepath.Join(src, "m-observe"), []byte("marker"), 0o600))
	dest := filepath.Join(t.TempDir(), "dest")

	originalHook := copyTreeAfterSourceInspect
	defer func() { copyTreeAfterSourceInspect = originalHook }()
	initialOpen := 0
	openAtMarker := 0
	copyTreeAfterSourceInspect = func(path string) error {
		openNow := countOpenFileDescriptors(limitedFDs)
		if initialOpen == 0 {
			initialOpen = openNow
		}
		if filepath.Base(path) == "m-observe" {
			openAtMarker = openNow
		}
		return nil
	}

	limited := original
	limited.Cur = limitedFDs
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &limited))
	t.Cleanup(func() { _ = unix.Setrlimit(unix.RLIMIT_NOFILE, &original) })
	err := copyTree(src, dest)
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &original))

	require.NoError(t, err)
	require.Positive(t, initialOpen)
	require.Greater(t, openAtMarker, initialOpen,
		"the copy retained no readers, so unreadable hard-link groups would regress to the master behavior")
	const minimumSpare = 12
	require.GreaterOrEqual(t, limitedFDs-openAtMarker, minimumSpare,
		"retained readers consumed the low RLIMIT before the copier's fixed cap activated")
}

// Concurrent copies share one process descriptor table. A budget computed by
// each copy from the same snapshot double-spends the spare slots, leaving no
// room for the next source/destination open. The readers still need to be
// retained (otherwise #3063 regresses), but their combined total must preserve
// the process-wide reserve.
func TestCopyTree_ConcurrentCopiesShareRetainedReaderCapacity(t *testing.T) {
	var original unix.Rlimit
	require.NoError(t, unix.Getrlimit(unix.RLIMIT_NOFILE, &original))
	if original.Cur < 64 {
		t.Skipf("RLIMIT_NOFILE=%d is too small for the concurrent pressure fixture", original.Cur)
	}
	const limitedFDs = 64
	if alreadyOpen := countOpenFileDescriptors(limitedFDs); alreadyOpen > 20 {
		t.Skipf("test process already holds %d descriptors; cannot create a stable low-limit fixture", alreadyOpen)
	}

	type copyPaths struct{ source, destination string }
	copies := make([]copyPaths, 2)
	for copyIndex := range copies {
		source := filepath.Join(t.TempDir(), fmt.Sprintf("src-%d", copyIndex))
		require.NoError(t, os.Mkdir(source, 0o755))
		for linkIndex := range limitedFDs {
			first := filepath.Join(source, fmt.Sprintf("a-%03d", linkIndex))
			require.NoError(t, os.WriteFile(first, []byte("shared"), 0o600))
			require.NoError(t, os.Link(first, filepath.Join(source, fmt.Sprintf("z-%03d", linkIndex))))
		}
		require.NoError(t, os.WriteFile(filepath.Join(source, "m-observe"), []byte("marker"), 0o600))
		copies[copyIndex] = copyPaths{source: source, destination: filepath.Join(t.TempDir(), fmt.Sprintf("dest-%d", copyIndex))}
	}

	originalHook := copyTreeAfterSourceInspect
	defer func() { copyTreeAfterSourceInspect = originalHook }()
	var reachedFirst sync.WaitGroup
	reachedFirst.Add(len(copies))
	releaseFirst := make(chan struct{})
	var observedMu sync.Mutex
	maxOpenAtMarker := 0
	copyTreeAfterSourceInspect = func(path string) error {
		switch filepath.Base(path) {
		case "a-000":
			reachedFirst.Done()
			<-releaseFirst
		case "m-observe":
			openNow := countOpenFileDescriptors(limitedFDs)
			observedMu.Lock()
			if openNow > maxOpenAtMarker {
				maxOpenAtMarker = openNow
			}
			observedMu.Unlock()
		}
		return nil
	}

	limited := original
	limited.Cur = limitedFDs
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &limited))
	t.Cleanup(func() { _ = unix.Setrlimit(unix.RLIMIT_NOFILE, &original) })

	errors := make(chan error, len(copies))
	for _, paths := range copies {
		go func() { errors <- copyTree(paths.source, paths.destination) }()
	}
	reachedFirst.Wait()
	openBeforeReaders := countOpenFileDescriptors(limitedFDs)
	close(releaseFirst)
	copyErrors := make([]error, 0, len(copies))
	for range copies {
		copyErrors = append(copyErrors, <-errors)
	}
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &original))
	for _, copyErr := range copyErrors {
		require.NoError(t, copyErr)
	}

	require.Greater(t, maxOpenAtMarker, openBeforeReaders,
		"the concurrent copies retained no readers, so unreadable hard-link groups would regress to the master behavior")
	const minimumSpare = 12
	require.GreaterOrEqual(t, limitedFDs-maxOpenAtMarker, minimumSpare,
		"concurrent copies consumed overlapping descriptor budgets")
}

func countOpenFileDescriptors(limit int) int {
	count := 0
	for fd := range limit {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			count++
		}
	}
	return count
}

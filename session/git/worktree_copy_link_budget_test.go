//go:build !windows

package git

import (
	"fmt"
	"os"
	"path/filepath"
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

func countOpenFileDescriptors(limit int) int {
	count := 0
	for fd := range limit {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			count++
		}
	}
	return count
}

package git

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// A filesystem timestamp beyond Go's UnixNano range must survive the
// cross-device copy, or be refused — never silently wrapped.
//
// time.Time.UnixNano() is only defined for 1678–2262 and wraps outside it, so
// the first spelling of preserveSourceModTime reproduced a far-future node
// centuries off while reporting success. ext4 with 256-byte inodes stores
// mtimes to 2446, so this is representable, not hypothetical: the copy claimed
// to preserve a timestamp it had corrupted.
//
// The rename path is the oracle, exactly as the fidelity guard uses it — it
// never reads the file, so it carries any value the filesystem can hold.
func TestMoveDirCrossDevice_FarFutureMtimeIsNotWrapped(t *testing.T) {
	// 2300-01-01T00:00:00Z — inside ext4's range, outside UnixNano's.
	const farFutureSec = 10413792000
	if _, err := unix.TimeToTimespec(time.Unix(farFutureSec, 0)); err != nil {
		t.Skipf("this platform cannot represent the probe timestamp: %v", err)
	}

	workspace := t.TempDir()
	src := filepath.Join(workspace, "src")
	require.NoError(t, os.MkdirAll(src, 0o700))
	probe := filepath.Join(src, "far-future.txt")
	require.NoError(t, os.WriteFile(probe, []byte("x"), 0o600))

	// Set it through unix directly: os.Chtimes goes via UnixNano and would wrap
	// the fixture itself, which would make this test pass for the wrong reason.
	stamp := unix.Timespec{Sec: farFutureSec}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, probe, []unix.Timespec{stamp, stamp}, 0); err != nil {
		t.Skipf("this filesystem cannot store a 2300 mtime: %v", err)
	}
	// Read back, and SKIP rather than fail if the filesystem did not keep it.
	// utimensat reports success on APFS and then clamps to 9223372036 — which is
	// exactly UnixNano's own ceiling (2262-04-11), so on macOS the wrap this test
	// is about is not reachable: the filesystem cannot hold a value outside the
	// range that wraps. Checking the WRITE succeeded is not enough; only the
	// read-back says whether the fixture carries what the test needs.
	sourceInfo, err := os.Lstat(probe)
	require.NoError(t, err)
	if sourceInfo.ModTime().Unix() != farFutureSec {
		t.Skipf("this filesystem clamped the probe mtime to %d, so an out-of-UnixNano-range value cannot be stored here",
			sourceInfo.ModTime().Unix())
	}

	original := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = original })

	dest := filepath.Join(workspace, "dest")
	require.NoError(t, moveDirCrossDevice(src, dest))

	copiedInfo, err := os.Lstat(filepath.Join(dest, "far-future.txt"))
	require.NoError(t, err)
	require.Equal(t, int64(farFutureSec), copiedInfo.ModTime().Unix(),
		"the copy must reproduce the source mtime, not a value wrapped through UnixNano")
}

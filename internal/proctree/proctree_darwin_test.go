//go:build darwin

package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// These tests are the RUNTIME half of #2050's verification, and they exist
// because the other half cannot be done anywhere but here.
//
// vnodepathinfo_test.go proves the decode and the struct offsets on every
// platform, but no amount of Linux CI can prove the syscall itself: that
// PROC_PIDVNODEPATHINFO is flavor 9, that the kernel writes 2352 bytes, that
// pvi_cdir really is the current directory and not the root directory. Those are
// facts about a running XNU kernel. So they are written down as tests that
// execute the moment anyone runs this suite on a Mac, rather than as a paragraph
// asking a reviewer to check by hand.
//
// If the backend were wrong, this is what would catch it: a bad flavor or size
// fails the syscall (every assertion below goes false), and reading pvi_rdir
// instead of pvi_cdir returns "/" — which TestReadWorkingDir_FollowsChdir
// rejects specifically.

// resolved matches what the kernel reports: vn_getpath returns the real path, so
// /tmp comes back as /private/tmp. Comparing raw strings would fail on macOS for
// that reason alone.
func resolved(t *testing.T, path string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return r
}

// TestReadWorkingDir_MatchesOwnCwd is the load-bearing one: the kernel's answer
// for this process must equal the working directory this process knows it has.
func TestReadWorkingDir_MatchesOwnCwd(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)

	got, ok := readWorkingDir(os.Getpid())
	require.True(t, ok, "readWorkingDir must resolve our OWN pid — if this is false the "+
		"syscall failed outright (wrong flavor, wrong buffer size, or an unexpected refusal)")
	assert.Equal(t, resolved(t, wd), resolved(t, got))
}

// TestReadWorkingDir_FollowsChdir proves we read pvi_cdir and not pvi_rdir. A
// mis-declared offset that landed on the root directory would return "/" here and
// still have passed a laxer version of the test above on a process started at /.
func TestReadWorkingDir_FollowsChdir(t *testing.T) {
	orig, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.Chdir(orig)) })

	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))

	got, ok := readWorkingDir(os.Getpid())
	require.True(t, ok)
	assert.Equal(t, resolved(t, dir), resolved(t, got))
	assert.NotEqual(t, "/", got, "returning the root directory means pvi_rdir is being read instead of pvi_cdir")
}

// TestReadWorkingDir_UnknownForDeadPid pins the honest-unknown contract on the
// failure side: a pid that names nothing must report false, never a stale or
// fabricated path. reapWorktreeWriters signals what this returns.
func TestReadWorkingDir_UnknownForDeadPid(t *testing.T) {
	// A pid we can be confident is not live: allocate one, then reap it.
	cmd := exec.Command("/usr/bin/true")
	require.NoError(t, cmd.Run())
	dead := cmd.Process.Pid

	got, ok := readWorkingDir(dead)
	assert.False(t, ok, "a reaped pid has no working directory to report")
	assert.Empty(t, got)
}

// TestReadWorkingDir_ForeignProcessIsUnknownNotWrong checks the refusal path.
// pid 1 (launchd) is root-owned, so an unprivileged run must be REFUSED rather
// than answered. Whatever comes back, it must never be a path we then treat as a
// match — the fabricated-negative/positive rule this package exists for.
func TestReadWorkingDir_ForeignProcessIsUnknownNotWrong(t *testing.T) {
	got, ok := readWorkingDir(1)
	if os.Geteuid() == 0 {
		t.Skip("running as root, so pid 1 is not a foreign process and nothing is refused")
	}
	if ok {
		// Not a failure per se — Apple may disclose it — but it must at least be a
		// real absolute path rather than decoded garbage.
		assert.True(t, filepath.IsAbs(got), "any disclosed cwd must be absolute, got %q", got)
	} else {
		assert.Empty(t, got)
	}
}

// TestOpenWorkingDir_AcceptsOwnCwd is the end-to-end half of #3525: the TOCTOU
// check must ACCEPT a directory that really is the process's cwd.
//
// It is the guard against getting the fix backwards. A mask that rejected
// everything would leave the darwin reap a permanent no-op — the same
// user-visible outcome as the bug, on every filesystem instead of some — and
// the synthesized-Stat_t tests below would not notice, because they never call
// fstat at all.
func TestOpenWorkingDir_AcceptsOwnCwd(t *testing.T) {
	orig, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.Chdir(orig)) })

	dir := t.TempDir()
	require.NoError(t, os.Chdir(dir))

	handle, path, ok := openWorkingDir(os.Getpid())
	require.True(t, ok, "openWorkingDir must resolve AND accept our own cwd; false here "+
		"means the identity check rejected a directory that is genuinely the one the kernel named")
	t.Cleanup(func() { _ = handle.Close() })
	assert.Equal(t, resolved(t, dir), resolved(t, path))
}

// TestCwdIdentityMatchesStat stages the condition #3525 is about, which cannot
// be produced by mounting anything on a CI runner: a device id whose high bit is
// set. The Stat_t is synthesized, so this proves the COMPARISON rather than the
// syscall — the syscall's half is TestOpenWorkingDir_AcceptsOwnCwd above.
//
// The values are the ones the two sources really would report for one such
// filesystem: dev_t is int32, so fstat hands back -2147483647 for the device the
// kernel's uint32_t vst_dev reports as 0x80000001.
func TestCwdIdentityMatchesStat(t *testing.T) {
	const (
		kernelDevice = uint64(0x80000001) // vst_dev, zero-extended from uint32_t
		kernelInode  = uint64(0x1_0000_0042)
		kernelMode   = uint32(unix.S_IFDIR | 0o755)
	)
	// matching returns the fstat result for that same directory.
	matching := func() unix.Stat_t {
		var stat unix.Stat_t
		stat.Dev = -2147483647 // the int32 bit pattern of 0x80000001
		stat.Ino = kernelInode
		stat.Mode = unix.S_IFDIR | 0o755
		return stat
	}

	t.Run("a high-bit device id matches itself", func(t *testing.T) {
		stat := matching()
		require.Equal(t, uint64(0xffffffff80000001), uint64(stat.Dev),
			"precondition: this Stat_t must be one a plain uint64() sign-extends, "+
				"or the test is not staging #3525 at all")
		assert.True(t, cwdIdentityMatchesStat(&stat, kernelDevice, kernelInode, kernelMode),
			"the process on this filesystem was SKIPPED before #3525 was fixed")
	})

	t.Run("an ordinary device id still matches", func(t *testing.T) {
		stat := matching()
		stat.Dev = 0x01000005
		assert.True(t, cwdIdentityMatchesStat(&stat, 0x01000005, kernelInode, kernelMode))
	})

	// The mask must not become "accept anything". Each of these is a real
	// replacement the check exists to catch between proc_info and open(2).
	t.Run("a genuinely different device is still refused", func(t *testing.T) {
		stat := matching()
		stat.Dev = -2147483646 // 0x80000002: adjacent minor, same major
		assert.False(t, cwdIdentityMatchesStat(&stat, kernelDevice, kernelInode, kernelMode))
	})

	t.Run("the truncated-away bits cannot be forged back in", func(t *testing.T) {
		// 0x80000001 and 0xffffffff80000001 must not be treated as one device by
		// a caller holding the sign-extended value: dev_t cannot hold the latter,
		// so a kernel reporting it is not describing this vnode.
		stat := matching()
		assert.False(t, cwdIdentityMatchesStat(&stat, 0xffffffff80000001, kernelInode, kernelMode))
	})

	t.Run("a different inode is still refused", func(t *testing.T) {
		stat := matching()
		stat.Ino = kernelInode + 1
		assert.False(t, cwdIdentityMatchesStat(&stat, kernelDevice, kernelInode, kernelMode))
	})

	t.Run("a file where a directory was named is still refused", func(t *testing.T) {
		stat := matching()
		stat.Mode = unix.S_IFREG | 0o755
		assert.False(t, cwdIdentityMatchesStat(&stat, kernelDevice, kernelInode, kernelMode))
	})

	t.Run("permission bits are not part of the identity", func(t *testing.T) {
		// Only S_IFMT is compared: a chmod between the two reads is not a
		// replacement, and treating it as one would skip the process.
		stat := matching()
		stat.Mode = unix.S_IFDIR | 0o700
		assert.True(t, cwdIdentityMatchesStat(&stat, kernelDevice, kernelInode, kernelMode))
	})
}

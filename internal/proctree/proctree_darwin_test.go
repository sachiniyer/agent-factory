//go:build darwin

package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

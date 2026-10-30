package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Acquiring the lock never fails because of another writer: a transient ENOENT
// or EINTR from the O_CREAT open is retried, while any other error, and an ENOENT
// that never clears, still surface.
func TestOpenLockWithRetry(t *testing.T) {
	prevN, prevB := lockOpenAttempts, lockOpenBackoff
	lockOpenAttempts, lockOpenBackoff = 5, time.Microsecond
	t.Cleanup(func() { lockOpenAttempts, lockOpenBackoff = prevN, prevB })

	for _, tc := range []struct {
		name      string
		failures  []error
		wantCalls int
		wantErr   error
	}{
		{name: "first try", wantCalls: 1},
		{name: "transient ENOENT then success", failures: []error{syscall.ENOENT, syscall.ENOENT}, wantCalls: 3},
		{name: "transient EINTR then success", failures: []error{syscall.EINTR}, wantCalls: 2},
		{name: "permission error is not retried", failures: []error{syscall.EACCES}, wantCalls: 1, wantErr: syscall.EACCES},
		{name: "ENOENT that never clears fails after the budget", failures: []error{syscall.ENOENT, syscall.ENOENT, syscall.ENOENT, syscall.ENOENT, syscall.ENOENT, syscall.ENOENT}, wantCalls: 5, wantErr: syscall.ENOENT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err := openLockWithRetry("x.lock", func() (*os.File, error) {
				calls++
				if calls <= len(tc.failures) {
					return nil, &os.PathError{Op: "open", Path: "x.lock", Err: tc.failures[calls-1]}
				}
				return nil, nil
			}, nil)
			require.Equal(t, tc.wantCalls, calls)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.True(t, errors.Is(err, tc.wantErr), "got %v", err)
		})
	}
}

// A pinned directory that is REPLACED (removed and recreated under the same name:
// `stow -D && stow`, a restored home) makes openat(O_CREAT) through the stale fd
// report ENOENT forever. The lock must re-pin from the path and acquire in the
// directory the path now names, and the body must see that directory.
func TestWithFollowedFileLock_RepinsAReplacedDirectory(t *testing.T) {
	prevN, prevB := lockOpenAttempts, lockOpenBackoff
	lockOpenAttempts, lockOpenBackoff = 5, time.Microsecond
	t.Cleanup(func() { lockOpenAttempts, lockOpenBackoff = prevN, prevB })

	root := t.TempDir()
	dir := filepath.Join(root, "home")
	require.NoError(t, os.Mkdir(dir, 0o755))
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("x = 1\n"), 0o644))

	// The window between pinning and waiting for the lock — where a peer's
	// stow/chezmoi run replaces the directory.
	followedLockRaceHookForTest = func() {
		require.NoError(t, os.RemoveAll(dir))
		require.NoError(t, os.Mkdir(dir, 0o755))
		require.NoError(t, os.WriteFile(path, []byte("x = 2\n"), 0o644))
	}
	t.Cleanup(func() { followedLockRaceHookForTest = nil })

	ran := false
	err := withFollowedFileLock(path, func(locked lockedTarget) error {
		ran = true
		var pinned, now unix.Stat_t
		require.NoError(t, unix.Fstat(locked.dirFD, &pinned))
		require.NoError(t, unix.Stat(dir, &now))
		require.True(t, sameInode(&pinned, &now), "the body must see the directory the path now names")
		return nil
	})
	require.NoError(t, err)
	require.True(t, ran, "the lock must still be acquired after the directory was replaced")
	_, statErr := os.Stat(path + ".lock")
	require.NoError(t, statErr, "the lock file lives in the new directory")
}

// A symlinked home retargeted from A to B, with A removed before the lock is
// opened, must be refused: re-pinning from the path would silently adopt B and
// confirmDir (B against B) would pass (#6130).
func TestWithFollowedFileLock_RefusesARetargetedHomeLink(t *testing.T) {
	prevN, prevB := lockOpenAttempts, lockOpenBackoff
	lockOpenAttempts, lockOpenBackoff = 5, time.Microsecond
	t.Cleanup(func() { lockOpenAttempts, lockOpenBackoff = prevN, prevB })

	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{a, b} {
		require.NoError(t, os.Mkdir(d, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(d, "config.toml"), []byte("x = 1\n"), 0o644))
	}
	link := filepath.Join(root, "home")
	require.NoError(t, os.Symlink(a, link))
	path := filepath.Join(link, "config.toml")

	followedLockRaceHookForTest = func() {
		require.NoError(t, os.Remove(link))
		require.NoError(t, os.Symlink(b, link))
		require.NoError(t, os.RemoveAll(a))
	}
	t.Cleanup(func() { followedLockRaceHookForTest = nil })

	ran := false
	err := withFollowedFileLock(path, func(lockedTarget) error { ran = true; return nil })
	require.Error(t, err)
	require.False(t, ran, "a retargeted home must not run the operation")
	require.Contains(t, err.Error(), "moved")
	_, statErr := os.Stat(filepath.Join(b, "config.toml.lock"))
	require.True(t, os.IsNotExist(statErr), "no lock file may be created in the new destination")
}

package git

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the cross-device tree copier in worktree_copy_tree.go. Its identity
// bookkeeping and cleanup paths are covered by worktree_copy_cleanup_test.go,
// and the move/publication flow around it by worktree_archive_test.go.

// TestCopyTree_PreservesModesAndSymlinks unit-tests the cross-device copy engine
// (the EXDEV fallback path can't be forced with a real second filesystem in a
// hermetic test): file contents, permission bits, nested dirs, and symlinks must
// all round-trip.
func TestCopyTree_PreservesModesAndSymlinks(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0640))
	require.NoError(t, os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("beta"), 0600))
	require.NoError(t, os.Symlink("a.txt", filepath.Join(src, "link")))

	dest := filepath.Join(t.TempDir(), "dest")
	require.NoError(t, copyTree(src, dest))

	a, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "alpha", string(a))
	b, err := os.ReadFile(filepath.Join(dest, "sub", "b.txt"))
	require.NoError(t, err)
	assert.Equal(t, "beta", string(b))

	aInfo, err := os.Stat(filepath.Join(dest, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0640), aInfo.Mode().Perm(), "file permission bits must be preserved")

	linkTarget, err := os.Readlink(filepath.Join(dest, "link"))
	require.NoError(t, err, "symlink must be copied as a link, not followed")
	assert.Equal(t, "a.txt", linkTarget)
}

// TestCopyTree_AllowsSymlinkedDestinationParent preserves configured layouts
// where (for example) $AGENT_FACTORY_HOME/worktrees is intentionally a symlink
// to another filesystem. The copy must anchor itself to the resolved directory
// descriptor without rejecting that parent symlink.
func TestCopyTree_AllowsSymlinkedDestinationParent(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "tracked.txt"), []byte("tracked"), 0644))

	realParent := filepath.Join(t.TempDir(), "real-parent")
	require.NoError(t, os.Mkdir(realParent, 0755))
	linkedParent := filepath.Join(t.TempDir(), "linked-parent")
	require.NoError(t, os.Symlink(realParent, linkedParent))
	dest := filepath.Join(linkedParent, "dest")

	require.NoError(t, copyTree(src, dest))
	contents, err := os.ReadFile(filepath.Join(realParent, "dest", "tracked.txt"))
	require.NoError(t, err)
	assert.Equal(t, "tracked", string(contents))
}

// TestCopyTree_RejectsNamedPipeWithoutBlocking is the #2654 regression. The
// cross-device move fallback copies a worktree node by node; opening a FIFO as
// though it were a regular file waits for a writer forever. Special files must
// instead fail promptly so ArchiveSession can release its operation/kill guards.
//
// PRE-FIX: copyTree does not return before the deadline. The bounded, nonblocking
// cleanup below attempts to release it solely so the test can report the hang.
func TestCopyTree_RejectsNamedPipeWithoutBlocking(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(src, 0755))
	fifo := filepath.Join(src, "events.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0600))

	dest := filepath.Join(t.TempDir(), "dest")
	assertNamedPipeCopyFailsPromptly(t, fifo, func() error {
		return copyTree(src, dest)
	})
}

// TestCopyFile_RejectsNamedPipeRaceWithoutBlocking closes the Lstat/open race in
// #2689's first fix. A worktree process can replace a path after copyTree sees a
// regular file but before copyFile opens it; copyFile must validate the object it
// actually opened without ever making a blocking FIFO open.
//
// PRE-FIX: copyFile blocks until the helper supplies a writer, then returns nil.
func TestCopyFile_RejectsNamedPipeRaceWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "events.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0600))
	dest := filepath.Join(t.TempDir(), "dest")

	assertNamedPipeCopyFailsPromptly(t, fifo, func() error {
		return copyFile(fifo, dest)
	})
}

// TestCopyTree_RejectsDirectoryToNamedPipeRaceWithoutBlocking covers the
// traversal side of the metadata/open race. filepath.Walk inspects a directory
// and then opens that pathname with a blocking call before invoking its callback.
// The seam forces the equivalent inspect/open boundary in the descriptor walker:
// replacing the directory with a FIFO there must be rejected without blocking.
func TestCopyTree_RejectsDirectoryToNamedPipeRaceWithoutBlocking(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dir := filepath.Join(src, "sub")
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked"), 0644))

	originalHook := copyTreeBeforeSourceOpen
	t.Cleanup(func() { copyTreeBeforeSourceOpen = originalHook })
	swapped := false
	copyTreeBeforeSourceOpen = func(path string) error {
		if path != dir || swapped {
			return nil
		}
		swapped = true
		if err := os.Rename(dir, dir+".original"); err != nil {
			return err
		}
		return syscall.Mkfifo(dir, 0600)
	}

	dest := filepath.Join(t.TempDir(), "dest")
	assertNamedPipeCopyFailsPromptly(t, dir, func() error {
		return copyTree(src, dest)
	})
}

// TestCopyTree_RejectsRegularToNamedPipeRaceWithoutBlocking is the #2708
// regression, driven through the production walker. Traversal classifies each
// entry from a stat of its NAME, then copies the object it opens under that name
// a moment later; a process still writing to the worktree can swap the two. A
// copier that trusts the earlier classification opens a FIFO as a regular file
// and waits for a writer that never comes — the indefinite archive hang, with
// the session's operation and kill guards held for the whole wait.
//
// The seam substitutes the node in exactly that window, so the stale "regular"
// verdict reaches the file copier and only descriptor-level validation can
// reject it. The whole copy, not just the failing entry, must return promptly.
func TestCopyTree_RejectsRegularToNamedPipeRaceWithoutBlocking(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(src, 0755))
	raced := filepath.Join(src, "tracked.txt")
	require.NoError(t, os.WriteFile(raced, []byte("tracked"), 0644))

	originalHook := copyTreeAfterSourceInspect
	t.Cleanup(func() { copyTreeAfterSourceInspect = originalHook })
	swapped := false
	copyTreeAfterSourceInspect = func(path string) error {
		if path != raced || swapped {
			return nil
		}
		swapped = true
		if err := os.Remove(path); err != nil {
			return err
		}
		return syscall.Mkfifo(path, 0600)
	}

	dest := filepath.Join(t.TempDir(), "dest")
	assertNamedPipeCopyFailsPromptly(t, raced, func() error {
		return copyTree(src, dest)
	})
	assert.True(t, swapped, "the test must reach the inspect/open window it covers")
}

// TestCopyFile_DoesNotFollowReplacementSymlink covers a regular path replaced
// by a symlink between traversal metadata and open. The source open must reject
// the link rather than archiving contents from outside the worktree.
func TestCopyFile_DoesNotFollowReplacementSymlink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside-secret")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0600))
	src := filepath.Join(t.TempDir(), "raced-source")
	require.NoError(t, os.Symlink(outside, src))
	dest := filepath.Join(t.TempDir(), "dest")

	err := copyFile(src, dest)
	require.Error(t, err, "a replacement symlink must not be followed")
	assert.Contains(t, err.Error(), src)
	assert.NoFileExists(t, dest)
}

// TestCopyFile_RejectsRacedInDestinationNodes proves a destination node that
// appears after the initial absence check is never opened. A symlink must not
// redirect/truncate another file, and a FIFO must not block waiting for a reader.
func TestCopyFile_RejectsRacedInDestinationNodes(t *testing.T) {
	src := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(src, []byte("new contents"), 0644))

	t.Run("symlink", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "outside")
		require.NoError(t, os.WriteFile(outside, []byte("keep me"), 0600))
		dest := filepath.Join(t.TempDir(), "raced-destination")
		require.NoError(t, os.Symlink(outside, dest))

		err := copyFile(src, dest)
		require.Error(t, err, "a raced-in destination symlink must be rejected")
		contents, readErr := os.ReadFile(outside)
		require.NoError(t, readErr)
		assert.Equal(t, "keep me", string(contents), "the symlink target must not be truncated")
	})

	t.Run("named pipe", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "raced-destination.fifo")
		require.NoError(t, syscall.Mkfifo(dest, 0600))
		assertNamedPipeDestinationFailsPromptly(t, dest, func() error {
			return copyFile(src, dest)
		})
	})
}

func assertNamedPipeCopyFailsPromptly(t *testing.T, fifo string, copyFn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- copyFn()
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a FIFO cannot be copied as a regular file")
		assert.Contains(t, err.Error(), "cannot move worktree across filesystems")
		assert.Contains(t, err.Error(), fifo)
	case <-time.After(500 * time.Millisecond):
		fd, unblockErr := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if unblockErr == nil {
			unblockErr = syscall.Close(fd)
		}
		select {
		case eventualErr := <-done:
			t.Fatalf("HUNG: copy blocked opening named pipe %s; nonblocking cleanup returned %v and the copy eventually returned %v", fifo, unblockErr, eventualErr)
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("HUNG: copy blocked opening named pipe %s and did not return after bounded nonblocking cleanup (%v)", fifo, unblockErr)
		}
	}
}

func assertNamedPipeDestinationFailsPromptly(t *testing.T, fifo string, copyFn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- copyFn()
	}()

	select {
	case err := <-done:
		require.Error(t, err, "an existing destination FIFO must be rejected")
		assert.Contains(t, err.Error(), fifo)
	case <-time.After(500 * time.Millisecond):
		fd, unblockErr := syscall.Open(fifo, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if unblockErr != nil {
			t.Fatalf("HUNG: copy blocked opening destination named pipe %s and bounded cleanup could not open a reader: %v", fifo, unblockErr)
		}
		defer syscall.Close(fd)
		select {
		case eventualErr := <-done:
			t.Fatalf("HUNG: copy blocked opening destination named pipe %s; after bounded cleanup supplied a reader, copy returned %v", fifo, eventualErr)
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("HUNG: copy blocked opening destination named pipe %s and did not return after bounded cleanup", fifo)
		}
	}
}

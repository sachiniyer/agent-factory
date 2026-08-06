package git

import (
	"fmt"
	"io/fs"
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

// TestCopyTree_PreservesModesTheUmaskWouldAlter is the #2869 regression. The
// destination nodes are created with mkdirat(2)/openat(2), and both subtract the
// process umask from the mode they are handed — so the copier silently tightened
// exactly the permissions its doc comment promises to preserve, while the
// same-device rename(2) path kept them verbatim. The existing coverage above
// missed it because 0755 dirs and 0640 files survive a typical umask 0022
// untouched; every probe here is a mode a umask does alter.
func TestCopyTree_PreservesModesTheUmaskWouldAlter(t *testing.T) {
	withUmask(t, 0077)
	src := filepath.Join(t.TempDir(), "src")
	want := writeModeProbeTree(t, src)

	dest := filepath.Join(t.TempDir(), "dest")
	require.NoError(t, copyTree(src, dest))

	assert.Equal(t, want, collectTreeDescription(t, dest),
		"the cross-device copy must reproduce the source modes, not the umask's opinion of them")
}

// withUmask sets the process umask for the duration of one test. The umask is
// process-wide, so this is only confined to the calling test because nothing in
// this package calls t.Parallel().
func withUmask(t *testing.T, mask int) {
	t.Helper()
	original := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(original) })
}

// writeModeProbeTree builds a fixture whose permission bits a umask does alter,
// and returns the description collectTreeDescription must produce for a faithful
// copy of it.
//
// Every node is chmod'd explicitly after it is created because os.Mkdir and
// os.WriteFile subtract the umask themselves: without the chmod the fixture
// would be born already tightened, the copy would match it, and the assertion
// would pass vacuously — which is how this bug survived its own test.
func writeModeProbeTree(t *testing.T, root string) map[string]string {
	t.Helper()
	directories := map[string]os.FileMode{
		".":   0750, // a group-shared worktree: umask 0077 drops group access
		"sub": 0777,
	}
	files := map[string]os.FileMode{
		"run.sh":         0755, // an executable hook: umask 0077 drops group/other +x
		"shared.txt":     0664,
		"sub/secret.key": 0600, // control: no umask widens, so this one must not move
	}
	require.NoError(t, os.Mkdir(root, 0700))
	require.NoError(t, os.Mkdir(filepath.Join(root, "sub"), 0700))
	for relative := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, relative), []byte(relative), 0600))
	}
	require.NoError(t, os.Symlink("run.sh", filepath.Join(root, "link")))

	want := map[string]string{"link": "symlink -> run.sh"}
	for relative, mode := range files {
		require.NoError(t, os.Chmod(filepath.Join(root, relative), mode))
		want[relative] = fmt.Sprintf("file %04o", mode)
	}
	// Directories last: the root drops to 0750 here, and the writes above need
	// it traversable while they run.
	for relative, mode := range directories {
		require.NoError(t, os.Chmod(filepath.Join(root, relative), mode))
		want[relative] = fmt.Sprintf("dir %04o", mode)
	}
	return want
}

// collectTreeDescription renders a tree as relative path → type and permission
// bits, so a mismatch reports every node at once instead of the first one.
// Symlinks are described by their target: their own mode bits are not portable
// (Linux fixes them at 0777, other kernels apply the umask) and nothing reads
// them, so asserting on those would pin the platform rather than the copier.
func collectTreeDescription(t *testing.T, root string) map[string]string {
	t.Helper()
	described := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			described[relative] = "symlink -> " + target
		case info.IsDir():
			described[relative] = fmt.Sprintf("dir %04o", info.Mode().Perm())
		default:
			described[relative] = fmt.Sprintf("file %04o", info.Mode().Perm())
		}
		return nil
	}))
	return described
}

// TestCopyTree_PreservesSparseFilesInsteadOfExpandingThem is the #2920
// regression. io.Copy reads a hole as zeros and writes them out, so the
// cross-device fallback allocated a real block for every hole while the
// same-device rename(2) left the extent layout untouched. The archive root is
// shared by every session on the box, so one session archiving a sparsely
// allocated database or image file could consume the space all of them need.
//
// The fixture has a leading hole, data in the middle, and a trailing hole — the
// trailing one is what catches a copy that skips the final zero chunk and then
// forgets to size the file.
func TestCopyTree_PreservesSparseFilesInsteadOfExpandingThem(t *testing.T) {
	const size = 32 << 20
	const payload = "sparse payload marker"
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	sparse, err := os.Create(filepath.Join(src, "database.bin"))
	require.NoError(t, err)
	require.NoError(t, sparse.Truncate(size))
	_, err = sparse.WriteAt([]byte(payload), size/2)
	require.NoError(t, err)
	require.NoError(t, sparse.Close())
	// A dense file alongside it: the zero-skipping must not disturb ordinary
	// content, including embedded NUL bytes.
	dense := []byte("head\x00\x00\x00tail")
	require.NoError(t, os.WriteFile(filepath.Join(src, "dense.bin"), dense, 0644))

	sourceBlocks := allocatedBlocks(t, filepath.Join(src, "database.bin"))

	dest := filepath.Join(t.TempDir(), "dest")
	require.NoError(t, copyTree(src, dest))
	copiedPath := filepath.Join(dest, "database.bin")

	// Contents first, and unconditionally: what the copy must never do is change
	// a byte, and that holds on every filesystem. Only the allocation half below
	// depends on the filesystem being able to express a hole at all.
	t.Run("contents survive the zero-run skipping", func(t *testing.T) {
		info, err := os.Stat(copiedPath)
		require.NoError(t, err)
		assert.Equal(t, int64(size), info.Size(),
			"the trailing hole must still count toward the file's length")

		copiedContents, err := os.ReadFile(copiedPath)
		require.NoError(t, err)
		require.Len(t, copiedContents, size)
		assert.Equal(t, payload, string(copiedContents[size/2:size/2+len(payload)]))
		assert.Equal(t, -1, firstNonZero(copiedContents[:size/2]), "the leading hole must read back as zeros")
		assert.Equal(t, -1, firstNonZero(copiedContents[size/2+len(payload):]), "the trailing hole must read back as zeros")

		copiedDense, err := os.ReadFile(filepath.Join(dest, "dense.bin"))
		require.NoError(t, err)
		assert.Equal(t, dense, copiedDense, "embedded NUL bytes must survive the zero-run skipping")
	})

	t.Run("holes are reproduced rather than allocated", func(t *testing.T) {
		// Whether ftruncate leaves a hole is the filesystem's call, not ours: it
		// does on ext4, and the macOS runner's does not. Skip rather than fail
		// where the fixture never became sparse — the assertion could only
		// compare a dense file against itself and would pass vacuously, which is
		// worse than not running. The subtest keeps that visible in CI instead of
		// hiding it inside a green parent.
		if sourceBlocks >= 4096 {
			t.Skipf("filesystem did not make the fixture sparse (%d KiB allocated for a %d MiB file), "+
				"so there is no hole here to preserve or lose", sourceBlocks/2, size>>20)
		}
		copiedBlocks := allocatedBlocks(t, copiedPath)
		assert.Less(t, copiedBlocks, sourceBlocks+4096,
			"the copy allocated the holes instead of reproducing them: %d KiB on disk vs the source's %d KiB",
			copiedBlocks/2, sourceBlocks/2)
	})
}

// firstNonZero returns the index of the first non-zero byte, or -1. It exists so
// the copy is verified independently of the production isAllZero that decided
// what to write: sharing that helper would make a bug in it corrupt the copy and
// the assertion the same way, and the test would confirm itself. Returning the
// index rather than a bool also names WHERE a hole was dirtied.
func firstNonZero(chunk []byte) int {
	for i, b := range chunk {
		if b != 0 {
			return i
		}
	}
	return -1
}

// allocatedBlocks reports a file's on-disk allocation in 512-byte units, which
// is what distinguishes a hole from a written run of zeros — st_size cannot.
func allocatedBlocks(t *testing.T, path string) int64 {
	t.Helper()
	var stat syscall.Stat_t
	require.NoError(t, syscall.Stat(path, &stat))
	return stat.Blocks
}

// TestLinkCopiedFile_RefusesALinkThatLandedOnAnotherInode covers the safety
// check behind the hard-link reuse in #2919.
//
// linkat() resolves a PATHNAME, and the staging tree — unguessably named but
// still same-UID reachable — can have that name swapped between the first copy
// and the link. Recording whatever inode results would make the injected one the
// manifest's expected identity, so both later validations would agree with each
// other and a corrupted tree would publish. The identity captured when the bytes
// were first written must therefore be compared, not merely stored.
//
// Driven directly rather than through a raced copy: the point under test is that
// the comparison exists and refuses, which a mismatched expectation shows without
// having to win a race.
func TestLinkCopiedFile_RefusesALinkThatLandedOnAnotherInode(t *testing.T) {
	staging := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(staging, "first.txt"), []byte("first"), 0644))

	root, _, err := openDirectoryPath(staging, "destination")
	require.NoError(t, err)
	defer root.Close()

	actual, err := identityAt(root, "first.txt")
	require.NoError(t, err)

	// What a swapped pathname looks like from here: the expectation names an
	// inode the link will not land on.
	impostor := actual
	impostor.inode++

	entry, err := linkCopiedFile(root, root, copiedFileLink{path: "first.txt", identity: impostor},
		"second.txt", filepath.Join(staging, "second.txt"), actual)
	require.Error(t, err, "a link that landed on a different inode must be refused")
	assert.Contains(t, err.Error(), "resolved to a different inode")
	assert.Equal(t, "second.txt", entry.name,
		"the entry must still be named so cleanup can remove what was created")
	assert.Equal(t, actual, entry.destination,
		"cleanup matches on the OBSERVED identity, so that is what the manifest must carry")

	// And the honest control: the same call with the right expectation succeeds.
	require.NoError(t, os.Remove(filepath.Join(staging, "second.txt")))
	_, err = linkCopiedFile(root, root, copiedFileLink{path: "first.txt", identity: actual},
		"third.txt", filepath.Join(staging, "third.txt"), actual)
	require.NoError(t, err, "a link onto the recorded inode must be accepted")
}

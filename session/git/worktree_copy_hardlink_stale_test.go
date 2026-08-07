package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// #3046: hard-link reproduction remembers an inode from a first sighting and
// links to the copy already made for it on a second. pathIdentity is
// {device, inode, fileType} and excludes size and times, so a file REWRITTEN IN
// PLACE between the two sightings keeps the same identity — the map hits, and
// the second path receives a link to the first sighting's content.
//
// The archive then holds bytes that never existed at that path. Worse, the two
// paths share an inode, so they cannot diverge on restore either.
//
// Asserted on the SECOND path only. The first path legitimately carries the old
// bytes — it was copied before the write — and a test requiring both paths to
// match would pin the wrong invariant and pass on the broken code.
func TestCopyTree_HardLinkDoesNotReproduceStaleContent(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "copy")

	firstPath := filepath.Join(source, "a.txt")
	secondPath := filepath.Join(source, "b.txt")
	require.NoError(t, os.WriteFile(firstPath, []byte("ORIGINAL"), 0o644))
	// One inode, two names: the shape hard-link reproduction exists for.
	require.NoError(t, os.Link(firstPath, secondPath))

	originalHook := copyTreeAfterSourceInspect
	t.Cleanup(func() { copyTreeAfterSourceInspect = originalHook })

	// Rewrite the shared inode IN PLACE after the first name is inspected and
	// copied, and before the second is. Truncate+write keeps the inode, which is
	// exactly what makes the identity match and the stale link possible.
	copyTreeAfterSourceInspect = func(path string) error {
		if strings.HasSuffix(path, "b.txt") {
			file, err := os.OpenFile(firstPath, os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			defer file.Close()
			if _, err := file.Write([]byte("REWRITTEN")); err != nil {
				return err
			}
		}
		return nil
	}

	require.NoError(t, copyTree(source, destination),
		"a concurrent write is benign and must not fail the archive; it may only cost the link")

	archivedSecond, err := os.ReadFile(filepath.Join(destination, "b.txt"))
	require.NoError(t, err)
	require.Equal(t, "REWRITTEN", string(archivedSecond),
		"the archived second path must carry the content the source held when that path was reached; "+
			"linking to the first sighting's copy puts bytes in the archive that never existed at this path")
}

// The optimisation must still happen when nothing changes, or the fix would be
// "never link", which passes the test above while silently discarding the
// feature and doubling every archive's size.
func TestCopyTree_StillLinksWhenTheSourceIsUnchanged(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "copy")

	firstPath := filepath.Join(source, "a.txt")
	secondPath := filepath.Join(source, "b.txt")
	require.NoError(t, os.WriteFile(firstPath, []byte("SHARED"), 0o644))
	require.NoError(t, os.Link(firstPath, secondPath))

	require.NoError(t, copyTree(source, destination))

	firstInfo, err := os.Stat(filepath.Join(destination, "a.txt"))
	require.NoError(t, err)
	secondInfo, err := os.Stat(filepath.Join(destination, "b.txt"))
	require.NoError(t, err)
	require.True(t, os.SameFile(firstInfo, secondInfo),
		"an unchanged shared inode must still be reproduced as one inode: the fix must not be 'stop linking'")
}

// Adding a hard link moves the inode's ctime WITHOUT changing its bytes, and
// that is the live-worktree case the link map exists to support. A guard keyed on
// timestamps read it as a rewrite and split one inode into two destination
// inodes; comparing content does not (#3046 review).
func TestCopyTree_LinkAddedMidWalkStillReproducesOneInode(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "copy")

	firstPath := filepath.Join(source, "a.txt")
	secondPath := filepath.Join(source, "b.txt")
	require.NoError(t, os.WriteFile(firstPath, []byte("SHARED"), 0o644))
	require.NoError(t, os.Link(firstPath, secondPath))

	originalHook := copyTreeAfterSourceInspect
	t.Cleanup(func() { copyTreeAfterSourceInspect = originalHook })
	copyTreeAfterSourceInspect = func(path string) error {
		if strings.HasSuffix(path, "b.txt") {
			// A third name for the same inode: ctime moves, content does not.
			return os.Link(firstPath, filepath.Join(source, "c.txt"))
		}
		return nil
	}

	require.NoError(t, copyTree(source, destination))

	firstInfo, err := os.Stat(filepath.Join(destination, "a.txt"))
	require.NoError(t, err)
	secondInfo, err := os.Stat(filepath.Join(destination, "b.txt"))
	require.NoError(t, err)
	require.True(t, os.SameFile(firstInfo, secondInfo),
		"adding a link changes ctime but not content, so the paths must still be reproduced as one inode")
}

// The stale-content guard must not depend on timestamps moving. A same-size
// rewrite with mtime restored can leave a (size, mtime, ctime) stamp identical on
// a coarse-timestamp filesystem, which would relink the first sighting's bytes.
// Content comparison is unaffected.
func TestCopyTree_SameSizeRewriteWithRestoredMtimeIsNotLinked(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "copy")

	firstPath := filepath.Join(source, "a.txt")
	secondPath := filepath.Join(source, "b.txt")
	require.NoError(t, os.WriteFile(firstPath, []byte("AAAAAAAA"), 0o644))
	require.NoError(t, os.Link(firstPath, secondPath))
	original, err := os.Stat(firstPath)
	require.NoError(t, err)

	originalHook := copyTreeAfterSourceInspect
	t.Cleanup(func() { copyTreeAfterSourceInspect = originalHook })
	copyTreeAfterSourceInspect = func(path string) error {
		if strings.HasSuffix(path, "b.txt") {
			file, err := os.OpenFile(firstPath, os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			// Same length, so size cannot betray it.
			if _, err := file.Write([]byte("BBBBBBBB")); err != nil {
				file.Close()
				return err
			}
			file.Close()
			// And the writer puts mtime back, as a build tool restoring timestamps
			// would.
			return os.Chtimes(firstPath, original.ModTime(), original.ModTime())
		}
		return nil
	}

	require.NoError(t, copyTree(source, destination))

	archived, err := os.ReadFile(filepath.Join(destination, "b.txt"))
	require.NoError(t, err)
	require.Equal(t, "BBBBBBBB", string(archived),
		"a same-size rewrite with mtime restored must not be linked to the first sighting's bytes: "+
			"timestamps are not a content version")
}

// The retained-descriptor path: a destination whose own mode denies the copier
// read must still compare, so the hard-link relationship survives (#3063).
//
// This is a unit test rather than a copyTree test on purpose, and the reason is
// worth recording. The end-to-end case needs a source the copier can read
// through GROUP or OTHER bits while the destination — which it owns — denies it
// through OWNER bits, because owner bits take precedence. That requires the
// source to be owned by a different uid (a root-owned 0044 file archived by a
// non-root user), which a unit test cannot arrange. Measured: a self-owned file
// at 0000, 0044 or 0004 is unreadable by its own owner, so any single-uid
// attempt at the end-to-end case fails earlier, at the SOURCE open.
//
// So this exercises the mechanism directly: a destination the process cannot
// reopen, and a retained descriptor that was opened before the mode narrowed.
func TestSourceMatchesCopiedFile_UsesARetainedDescriptorWhenTheModeDeniesRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode bits, so an unreadable destination cannot arise")
	}
	dir := t.TempDir()
	root, err := os.Open(dir)
	require.NoError(t, err)
	defer root.Close()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "src"), []byte("SHARED"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dst"), []byte("SHARED"), 0o600))

	// Retain the descriptor while the file is still readable, exactly as the
	// copier does before preserveSourceMode narrows the mode.
	retained, err := openAtForCompare(root, "dst")
	require.NoError(t, err)
	defer retained.Close()
	require.NoError(t, os.Chmod(filepath.Join(dir, "dst"), 0o000))

	_, err = os.Open(filepath.Join(dir, "dst"))
	require.Error(t, err, "precondition: the destination must be unopenable by its own owner")

	require.False(t, sourceMatchesCopiedFile(root, "src", root, "dst", nil),
		"without the retained descriptor the comparison cannot read the destination at all, "+
			"which is the bug: the link relationship is dropped for a permission reason, not a content one")
	require.True(t, sourceMatchesCopiedFile(root, "src", root, "dst", retained),
		"with a descriptor opened before the mode narrowed, identical content must still compare equal")

	// A SECOND sighting must also match. The retained descriptor sits at EOF after
	// the first comparison, so without a rewind it would compare against nothing
	// and report a mismatch — turning the fix into a one-shot.
	require.True(t, sourceMatchesCopiedFile(root, "src", root, "dst", retained),
		"a repeat sighting must rewind the retained descriptor rather than read from EOF")

	// And it must still be a real comparison, not "always true when retained".
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src"), []byte("CHANGED"), 0o600))
	require.False(t, sourceMatchesCopiedFile(root, "src", root, "dst", retained),
		"a retained descriptor must not turn the comparison into an unconditional yes")
}

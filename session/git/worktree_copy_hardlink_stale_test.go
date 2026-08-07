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

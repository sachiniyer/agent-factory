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

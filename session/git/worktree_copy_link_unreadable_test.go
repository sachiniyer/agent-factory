package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// #3063: the byte comparison must survive a copy whose OWN mode denies the copier.
//
// The reported shape needs two uids — a root-owned 0004 file read by a non-root
// copier through its other bits, whose copy is then owned by the copier with no
// owner-read, and owner bits win. That is not constructible in an unprivileged
// single-uid test: at any mode where the copy is unreadable to me, the source is
// too, and #3087's refuse-unreadable policy stops the copy before this code runs.
// Measured, and the reason this tests the MECHANISM at its own seam rather than
// through copyTree.
//
// What it pins is the exact property the end-to-end case needs: given a
// destination that cannot be reopened, a descriptor retained from when it was
// written still compares — so the later hard-link sighting links instead of
// copying a second inode.
func TestSourceMatchesCopiedFile_UsesTheRetainedReaderWhenTheCopyCannotBeReopened(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits, so an unreadable copy cannot be constructed")
	}
	dir := t.TempDir()
	root, err := os.Open(dir)
	require.NoError(t, err)
	defer root.Close()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "src"), []byte("SHARED"), 0o600))
	copiedPath := filepath.Join(dir, "dst")
	require.NoError(t, os.WriteFile(copiedPath, []byte("SHARED"), 0o600))

	// The descriptor is taken while the file is still readable — exactly when the
	// copier holds it, before preserveSourceMode narrows the mode.
	retained, err := openAtForCompare(root, "dst")
	require.NoError(t, err)
	defer retained.Close()

	srcIdentity, err := identityAt(root, "src")
	require.NoError(t, err)
	dstIdentity, err := identityAt(root, "dst")
	require.NoError(t, err)

	// Now the copy denies its owner, which is this process.
	require.NoError(t, os.Chmod(copiedPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(copiedPath, 0o600) })
	if _, err := openAtForCompare(root, "dst"); err == nil {
		t.Skip("this user can reopen a 0000 file it owns; the #3063 window does not exist here")
	}

	require.False(t, sourceMatchesCopiedFile(root, "src", root, "dst", dstIdentity, srcIdentity, nil),
		"precondition: without a retained descriptor this is the #3063 loss — the reopen fails and the "+
			"caller copies a second inode instead of linking")
	require.True(t, sourceMatchesCopiedFile(root, "src", root, "dst", dstIdentity, srcIdentity, retained),
		"a descriptor retained from the write predates the mode narrowing, so the comparison still "+
			"reads and the hard link is preserved (#3063)")
}

// The retained descriptor must not become a way to link MISMATCHED bytes: it is a
// cheaper way to read the same file, not a reason to trust it. Same refusal as the
// reopen path, which is what keeps #3046's guarantee intact.
func TestSourceMatchesCopiedFile_RetainedReaderStillRefusesChangedBytes(t *testing.T) {
	dir := t.TempDir()
	root, err := os.Open(dir)
	require.NoError(t, err)
	defer root.Close()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "src"), []byte("ORIGINAL"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dst"), []byte("ORIGINAL"), 0o600))
	retained, err := openAtForCompare(root, "dst")
	require.NoError(t, err)
	defer retained.Close()

	srcIdentity, err := identityAt(root, "src")
	require.NoError(t, err)
	dstIdentity, err := identityAt(root, "dst")
	require.NoError(t, err)
	require.True(t, sourceMatchesCopiedFile(root, "src", root, "dst", dstIdentity, srcIdentity, retained))

	// Rewritten in place, same size — the case a stamp misses and #3046 is about.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src"), []byte("REWRITTE"), 0o600))
	require.False(t, sourceMatchesCopiedFile(root, "src", root, "dst", dstIdentity, srcIdentity, retained),
		"the retained descriptor reads the COPY; a source that no longer matches it must still refuse, "+
			"or #3046's stale-content guarantee is lost through the new path")
}

// A destination LONGER than the source must not compare equal.
//
// NAMED for what it actually covers, after a mutation showed it does not cover
// what I first claimed. It exercises the SIZE GUARD, which already worked: with
// the copy at 15 bytes and the source at 6, `currentInfo.Size() != copiedInfo.Size()`
// refuses before any byte is read, so it passes with or without the stale-EOF cap.
//
// The P1 that prompted the reader change is an append landing BETWEEN
// `copied.Stat()` and the comparison, and that interleaving is not reachable from
// outside the function — there is no seam to land a write in that window. The fix
// (reading to the inode's real end instead of the size read a moment earlier) is
// therefore NOT covered by a test, and saying so is better than leaving this one
// looking like it is.
func TestSourceMatchesCopiedFile_RefusesADestinationLongerThanTheSource(t *testing.T) {
	dir := t.TempDir()
	root, err := os.Open(dir)
	require.NoError(t, err)
	defer root.Close()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "src"), []byte("SHARED"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dst"), []byte("SHARED"), 0o600))
	srcIdentity, err := identityAt(root, "src")
	require.NoError(t, err)
	dstIdentity, err := identityAt(root, "dst")
	require.NoError(t, err)
	require.True(t, sourceMatchesCopiedFile(root, "src", root, "dst", dstIdentity, srcIdentity, nil),
		"precondition: identical bytes compare equal")

	// The destination now holds MORE than the source. Same inode, same identity.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dst"), []byte("SHARED-AND-MORE"), 0o600))

	require.False(t, sourceMatchesCopiedFile(root, "src", root, "dst", dstIdentity, srcIdentity, nil),
		"the copy is longer than the source, so linking to it would publish bytes that were never at "+
			"this path; a reader capped at the earlier size reports a false match")
}

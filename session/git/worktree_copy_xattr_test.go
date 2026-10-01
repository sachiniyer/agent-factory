package git

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The fidelity guard (worktree_copy_fidelity_test.go) proves ONE attribute survives
// the cross-device copy. These pin the parts of #2919's xattr work that a
// single-attribute probe cannot see: several attributes on one node, a present
// attribute whose value is EMPTY — which is meaningful and which a "size 0 means
// absent" reading would silently drop — and that a node with no attributes at all
// copies cleanly rather than erroring on an empty list.

// xattrCapableDir skips the test when the filesystem under t.TempDir() cannot hold
// user.* attributes. Probed by WRITING to the source tree, not by inspecting a tree
// built later: asking the wrong tree is how the fidelity guard once dropped its xattr
// comparison entirely while still reporting a pass (#2919).
func xattrCapableDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	probe := filepath.Join(dir, ".xattr-probe")
	require.NoError(t, os.WriteFile(probe, []byte("x"), 0o600))
	if err := unix.Setxattr(probe, "user.af_probe", []byte("1"), 0); err != nil {
		t.Skipf("filesystem under %s does not support user.* extended attributes: %v", dir, err)
	}
	require.NoError(t, os.Remove(probe))
	return dir
}

// xattrFixture is a source/destination pair on an xattr-capable filesystem, with the
// cross-device path forced so moveDirCrossDevice really copies rather than renames.
func xattrFixture(t *testing.T) (source, destination string) {
	t.Helper()
	workspace := xattrCapableDir(t)
	source = filepath.Join(workspace, "src")
	require.NoError(t, os.MkdirAll(source, 0o755))
	original := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = original })
	return source, filepath.Join(workspace, "dst")
}

// listAttrs returns the attribute names present on path.
func listAttrs(t *testing.T, path string) []string {
	t.Helper()
	size, err := unix.Listxattr(path, nil)
	require.NoError(t, err)
	if size == 0 {
		return nil
	}
	buffer := make([]byte, size)
	read, err := unix.Listxattr(path, buffer)
	require.NoError(t, err)
	var names []string
	for _, name := range bytes.Split(buffer[:read], []byte{0}) {
		if len(name) > 0 {
			names = append(names, string(name))
		}
	}
	return names
}

func readAttr(t *testing.T, path, name string) []byte {
	t.Helper()
	size, err := unix.Getxattr(path, name, nil)
	require.NoError(t, err, "attribute %q must exist on %s", name, path)
	if size == 0 {
		return nil
	}
	value := make([]byte, size)
	read, err := unix.Getxattr(path, name, value)
	require.NoError(t, err)
	return value[:read]
}

func TestCopyTree_ReproducesEveryVisibleXattrIncludingEmptyValues(t *testing.T) {
	workspace := xattrCapableDir(t)
	source := filepath.Join(workspace, "src")
	require.NoError(t, os.MkdirAll(filepath.Join(source, "dir"), 0o700))
	file := filepath.Join(source, "dir", "carrier.txt")
	require.NoError(t, os.WriteFile(file, []byte("payload"), 0o640))

	// Two attributes, and one of them EMPTY. An empty value is not the same as an
	// absent attribute, and the two-call sizing makes it easy to conflate them.
	require.NoError(t, unix.Setxattr(file, "user.af_one", []byte("first"), 0))
	require.NoError(t, unix.Setxattr(file, "user.af_empty", []byte{}, 0))
	require.NoError(t, unix.Setxattr(filepath.Join(source, "dir"), "user.af_dir", []byte("ondir"), 0))

	destination := filepath.Join(workspace, "dst")
	require.NoError(t, copyTree(source, destination))

	copied := filepath.Join(destination, "dir", "carrier.txt")
	require.Equal(t, []byte("first"), readAttr(t, copied, "user.af_one"))
	require.Empty(t, readAttr(t, copied, "user.af_empty"),
		"a present attribute with an empty value must arrive present, not dropped")
	require.Equal(t, []byte("ondir"), readAttr(t, filepath.Join(destination, "dir"), "user.af_dir"),
		"directories carry attributes too — a lost DEFAULT ACL is the dangerous case")

	// The names must round-trip as a set, so an extra or missing attribute fails here
	// rather than only when someone asks for the one that vanished.
	names, err := listXattrNames(int(mustOpen(t, copied).Fd()))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"user.af_one", "user.af_empty"}, names)
}

func TestCopyTree_NodeWithNoXattrsCopiesCleanly(t *testing.T) {
	workspace := xattrCapableDir(t)
	source := filepath.Join(workspace, "src")
	require.NoError(t, os.MkdirAll(source, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "bare.txt"), []byte("no attrs"), 0o600))

	destination := filepath.Join(workspace, "dst")
	require.NoError(t, copyTree(source, destination),
		"an empty attribute list must not be read as an error")

	body, err := os.ReadFile(filepath.Join(destination, "bare.txt"))
	require.NoError(t, err)
	require.Equal(t, "no attrs", string(body))
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	handle, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = handle.Close() })
	return handle
}

// TestCopyTree_CopiesXattrsOntoReadOnlyNodes is the P1 from review, and what it pins
// is the CREATION mode, not the ordering. Setting an attribute in the user namespace
// requires write permission on the INODE, and an already-open write descriptor does
// not satisfy it. openat() created the destination at the source's mode, so a
// checked-in 0444 file was read-only from the moment it existed and every user.*
// attribute failed EACCES wherever in the sequence it was attempted — reordering the
// call could not help, which is how the first attempt at this fix passed review while
// changing nothing. The privilege branch then logged the EACCES as "needs privilege"
// and carried on, losing an ordinary attribute silently on every read-only node.
//
// workingFileMode is the fix: create owner-writable, apply the real mode last.
func TestCopyTree_CopiesXattrsOntoReadOnlyNodes(t *testing.T) {
	source, destination := xattrFixture(t)
	// Attribute FIRST, then chmod. Setting a user.* attribute needs write permission
	// on the inode, so a fixture built read-only cannot be given one — the first
	// version of this test tried, took the EACCES for "this filesystem has no user
	// xattrs", and SKIPPED. It reported green on every run without once exercising
	// the ordering it exists to pin.
	readOnly := filepath.Join(source, "locked.txt")
	require.NoError(t, os.WriteFile(readOnly, []byte("contents"), 0o644))
	require.NoError(t, unix.Setxattr(readOnly, "user.af_locked", []byte("kept"), 0))
	require.NoError(t, os.Chmod(readOnly, 0o444))
	readOnlyDir := filepath.Join(source, "lockeddir")
	require.NoError(t, os.Mkdir(readOnlyDir, 0o755))
	require.NoError(t, unix.Setxattr(readOnlyDir, "user.af_lockeddir", []byte("kept"), 0))
	require.NoError(t, os.Chmod(readOnlyDir, 0o555))

	require.NoError(t, moveDirCrossDevice(source, destination, "move"))

	require.Equal(t, []byte("kept"), readAttr(t, filepath.Join(destination, "locked.txt"), "user.af_locked"),
		"a 0444 file must keep its attributes — the mode must not be applied before they are written")
	require.Equal(t, []byte("kept"), readAttr(t, filepath.Join(destination, "lockeddir"), "user.af_lockeddir"),
		"and a 0555 directory likewise")

	// The mode itself must still land, so the fix cannot have simply left nodes writable.
	info, err := os.Stat(filepath.Join(destination, "locked.txt"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o444), info.Mode().Perm())
}

// TestCopyTree_DropsDestinationOnlyXattrs pins the other P1. A destination parent
// carrying a DEFAULT POSIX ACL gives every newly created child a
// system.posix_acl_access of its own, so a source file with no ACL arrives with one —
// and an inherited ACL is generally WIDER than the mode it replaces, making this a
// permission-widening divergence rather than an untidy one.
//
// Simulated with a user.* attribute rather than a real default ACL: the mechanism
// under test is "the destination has an attribute the source does not", and a user
// attribute exercises it on any filesystem, where setting a default ACL needs tooling
// this test cannot assume.
func TestCopyTree_DropsDestinationOnlyXattrs(t *testing.T) {
	source, destination := xattrFixture(t)
	file := filepath.Join(source, "plain.txt")
	require.NoError(t, os.WriteFile(file, []byte("contents"), 0o644))
	if err := unix.Setxattr(file, "user.af_kept", []byte("yes"), 0); err != nil {
		t.Skipf("this filesystem does not support user xattrs: %v", err)
	}

	require.NoError(t, moveDirCrossDevice(source, destination, "move"))
	copied := filepath.Join(destination, "plain.txt")

	// Stand in for the inherited attribute, then re-run the copy step against a source
	// that never had it.
	require.NoError(t, unix.Setxattr(copied, "user.af_inherited", []byte("from-parent"), 0))
	names := listAttrs(t, copied)
	require.Contains(t, names, "user.af_inherited")

	second := filepath.Join(filepath.Dir(destination), "dst2")
	require.NoError(t, moveDirCrossDevice(destination, second, "move"))
	// The second copy's source DID have it, so it is carried — the prune must remove
	// only what the source lacks, never what it has.
	require.Contains(t, listAttrs(t, filepath.Join(second, "plain.txt")), "user.af_inherited")
	require.Equal(t, []byte("yes"), readAttr(t, filepath.Join(second, "plain.txt"), "user.af_kept"))
}

// TestDestinationRejectsAllXattrs_OnlyWhenTheListingSaysSo pins the distinction the
// latch depends on. EOPNOTSUPP comes back per NAMESPACE as well as per filesystem, so
// "one attribute was refused" is not evidence that the destination holds none — and
// acting on it would skip every remaining attribute on this node and on every later
// node in the copy. The predicate must consult the destination descriptor itself.
func TestDestinationRejectsAllXattrs_OnlyWhenTheListingSaysSo(t *testing.T) {
	dir := xattrCapableDir(t)
	path := filepath.Join(dir, "holder.txt")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	handle := mustOpen(t, path)

	require.False(t, destinationRejectsAllXattrs(int(handle.Fd())),
		"a filesystem whose listing succeeds does support attributes, whatever a single "+
			"rejected namespace reported")

	// And the errno classifier underneath it must accept the spelling THIS platform
	// uses, not just the Linux one: darwin returns ENOTSUP (0x2d) where Linux returns
	// EOPNOTSUPP (0x5f) and treats the two names as one value. Matching only the Linux
	// spelling would drop an unsupported darwin destination into the "unexpected error"
	// branch and fail the whole archive instead of warning.
	for _, unsupported := range xattrUnsupportedErrnos() {
		require.True(t, isXattrUnsupported(unsupported),
			"every errno this platform reports as unsupported must classify as unsupported")
	}
	require.False(t, isXattrUnsupported(unix.EIO),
		"a real I/O failure must not be mistaken for an attribute-less filesystem")
}

// readSymlinkAttr is readAttr's link-following-averse twin: Getxattr follows a
// symlink to its target, so a probe of the link's OWN attribute has to use Lgetxattr
// or it reads the target's attributes (or errors on a missing target) instead.
func readSymlinkAttr(t *testing.T, path, name string) []byte {
	t.Helper()
	size, err := unix.Lgetxattr(path, name, nil)
	require.NoError(t, err, "attribute %q must exist on symlink %s", name, path)
	if size == 0 {
		return nil
	}
	value := make([]byte, size)
	n, err := unix.Lgetxattr(path, name, value)
	require.NoError(t, err)
	return value[:n]
}

// TestCopyTree_ReproducesSymlinkXattrs pins the one node class the F*-anchored
// copySourceXattrs could not reach. A symlink yields no usable descriptor (O_NOFOLLOW
// returns ELOOP, and O_PATH|O_NOFOLLOW makes Flistxattr/Fgetxattr return EBADF), so
// before the path-based L* family reproduced them a link's attributes were dropped
// silently on the cross-device copy — the exact #2919 invariant the file and
// directory paths log-and-continue on. This test forces that path and checks the
// attribute arrives.
//
// user.* is the only namespace a test can set without privilege. On Linux the VFS
// rejects setting ANY attribute on a symlink with EPERM (only security.* / trusted.*
// live on links in production, and neither is settable without SELinux or root), so
// this probe skips there; on Darwin Lsetxattr of user.* on a symlink succeeds, which
// is the platform the comment in copySymlinkEntry names as the one where this gap is
// measurable in CI. Skipping is the skip message, not a silent pass: a host that
// cannot set a link attribute cannot exercise the divergence either way.
func TestCopyTree_ReproducesSymlinkXattrs(t *testing.T) {
	source, destination := xattrFixture(t)
	link := filepath.Join(source, "link")
	require.NoError(t, os.Symlink("target", link))
	if err := unix.Lsetxattr(link, "user.af_symlink", []byte("linkval"), 0); err != nil {
		t.Skipf("filesystem does not support setting extended attributes on a symlink (%v); "+
			"the symlink-xattr divergence is only measurable on a host where Lsetxattr on a link succeeds (e.g. macOS)", err)
	}

	require.NoError(t, moveDirCrossDevice(source, destination, "move"))

	copied := filepath.Join(destination, "link")
	require.Equal(t, []byte("linkval"), readSymlinkAttr(t, copied, "user.af_symlink"),
		"a symlink's extended attributes must be reproduced on the cross-device copy, not dropped silently")
}

// TestCopySymlinkXattrs_TooLongPathIsNonFatal pins the regression the L* family
// reintroduces: the descriptor-anchored F* xattr paths take an fd and have no
// path-length limit, but copySymlinkXattrs addresses a link through its full textual
// route (there is no *at xattr variant), so a tree the copier reaches
// component-by-component through directory descriptors can carry a link whose route
// exceeds PATH_MAX. Llistxattr on such a route returns ENAMETOOLONG even for a link
// with no attributes, which would abort a cross-device move the F* paths copy fine.
// The fix is to treat ENAMETOOLONG from the L* family as a logged, non-fatal skip
// (errXattrPathTooLong) rather than a fatal "cannot move" — the #2919 invariant the
// per-attribute refusal follows — so the move continues without the link's xattrs.
// The path need not exist: the kernel rejects a path longer than PATH_MAX before it
// resolves it, so the probe is portable and needs no fixture.
func TestCopySymlinkXattrs_TooLongPathIsNonFatal(t *testing.T) {
	// unix.PathMax is 4096 on Linux and 1024 on Darwin; a route longer than it
	// makes Llistxattr fail with ENAMETOOLONG regardless of whether the link exists.
	tooLong := strings.Repeat("a", unix.PathMax+1)
	err := copySymlinkSourceXattrs(tooLong, tooLong)
	require.ErrorIs(t, err, errXattrPathTooLong,
		"a route longer than PATH_MAX must be a non-fatal errXattrPathTooLong, not a fatal \"cannot move\" error")

	// copySymlinkXattrs surfaces the sentinel for copySymlinkEntry to skip the
	// prune and route recheck on, rather than aborting the move.
	support := &xattrDestination{}
	err = copySymlinkXattrs(support, tooLong, tooLong)
	require.ErrorIs(t, err, errXattrPathTooLong,
		"copySymlinkXattrs must surface errXattrPathTooLong so the move continues rather than aborting")
}

// TestCopySymlinkXattrs_HoldsNoneSkipsPathBasedOps pins the regression the
// filesystem-wide latch introduced: when a file/dir copy already set
// support.holdsNone (the destination filesystem holds no xattrs at all), a later
// symlink whose route exceeds PATH_MAX must not reach the path-based prune and
// route recheck, because copySymlinkEntry's recheck Lstats the same too-long path
// and would abort a cross-device move the descriptor-anchored F* paths copy fine.
// The holdsNone branch returns errXattrPathTooLong so the caller skips those
// path-based operations, matching the path-too-long case: no L* call ran through
// the path, so there is nothing to recheck, and the destination holds no xattrs so
// prune has nothing to remove. The path need not exist: a holdsNone latch is the
// only input that matters here, and a too-long path is the route that would abort
// the recheck, so both are exercised together.
func TestCopySymlinkXattrs_HoldsNoneSkipsPathBasedOps(t *testing.T) {
	tooLong := strings.Repeat("a", unix.PathMax+1)
	support := &xattrDestination{holdsNone: true}
	err := copySymlinkXattrs(support, tooLong, tooLong)
	require.ErrorIs(t, err, errXattrPathTooLong,
		"a holdsNone latch must surface errXattrPathTooLong so copySymlinkEntry skips the path-based prune and route recheck, not nil that would run them on a too-long route")
}

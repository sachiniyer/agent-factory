package git

import (
	"os"
	"path/filepath"
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

package git

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopiedDirectoryRoutesRetainLinearAncestry(t *testing.T) {
	const depth = 128
	manifest := copiedDirectory{}
	current := &manifest
	for range depth {
		child := &copiedDirectory{}
		current.entries = []copiedEntry{{name: "d", directory: child}}
		current = child
	}

	routes := copiedDirectoryRoutes(&manifest)
	assert.LessOrEqual(t, retainedRouteEntries(routes), depth*2,
		"cleanup routes must not retain a complete ancestry copy for every directory")
}

func TestCopyTree_PartialFailurePreservesChangedDestination(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "a-dir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a-dir", "original.txt"), []byte("original"), 0644))
	require.NoError(t, syscall.Mkfifo(filepath.Join(src, "z-fifo"), 0600))
	dest := filepath.Join(t.TempDir(), "dest")

	originalHook := copyTreeBeforeSourceOpen
	copyTreeBeforeSourceOpen = func(path string) error {
		if !strings.HasSuffix(path, "z-fifo") {
			return nil
		}
		if err := os.Rename(filepath.Join(dest, "a-dir"), filepath.Join(dest, "stranded-copy")); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(dest, "a-dir"), 0755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dest, "a-dir", "replacement.txt"), []byte("replacement"), 0644)
	}
	t.Cleanup(func() { copyTreeBeforeSourceOpen = originalHook })

	require.Error(t, copyTree(src, dest))
	assert.FileExists(t, filepath.Join(dest, "a-dir", "replacement.txt"))
	assert.FileExists(t, filepath.Join(dest, "stranded-copy", "original.txt"))
}

func TestCopyTree_RejectsDestinationSymlinkReplacement(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.Symlink("source-target", filepath.Join(src, "link")))
	dest := filepath.Join(t.TempDir(), "dest")

	originalHook := copyTreeAfterSymlinkCreate
	copyTreeAfterSymlinkCreate = func(path string) error {
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.Symlink("replacement-target", path)
	}
	t.Cleanup(func() { copyTreeAfterSymlinkCreate = originalHook })

	require.Error(t, copyTree(src, dest), "a replacement symlink target must not enter the copy manifest")
}

func TestCopyTree_RejectsExcessiveDepth(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	current := src
	for range 257 {
		current = filepath.Join(current, "d")
		require.NoError(t, os.Mkdir(current, 0755))
	}
	dest := filepath.Join(t.TempDir(), "dest")

	err := copyTree(src, dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maximum supported depth")
}

package git

import (
	"fmt"
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
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a-file"), []byte("original"), 0644))
	require.NoError(t, syscall.Mkfifo(filepath.Join(src, "z-fifo"), 0600))
	dest := filepath.Join(t.TempDir(), "dest")

	originalHook := copyTreeBeforeSourceOpen
	copyTreeBeforeSourceOpen = func(path string) error {
		if !strings.HasSuffix(path, "z-fifo") {
			return nil
		}
		if err := os.Rename(filepath.Join(dest, "a-file"), filepath.Join(dest, "stranded-copy")); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dest, "a-file"), []byte("replacement"), 0644)
	}
	t.Cleanup(func() { copyTreeBeforeSourceOpen = originalHook })

	require.Error(t, copyTree(src, dest))
	assert.FileExists(t, filepath.Join(dest, "a-file"))
	contents, readErr := os.ReadFile(filepath.Join(dest, "stranded-copy"))
	require.NoError(t, readErr)
	assert.Equal(t, "original", string(contents))
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

// TestCopyTree_PostCreateFailureCleansPartialDestination pins the manifest
// contract: a destination node is recorded the moment this process creates it
// and learns its identity, not once the whole entry succeeds. A failure after
// creation — ENOSPC mid-copy, a failing Close, a directory that cannot be
// reopened — must leave a removable partial tree rather than one cleanup
// refuses as unexpected.
func TestCopyTree_PostCreateFailureCleansPartialDestination(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		build   func(t *testing.T, src string)
		created string
	}{
		{
			name: "regular file",
			build: func(t *testing.T, src string) {
				require.NoError(t, os.WriteFile(filepath.Join(src, "a-file"), []byte("original"), 0644))
			},
			created: "a-file",
		},
		{
			name: "directory",
			build: func(t *testing.T, src string) {
				require.NoError(t, os.Mkdir(filepath.Join(src, "a-dir"), 0755))
			},
			created: "a-dir",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "src")
			require.NoError(t, os.Mkdir(src, 0755))
			testCase.build(t, src)
			dest := filepath.Join(t.TempDir(), "dest")

			originalHook := copyTreeAfterDestCreate
			copyTreeAfterDestCreate = func(path string) error {
				if !strings.HasSuffix(path, testCase.created) {
					return nil
				}
				return fmt.Errorf("forced failure after destination create: %w", syscall.ENOSPC)
			}
			t.Cleanup(func() { copyTreeAfterDestCreate = originalHook })

			err := copyTree(src, dest)
			require.ErrorIs(t, err, syscall.ENOSPC)
			assert.NotContains(t, err.Error(), "refusing to remove",
				"a node this process created must stay removable, not read as unexpected")
			assert.NoDirExists(t, dest, "the partial destination tree must not leak")
		})
	}
}

// TestCopyTree_SymlinkPostCreateFailureCleansPartialDestination covers the same
// contract on the symlink path, whose creation seam predates the manifest entry.
func TestCopyTree_SymlinkPostCreateFailureCleansPartialDestination(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.Symlink("source-target", filepath.Join(src, "link")))
	dest := filepath.Join(t.TempDir(), "dest")

	originalHook := copyTreeAfterSymlinkCreate
	copyTreeAfterSymlinkCreate = func(string) error {
		return fmt.Errorf("forced failure after symlink create: %w", syscall.EIO)
	}
	t.Cleanup(func() { copyTreeAfterSymlinkCreate = originalHook })

	err := copyTree(src, dest)
	require.ErrorIs(t, err, syscall.EIO)
	assert.NotContains(t, err.Error(), "refusing to remove")
	assert.NoDirExists(t, dest, "the partial destination tree must not leak")
}

// TestMoveDirCrossDevice_PostCreateFailureCleansPrivateStaging is the user-facing
// consequence: without the created node in the manifest, every failed archive
// attempt strands another .af-copy-* tree in the destination parent.
func TestMoveDirCrossDevice_PostCreateFailureCleansPrivateStaging(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a-file"), []byte("original"), 0644))
	destinationParent := t.TempDir()

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	originalHook := copyTreeAfterDestCreate
	copyTreeAfterDestCreate = func(path string) error {
		if !strings.HasSuffix(path, "a-file") {
			return nil
		}
		return fmt.Errorf("forced failure after destination create: %w", syscall.ENOSPC)
	}
	t.Cleanup(func() { copyTreeAfterDestCreate = originalHook })

	err := moveDirCrossDevice(src, filepath.Join(destinationParent, "dest"))
	require.ErrorIs(t, err, syscall.ENOSPC)
	entries, readErr := os.ReadDir(destinationParent)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "a failed copy must not strand a private staging tree")
	assert.FileExists(t, filepath.Join(src, "a-file"), "a failed move must leave the source intact")
}

// TestMoveDirCrossDevice_InspectFailureNeverRestoresAReplacement pins the
// inspection-error branch to what it can verify. A racer that strands the
// just-claimed source and drops a replacement at the quarantine name before the
// restore runs must not have that replacement renamed onto src and reported as
// the restored source — the real tree has to stay recoverable.
func TestMoveDirCrossDevice_InspectFailureNeverRestoresAReplacement(t *testing.T) {
	sourceParent := t.TempDir()
	src := filepath.Join(sourceParent, "src")
	require.NoError(t, os.Mkdir(src, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "original.txt"), []byte("original"), 0644))
	destinationParent := t.TempDir()

	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })

	stranded := filepath.Join(sourceParent, "stranded")
	originalInspect := moveDirInspectClaimedSource
	moveDirInspectClaimedSource = func(_ *os.File, name string) (pathIdentity, error) {
		claimed := filepath.Join(sourceParent, name)
		if err := os.Rename(claimed, stranded); err != nil {
			return pathIdentity{}, err
		}
		if err := os.Mkdir(claimed, 0755); err != nil {
			return pathIdentity{}, err
		}
		if err := os.WriteFile(filepath.Join(claimed, "replacement.txt"), []byte("replacement"), 0644); err != nil {
			return pathIdentity{}, err
		}
		return pathIdentity{}, syscall.ENOENT
	}
	t.Cleanup(func() { moveDirInspectClaimedSource = originalInspect })

	err := moveDirCrossDevice(src, filepath.Join(destinationParent, "dest"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "restored it to",
		"a replacement must never be reported as the restored source")
	assert.FileExists(t, filepath.Join(stranded, "original.txt"),
		"the source this process opened must stay recoverable")
	assert.NoFileExists(t, filepath.Join(src, "replacement.txt"),
		"a replacement must not be published at the source path")
}

// TestRemoveCreatedDirectory_RefusesAReplacement proves the staging-root
// cleanup is tied to an identity rather than to a name. unlinkat() resolves a
// name, so a same-UID racer that swaps its own empty directory in at the moment
// the staging open fails would otherwise have that directory rmdir'd.
func TestRemoveCreatedDirectory_RefusesAReplacement(t *testing.T) {
	parentPath := t.TempDir()
	parent, _, err := openDirectoryPathFollowingLinks(parentPath, "parent")
	require.NoError(t, err)
	t.Cleanup(func() { _ = parent.Close() })

	// The happy path: the name still identifies what this process created.
	require.NoError(t, os.Mkdir(filepath.Join(parentPath, "removable"), 0755))
	removable, err := identityAt(parent, "removable")
	require.NoError(t, err)
	require.NoError(t, removeCreatedDirectory(parent, parentPath, "removable", removable))
	assert.NoDirExists(t, filepath.Join(parentPath, "removable"))

	// The raced path: the racer renames the staging root away and drops its own
	// directory at that name. Keeping the original alive under another name is
	// also what stops the filesystem from recycling its inode.
	require.NoError(t, os.Mkdir(filepath.Join(parentPath, "staging"), 0755))
	created, err := identityAt(parent, "staging")
	require.NoError(t, err)
	require.NoError(t, os.Rename(filepath.Join(parentPath, "staging"), filepath.Join(parentPath, "stranded")))
	require.NoError(t, os.Mkdir(filepath.Join(parentPath, "staging"), 0755))
	replacementID, err := identityAt(parent, "staging")
	require.NoError(t, err)
	require.NotEqual(t, created, replacementID, "the replacement must be a distinct inode")

	err = removeCreatedDirectory(parent, parentPath, "staging", created)
	require.Error(t, err, "cleanup must not remove a directory it did not create")
	var unverified *unverifiedCleanupPathError
	assert.ErrorAs(t, err, &unverified, "an unverified removal must fail closed")
	assert.DirExists(t, filepath.Join(parentPath, "staging"), "the replacement must survive")
}

func TestCopyTree_RejectsExcessiveDepth(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.Mkdir(src, 0755))
	current := src
	for range maxArchiveTreeDepth + 1 {
		current = filepath.Join(current, "d")
		require.NoError(t, os.Mkdir(current, 0755))
	}
	dest := filepath.Join(t.TempDir(), "dest")

	err := copyTree(src, dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maximum supported depth")
}

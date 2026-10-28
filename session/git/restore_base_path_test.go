package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The preferred restore placement must not step past an occupant: a restore
// retry recognizes its own crashed rebuild by comparing git's live checkout
// against it, and on that retry the occupant IS the rebuild, so
// RestoreWorktreePath's collision suffix would never match (#5102).
func TestRestoreWorktreeBasePath_IgnoresAnOccupant(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)

	preferred, err := RestoreWorktreeBasePath(repoRoot, "feature-x", "af/feature-x")
	require.NoError(t, err)
	free, err := RestoreWorktreePath(repoRoot, "feature-x", "af/feature-x")
	require.NoError(t, err)
	assert.Equal(t, free, preferred, "with the path free, both name the same placement")

	require.NoError(t, os.MkdirAll(preferred, 0o755))
	again, err := RestoreWorktreeBasePath(repoRoot, "feature-x", "af/feature-x")
	require.NoError(t, err)
	assert.Equal(t, preferred, again, "an occupant must not move the preferred placement")
	bumped, err := RestoreWorktreePath(repoRoot, "feature-x", "af/feature-x")
	require.NoError(t, err)
	assert.Equal(t, preferred+"-2", bumped, "the rebuild destination still steps past it")
}

// git reports resolved paths while af's recorded placement may go through a
// symlink, so the comparison resolves both; distinct directories, including
// a suffixed sibling, never compare equal.
func TestSameWorktreePath(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "wt"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(real, "wt-2"), 0o755))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(real, link))

	same, err := SameWorktreePath(filepath.Join(link, "wt"), filepath.Join(real, "wt"))
	require.NoError(t, err)
	assert.True(t, same, "a symlinked spelling names the same directory")

	same, err = SameWorktreePath(filepath.Join(real, "wt")+"/", filepath.Join(real, "wt"))
	require.NoError(t, err)
	assert.True(t, same)

	same, err = SameWorktreePath(filepath.Join(link, "wt-2"), filepath.Join(real, "wt"))
	require.NoError(t, err)
	assert.False(t, same, "a suffixed sibling is a different placement")
}

package git

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestMoveWorktree_BoundedLongDestLeaf is the session/git layer of the archive
// long-title fix. The daemon bounds the archive leaf to NAME_MAX (255) in
// sanitizeArchiveTitle; this proves the relocation paths a bounded leaf flows
// into — the git fast path AND the manual os.Rename fallback (Case B in the
// report) — accept a 255-byte destination leaf. Against the unbounded sanitizer
// the leaf was > 255 and BOTH `git worktree move` and the os.Rename fallback
// failed with "file name too long"; at the 255-byte bound both relocate cleanly.
//
// The fast path is covered end-to-end via the daemon's TestArchiveLongTitleE2E
// (real `git worktree move` onto the bounded leaf). This test forces the fast
// path off so the os.Rename fallback runs against a 255-byte leaf — the path
// the daemon package cannot reach (worktreeMoveFast is unexported here on
// purpose) and the one the report reproduces as Case B.
func TestMoveWorktree_BoundedLongDestLeaf(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGitInPlaceTest(t, repoRoot, "commit", "--allow-empty", "-m", "init")

	srcPath := filepath.Join(filepath.Dir(repoRoot), "long-dest-src")
	runGitInPlaceTest(t, repoRoot, "worktree", "add", "-b", "arch/branch", srcPath)
	require.NoError(t, os.WriteFile(filepath.Join(srcPath, "dirty.txt"), []byte("uncommitted work"), 0644))

	gw, err := NewGitWorktreeFromStorage(repoRoot, srcPath, "arch", "arch/branch", "", false, true)
	require.NoError(t, err)

	// A 255-byte leaf: exactly NAME_MAX, the bound sanitizeArchiveTitle enforces.
	// Both the git fast path move and the os.Rename fallback must accept it.
	leaf := strings.Repeat("a", 255)
	require.Equal(t, 255, len(leaf))
	dest := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", leaf)

	// Force the git fast path off so relocateWorktreeTo falls back to the manual
	// os.Rename relocation (the report's Case B). Against a > 255-byte leaf this
	// fails ENAMETOOLONG; at the 255-byte bound os.Rename relocates cleanly.
	previousMove := worktreeMoveFast
	forced := false
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		forced = true
		return errors.New("force manual relocation")
	}
	t.Cleanup(func() { worktreeMoveFast = previousMove })

	require.NoError(t, gw.MoveWorktree(dest), "the os.Rename fallback must relocate onto the 255-byte leaf without ENAMETOOLONG")
	assert.True(t, forced, "the fast path must have been attempted and fallen back to the manual move")
	assert.False(t, pathExists(srcPath), "the source worktree directory must be gone after the rename")
	assertLiveWorktreeAt(t, gw, dest)
	assert.Equal(t, 255, len(filepath.Base(dest)), "the destination leaf must be the 255-byte bounded name")
}

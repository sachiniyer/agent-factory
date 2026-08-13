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

// TestVerifyArchivedWorktreePointer_Shapes pins the pointer check's refusal
// polarity (#3278 review): only a regular, bounded .git file with a
// linked-worktree gitdir line whose target is conclusively absent passes.
// Symlinks and special files must be refused by the descriptor itself — the
// open never follows links and the read is bounded — so a same-UID race
// swapping the file cannot stall the kill or feed it an unbounded read.
func TestVerifyArchivedWorktreePointer_Shapes(t *testing.T) {
	newWorktreeDir := func(t *testing.T) string {
		t.Helper()
		return t.TempDir()
	}

	t.Run("valid pointer with absent gitdir passes", func(t *testing.T) {
		dir := newWorktreeDir(t)
		gone := filepath.Join(dir, "gone-repo", ".git", "worktrees", "wt")
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"),
			[]byte("gitdir: "+gone+"\n"), 0o644))
		assert.NoError(t, VerifyArchivedWorktreePointer(dir))
	})

	t.Run("missing pointer refused", func(t *testing.T) {
		dir := newWorktreeDir(t)
		assert.Error(t, VerifyArchivedWorktreePointer(dir))
	})

	t.Run("symlink pointer refused without following", func(t *testing.T) {
		dir := newWorktreeDir(t)
		target := filepath.Join(dir, "real-file")
		require.NoError(t, os.WriteFile(target, []byte("gitdir: /x/.git/worktrees/wt\n"), 0o644))
		require.NoError(t, os.Symlink(target, filepath.Join(dir, ".git")))
		err := VerifyArchivedWorktreePointer(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "without following links")
	})

	t.Run("fifo pointer refused without blocking", func(t *testing.T) {
		dir := newWorktreeDir(t)
		require.NoError(t, syscall.Mkfifo(filepath.Join(dir, ".git"), 0o644))
		err := VerifyArchivedWorktreePointer(dir)
		require.Error(t, err, "a FIFO at .git must be refused, not block the kill on open or read")
		assert.Contains(t, err.Error(), "not a regular file")
	})

	t.Run("oversized pointer refused by the bounded read", func(t *testing.T) {
		dir := newWorktreeDir(t)
		huge := "gitdir: /x/.git/worktrees/" + strings.Repeat("a", archivedWorktreePointerMaxSize) + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"), []byte(huge), 0o644))
		err := VerifyArchivedWorktreePointer(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "too large")
	})

	t.Run("non-worktree gitdir refused", func(t *testing.T) {
		dir := newWorktreeDir(t)
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"),
			[]byte("gitdir: /somewhere/plain\n"), 0o644))
		err := VerifyArchivedWorktreePointer(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a linked-worktree metadata directory")
	})

	t.Run("live gitdir target refused", func(t *testing.T) {
		dir := newWorktreeDir(t)
		live := filepath.Join(dir, "live-repo", ".git", "worktrees", "wt")
		require.NoError(t, os.MkdirAll(live, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"),
			[]byte("gitdir: "+live+"\n"), 0o644))
		err := VerifyArchivedWorktreePointer(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "belongs to a live repository")
	})
}

package git

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// presenceTestWorktree records path as a worktree without needing a repository:
// the presence probe only ever stats the recorded path.
func presenceTestWorktree(t *testing.T, path string) *GitWorktree {
	t.Helper()
	gw, err := NewGitWorktreeFromStorage(
		filepath.Join(filepath.Dir(path), "repo"), path, "presence", "af/presence", "", false, true,
	)
	require.NoError(t, err)
	return gw
}

// stubPresenceLstat fails every lstat of path with err while leaving other
// paths on the real syscall.
func stubPresenceLstat(t *testing.T, path string, err error) {
	t.Helper()
	original := boundedLstatPath
	boundedLstatPath = func(p string) (os.FileInfo, error) {
		if p == path {
			return nil, err
		}
		return original(p)
	}
	t.Cleanup(func() { boundedLstatPath = original })
}

func TestProbeWorktreePresence(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, gw *GitWorktree, path string)
		want  WorktreePresence
	}{
		{
			name:  "existing directory is present",
			setup: func(t *testing.T, gw *GitWorktree, path string) { require.NoError(t, os.Mkdir(path, 0o755)) },
			want:  WorktreePresencePresent,
		},
		{
			name:  "removed directory is absent",
			setup: func(t *testing.T, gw *GitWorktree, path string) {},
			want:  WorktreePresenceAbsent,
		},
		{
			name: "non-ENOENT stat error is unknown",
			setup: func(t *testing.T, gw *GitWorktree, path string) {
				stubPresenceLstat(t, path, &os.PathError{Op: "lstat", Path: path, Err: syscall.EACCES})
			},
			want: WorktreePresenceUnknown,
		},
		{
			name: "active relocation claim is unknown",
			setup: func(t *testing.T, gw *GitWorktree, path string) {
				gw.relocationMu.Lock()
				gw.activeRelocationClaim = &RelocationClaim{Path: path}
				gw.relocationMu.Unlock()
			},
			want: WorktreePresenceUnknown,
		},
		{
			name: "durable recovery record is unknown",
			setup: func(t *testing.T, gw *GitWorktree, path string) {
				require.NoError(t, gw.RestoreRelocationRecovery(RelocationRecovery{State: RelocationRecoveryStalled}))
			},
			want: WorktreePresenceUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(testguard.CanonicalTempDir(t), "wt")
			gw := presenceTestWorktree(t, path)
			tt.setup(t, gw, path)
			assert.Equal(t, tt.want, gw.ProbeWorktreePresence())
		})
	}

	t.Run("empty path is unknown", func(t *testing.T) {
		assert.Equal(t, WorktreePresenceUnknown, (&GitWorktree{}).ProbeWorktreePresence())
	})
}

func TestRepointAbsentWorktreePath(t *testing.T) {
	t.Run("absent source and free destination repoint the record", func(t *testing.T) {
		root := testguard.CanonicalTempDir(t)
		gw := presenceTestWorktree(t, filepath.Join(root, "gone"))
		dest := filepath.Join(root, "rebuilt")
		require.NoError(t, gw.RepointAbsentWorktreePath(dest))
		assert.Equal(t, dest, gw.GetWorktreePath())
		_, err := os.Lstat(dest)
		assert.ErrorIs(t, err, os.ErrNotExist, "repoint rewrites the record only; it creates nothing")
	})

	t.Run("present source refuses", func(t *testing.T) {
		root := testguard.CanonicalTempDir(t)
		src := filepath.Join(root, "here")
		require.NoError(t, os.Mkdir(src, 0o755))
		gw := presenceTestWorktree(t, src)
		require.Error(t, gw.RepointAbsentWorktreePath(filepath.Join(root, "dest")))
		assert.Equal(t, src, gw.GetWorktreePath())
	})

	t.Run("occupied destination refuses", func(t *testing.T) {
		root := testguard.CanonicalTempDir(t)
		src := filepath.Join(root, "gone")
		dest := filepath.Join(root, "occupied")
		require.NoError(t, os.Mkdir(dest, 0o755))
		gw := presenceTestWorktree(t, src)
		require.Error(t, gw.RepointAbsentWorktreePath(dest))
		assert.Equal(t, src, gw.GetWorktreePath())
	})

	t.Run("unanswerable source refuses", func(t *testing.T) {
		root := testguard.CanonicalTempDir(t)
		src := filepath.Join(root, "eacces")
		stubPresenceLstat(t, src, &os.PathError{Op: "lstat", Path: src, Err: syscall.EACCES})
		gw := presenceTestWorktree(t, src)
		require.Error(t, gw.RepointAbsentWorktreePath(filepath.Join(root, "dest")))
		assert.Equal(t, src, gw.GetWorktreePath())
	})

	t.Run("outstanding recovery record refuses", func(t *testing.T) {
		root := testguard.CanonicalTempDir(t)
		src := filepath.Join(root, "gone")
		gw := presenceTestWorktree(t, src)
		require.NoError(t, gw.RestoreRelocationRecovery(RelocationRecovery{State: RelocationRecoveryStalled}))
		err := gw.RepointAbsentWorktreePath(filepath.Join(root, "dest"))
		require.ErrorIs(t, err, ErrRelocateStateUnknown)
		assert.Equal(t, src, gw.GetWorktreePath())
	})
}

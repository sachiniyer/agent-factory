package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func wsGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestMeasureWorkspace(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		f := MeasureWorkspace(filepath.Join(t.TempDir(), "gone"))
		assert.False(t, f.Exists)
		assert.Empty(t, f.Unverified)
	})

	t.Run("clean repo with a remote", func(t *testing.T) {
		origin := t.TempDir()
		wsGit(t, origin, "init", "-q", "--bare")
		dir := t.TempDir()
		wsGit(t, dir, "init", "-q", "-b", "main")
		wsGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "base")
		wsGit(t, dir, "remote", "add", "origin", origin)
		wsGit(t, dir, "push", "-q", "origin", "main")
		wsGit(t, dir, "fetch", "-q", "origin")
		f := MeasureWorkspace(dir)
		assert.Equal(t, WorkspaceFacts{Exists: true}, f)
	})

	t.Run("dirty, unpushed, extra branch, stash", func(t *testing.T) {
		dir := t.TempDir()
		wsGit(t, dir, "init", "-q", "-b", "main")
		id := []string{"-c", "user.name=t", "-c", "user.email=t@t"}
		wsGit(t, dir, append(id, "commit", "-q", "--allow-empty", "-m", "one")...)
		wsGit(t, dir, "branch", "side")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked"), []byte("a"), 0o644))
		wsGit(t, dir, "add", "tracked")
		wsGit(t, dir, append(id, "stash", "push", "-q")...)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked"), []byte("b"), 0o644))
		f := MeasureWorkspace(dir)
		assert.True(t, f.Exists)
		assert.Equal(t, 1, f.DirtyFiles)
		assert.Equal(t, 1, f.UnpushedCommits)
		assert.Equal(t, 1, f.ExtraBranches)
		assert.Equal(t, 1, f.Stashes)
		assert.Empty(t, f.Unverified)
	})

	t.Run("not a git repo is unverified, never clean", func(t *testing.T) {
		f := MeasureWorkspace(t.TempDir())
		assert.True(t, f.Exists)
		assert.NotEmpty(t, f.Unverified)
	})
}

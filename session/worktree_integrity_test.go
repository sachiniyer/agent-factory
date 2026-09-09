package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func worktreeScanGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s: %s", cmd.String(), string(out))
}

func TestInspectSessionWorktreesNamesTheOtherLiveLane(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644))
	worktreeScanGit(t, repo, "add", "--all")
	worktreeScanGit(t, repo, "commit", "-q", "-m", "base")
	holder := filepath.Join(filepath.Dir(repo), "holder")
	sibling := filepath.Join(filepath.Dir(repo), "sibling")
	worktreeScanGit(t, repo, "worktree", "add", "-q", "-b", "shared", holder, "HEAD")
	worktreeScanGit(t, repo, "worktree", "add", "-q", "-b", "takeover", sibling, "HEAD")
	worktreeScanGit(t, sibling, "checkout", "-q", "-B", "shared", "shared")

	rows := []InstanceData{
		{ID: "holder-id", Title: "holder", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: holder}},
		{ID: "takeover-id", Title: "takeover", Liveness: LiveReady, BackendType: "local", Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: sibling}},
	}
	got := InspectSessionWorktrees(rows)
	require.Len(t, got, 2)
	assert.Contains(t, got[0].Warning, `live lane(s) "takeover"`)
	assert.Contains(t, got[1].Warning, `live lane(s) "holder"`)

	rows[1].Liveness = LiveArchived
	got = InspectSessionWorktrees(rows)
	require.Len(t, got, 1, "an archived worktree retains its ref but is not a live collision")
	assert.Empty(t, got[0].Warning)
}

func TestInspectSessionWorktreesIgnoresOrdinaryFullyStagedChange(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	for i := 0; i < 25; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(repo, fmt.Sprintf("file-%02d.txt", i)), []byte("base\n"), 0o644))
	}
	worktreeScanGit(t, repo, "add", "--all")
	worktreeScanGit(t, repo, "commit", "-q", "-m", "base")
	for i := 0; i < 25; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(repo, fmt.Sprintf("file-%02d.txt", i)), []byte("intentional edit\n"), 0o644))
	}
	worktreeScanGit(t, repo, "add", "--all")

	got := InspectSessionWorktrees([]InstanceData{{
		ID: "editor-id", Title: "editor", Liveness: LiveReady, BackendType: "local",
		Worktree: GitWorktreeData{RepoPath: repo, WorktreePath: repo},
	}})
	require.Len(t, got, 1)
	assert.True(t, got[0].Evidence.MassRevert, "the raw index shape is present")
	assert.False(t, got[0].Evidence.HeadMovedWithoutReflog, "HEAD still agrees with this worktree's reflog")
	assert.Empty(t, got[0].Warning, "an ordinary fully staged commit must never be surfaced as a takeover danger")
}

func TestWorktreeWarningIsProjectionOnly(t *testing.T) {
	data := InstanceData{ID: "warning", Title: "lane", BackendType: "remote", Liveness: LiveReady, WorktreeWarning: "DANGER"}
	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	assert.Equal(t, "DANGER", restored.WorktreeWarning())
	assert.Empty(t, restored.ToInstanceData().ForStorage().WorktreeWarning)
	assert.Empty(t, data.ForClientRead().WorktreeWarning)
}

package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepositoryNamedProbes(t *testing.T) {
	for _, selector := range []string{"clean", "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR"} {
		t.Run(selector, func(t *testing.T) {
			repo, wt, branch := resetRepoWithWorktree(t, "owned")
			foreign, _, _ := resetRepoWithWorktree(t, "foreign")
			if selector != "clean" {
				value := filepath.Join(foreign, ".git")
				if selector == "GIT_WORK_TREE" {
					value = foreign
				}
				t.Setenv(selector, value)
			}
			g := &GitWorktree{repoPath: repo, worktreePath: wt, branchName: branch}
			require.NoError(t, g.SetHookEnvironment([]string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR"}))
			for _, path := range []string{wt, filepath.Join(t.TempDir(), "absent")} {
				want := path == wt
				t.Run(filepath.Base(path), func(t *testing.T) {
					registered, err := worktreeRegisteredIn(repo, path)
					require.NoError(t, err)
					assert.Equal(t, want, registered, "reset registration")
					g.worktreePath = path
					registered, err = g.isWorktreeRegistered()
					require.NoError(t, err)
					assert.Equal(t, want, registered, "diagnostics registration with passthrough")
					r := &cleanupRun{g: g}
					registered, known := r.registered()
					require.True(t, known)
					assert.Equal(t, want, registered, "cleanup registration with passthrough")
				})
			}
			g.worktreePath = wt
			assert.NoError(t, probeRepoGoneOrigin(context.Background(), g), "live origin must remain live under ambient selectors")
			r := &cleanupRun{g: g}
			assert.NoError(t, r.requireRegisteredBranchMatch(), "archive branch authorization")
			held, err := BranchesHeldByWorktrees(repo)
			require.NoError(t, err)
			assert.Equal(t, []string{normalizeWorktreePath(wt)}, held[branch])
			// Hook resume uses an empty GitWorktree, without session passthrough.
			output, err := (&GitWorktree{}).runGitCommandContext(context.Background(), repo, "worktree", "list", "--porcelain", "-z")
			require.NoError(t, err)
			registered, err := worktreeListed(output, wt)
			require.NoError(t, err)
			assert.True(t, registered)
		})
	}
}

func TestRepositoryNamedRemovalPreservesLockedWorktree(t *testing.T) {
	repo, wt, _ := resetRepoWithWorktree(t, "locked")
	foreign, _, _ := resetRepoWithWorktree(t, "foreign")
	out, err := exec.Command("git", "-C", repo, "worktree", "lock", wt).CombinedOutput()
	require.NoError(t, err, "%s", out)
	sentinel := filepath.Join(wt, "uncommitted.txt")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep me"), 0600))
	t.Setenv("GIT_DIR", filepath.Join(foreign, ".git"))
	removed, err := RemoveWorktreeDir(repo, wt)
	assert.False(t, removed)
	assert.ErrorIs(t, err, ErrWorktreeStillRegistered)
	content, readErr := os.ReadFile(sentinel)
	require.NoError(t, readErr, "locked worktree's uncommitted data must survive")
	assert.Equal(t, "keep me", string(content))
}

// Git itself supplies GIT_DIR to a post-commit hook in a linked worktree.
// The hook invokes only this read-only probe, never the reset command.
func TestRepositoryNamedProbeFromGitHook(t *testing.T) {
	if os.Getenv("AF_4198_HOOK_PROBE") == "1" {
		require.NotEmpty(t, os.Getenv("GIT_DIR"), "Git must supply the trigger")
		registered, err := worktreeRegisteredIn(os.Getenv("AF_4198_REPO"), os.Getenv("AF_4198_WORKTREE"))
		require.NoError(t, err)
		require.True(t, registered)
		return
	}
	repo, wt, _ := resetRepoWithWorktree(t, "owned")
	foreign, foreignWT, _ := resetRepoWithWorktree(t, "hook")
	executable, err := os.Executable()
	require.NoError(t, err)
	hook := "#!/bin/sh\nexec " + config.ShellQuotePath(executable) + " -test.run '^TestRepositoryNamedProbeFromGitHook$' -test.count=1\n"
	require.NoError(t, os.WriteFile(filepath.Join(foreign, ".git", "hooks", "post-commit"), []byte(hook), 0700))
	c := exec.Command("git", "-C", foreignWT, "-c", "core.hooksPath="+filepath.Join(foreign, ".git", "hooks"), "commit", "--allow-empty", "-m", "hook probe")
	c.Env = append(os.Environ(), "AF_4198_HOOK_PROBE=1", "AF_4198_REPO="+repo, "AF_4198_WORKTREE="+wt,
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := c.CombinedOutput()
	require.NoError(t, err, "%s", out)
	// post-commit failure does not change git commit's exit status.
	require.NotContains(t, string(out), "FAIL", "%s", out)
	require.Contains(t, string(out), "PASS", "%s", out)
}

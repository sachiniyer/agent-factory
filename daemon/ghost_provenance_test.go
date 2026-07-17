package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/require"
)

func ghostGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := c.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, string(out))
}

func ghostBranchExists(repo, branch string) bool {
	return exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

// TestGhostCleanupWorktree_LegacyNilProvenance_PreservesUserBranch covers the
// third copy of the nil→provenance default (#1953), in daemon-side ghost
// teardown. Every other daemon test STUBS ghostCleanupWorktree, so its real body
// — and its own duplicated default — had no lock at all; that is how it rotted
// out of step with the reset call site.
//
// The ExternalWorktree bail at the top of ghostCleanupWorktree already covers the
// shape the issue describes. It does NOT cover this one: a normal, non-external
// AF linked worktree that Setup built on a branch the user already had
// (setupFromExistingBranch, 2025-07-23), whose pre-2026-04-17 record persisted no
// flag. Unknown provenance must not reach `git branch -D`.
//
// This calls the real function against a real repo. It spawns no daemon.
func TestGhostCleanupWorktree_LegacyNilProvenance_PreservesUserBranch(t *testing.T) {
	repo := t.TempDir()
	ghostGit(t, repo, "init", "-q")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi"), 0644))
	ghostGit(t, repo, "add", "-A")
	ghostGit(t, repo, "commit", "-q", "-m", "init")
	ghostGit(t, repo, "branch", "-M", "master")
	ghostGit(t, repo, "branch", "user-feature")

	wt := filepath.Join(t.TempDir(), "wt")
	ghostGit(t, repo, "worktree", "add", "-q", wt, "user-feature")
	require.True(t, ghostBranchExists(repo, "user-feature"))

	ghostCleanupWorktree(&session.InstanceData{
		Title: "legacy-ghost",
		Path:  repo,
		Worktree: session.GitWorktreeData{
			RepoPath: repo, WorktreePath: wt, SessionName: "legacy-ghost",
			BranchName: "user-feature", ExternalWorktree: false, BranchCreatedByUs: nil,
		},
	}, "legacy-ghost")

	require.True(t, ghostBranchExists(repo, "user-feature"),
		"#1953: ghost cleanup force-deleted the user's pre-existing branch from a legacy record with no provenance")
	require.True(t, ghostBranchExists(repo, "master"), "master must survive")
}

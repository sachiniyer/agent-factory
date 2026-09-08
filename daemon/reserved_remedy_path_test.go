package daemon

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReservedTitleRemedyUsesRepoPath(t *testing.T) {
	m := newTitleAdmissionManager()
	repo := t.TempDir()
	for _, err := range []error{
		m.validateTitleAvailableLocked("repo", repo, "root", "claude", runtimeNamespaceLocalTmux, false, nil, false),
		m.validateTitleClaimableLocked("repo", repo, "root", "claude", runtimeNamespaceLocalTmux, false, nil, nil, false),
	} {
		require.Error(t, err)
		require.Contains(t, err.Error(), "af projects add "+config.ShellQuotePath(repo))
		require.Contains(t, err.Error(), "af config set --project "+config.ShellQuotePath(repo))
	}
}

func TestReservedTitleRemedyUsesLinkedWorktreeForBareRepo(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "source")
	bare := filepath.Join(base, "repo.git")
	workspace := filepath.Join(base, "worktree")
	require.NoError(t, exec.Command("git", "init", "-q", "-b", "master", source).Run())
	require.NoError(t, exec.Command("git", "-C", source, "config", "user.email", "test@example.com").Run())
	require.NoError(t, exec.Command("git", "-C", source, "config", "user.name", "test").Run())
	require.NoError(t, exec.Command("git", "-C", source, "commit", "-q", "--allow-empty", "-m", "initial").Run())
	require.NoError(t, exec.Command("git", "clone", "-q", "--bare", source, bare).Run())
	require.NoError(t, exec.Command("git", "-C", bare, "worktree", "add", "-q", workspace, "master").Run())

	m := newTitleAdmissionManager()
	err := m.validateTitleClaimableLocked("repo", workspace, "root", "claude", runtimeNamespaceLocalTmux, false, nil, nil, false)
	require.Error(t, err)
	quoted := config.ShellQuotePath(workspace)
	assert.Contains(t, err.Error(), "af projects add "+quoted)
	assert.Contains(t, err.Error(), "af config set --project "+quoted)
	assert.NotContains(t, err.Error(), config.ShellQuotePath(bare))

	resolved, err := exec.Command("git", "-C", workspace, "rev-parse", "--show-toplevel").Output()
	require.NoError(t, err)
	assert.Equal(t, workspace+"\n", string(resolved))
}

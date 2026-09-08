package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
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
	realBase := t.TempDir()
	linkParent := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(realBase, linkParent))
	source := filepath.Join(linkParent, "source")
	bare := filepath.Join(linkParent, "repo.git")
	workspace := filepath.Join(linkParent, "worktree")
	require.NoError(t, exec.Command("git", "init", "-q", "-b", "master", source).Run())
	require.NoError(t, exec.Command("git", "-C", source, "config", "user.email", "test@example.com").Run())
	require.NoError(t, exec.Command("git", "-C", source, "config", "user.name", "test").Run())
	require.NoError(t, exec.Command("git", "-C", source, "commit", "-q", "--allow-empty", "-m", "initial").Run())
	require.NoError(t, exec.Command("git", "clone", "-q", "--bare", source, bare).Run())
	require.NoError(t, exec.Command("git", "-C", bare, "worktree", "add", "-q", workspace, "master").Run())
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	require.NoError(t, err)
	resolvedBare, err := filepath.EvalSymlinks(bare)
	require.NoError(t, err)

	m := newTitleAdmissionManager()
	err = m.validateTitleClaimableLocked("repo", resolvedWorkspace, "root", "claude", runtimeNamespaceLocalTmux, false, nil, nil, false, resolvedWorkspace)
	require.Error(t, err)
	quoted := config.ShellQuotePath(resolvedWorkspace)
	assert.Contains(t, err.Error(), "af projects add "+quoted)
	assert.Contains(t, err.Error(), "af config set --project "+quoted)
	assert.NotContains(t, err.Error(), config.ShellQuotePath(resolvedBare))

	resolved, err := exec.Command("git", "-C", workspace, "rev-parse", "--show-toplevel").Output()
	require.NoError(t, err)
	assert.Equal(t, resolvedWorkspace+"\n", string(resolved))
}

func TestTitleAdmissionUsesIdentityRootForLinkedWorktreeNamespace(t *testing.T) {
	m := newTitleAdmissionManager()
	identityRoot := filepath.Join(t.TempDir(), "repo.git")
	workspace := filepath.Join(t.TempDir(), "worktree")
	m.reservedTmuxNames[daemonInstanceKey("repo", tmux.SanitizedNameForRepo("a b", identityRoot))] = "a b"

	err := m.validateTitleAvailableLocked("repo", identityRoot, "ab", "claude", runtimeNamespaceLocalTmux, false, nil, false, workspace)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a b")
}

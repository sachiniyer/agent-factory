package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gitIdentityTestRepo(t *testing.T) (repo, home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	repo = filepath.Join(t.TempDir(), "ws")
	out, err := exec.Command("git", "init", "-q", repo).CombinedOutput()
	require.NoError(t, err, string(out))
	return repo, home
}

func gitIdentityConfigValue(t *testing.T, repo, key string) string {
	t.Helper()
	out, _ := exec.Command("git", "-C", repo, "config", "--local", "--get", key).Output()
	return strings.TrimSpace(string(out))
}

// TestGitIdentityCommand_WritesRepoLocalOnly is the #5571 W2.7 property: the
// identity lands in the workspace's own config and the host's global gitconfig
// is never created or changed, nor given a safe.directory entry.
func TestGitIdentityCommand_WritesRepoLocalOnly(t *testing.T) {
	repo, home := gitIdentityTestRepo(t)
	global := filepath.Join(home, ".gitconfig")
	require.NoError(t, os.WriteFile(global, []byte("[core]\n\teditor = vi\n"), 0o600))
	before, err := os.ReadFile(global)
	require.NoError(t, err)

	out, err := exec.Command("sh", "-c", gitIdentityCommand(repo, "Ada Lovelace", "ada@example.test")).CombinedOutput()
	require.NoError(t, err, string(out))

	assert.Equal(t, "Ada Lovelace", gitIdentityConfigValue(t, repo, "user.name"))
	assert.Equal(t, "ada@example.test", gitIdentityConfigValue(t, repo, "user.email"))
	after, err := os.ReadFile(global)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "the host's global gitconfig must be byte-identical")
	assert.NotContains(t, gitIdentityCommand(repo, "a", "b"), "--global")
	assert.NotContains(t, gitIdentityCommand(repo, "a", "b"), "safe.directory")
}

func TestGitIdentityCommand_FallsBackToPlaceholder(t *testing.T) {
	repo, _ := gitIdentityTestRepo(t)
	out, err := exec.Command("sh", "-c", gitIdentityCommand(repo, " ", "")).CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, defaultGitUserName, gitIdentityConfigValue(t, repo, "user.name"))
	assert.Equal(t, defaultGitUserEmail, gitIdentityConfigValue(t, repo, "user.email"))
}

func TestGitIdentityCommand_QuotesHostileValues(t *testing.T) {
	repo, _ := gitIdentityTestRepo(t)
	name := `O'Brien $(touch pwned) "x"`
	out, err := exec.Command("sh", "-c", gitIdentityCommand(repo, name, "o@example.test")).CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, name, gitIdentityConfigValue(t, repo, "user.name"))
}

func TestOperatorGitIdentity_ReadsTheDaemonHostConfig(t *testing.T) {
	repo, home := gitIdentityTestRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("[user]\n\tname = Grace Hopper\n\temail = grace@example.test\n"), 0o600))
	name, email := operatorGitIdentity(repo)
	assert.Equal(t, "Grace Hopper", name)
	assert.Equal(t, "grace@example.test", email)

	emptyRepo, _ := gitIdentityTestRepo(t) // re-points HOME at a fresh, config-less one
	name, email = operatorGitIdentity(emptyRepo)
	assert.Empty(t, name)
	assert.Empty(t, email)
}

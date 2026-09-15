package git

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepositoryPathEnvironmentPreservesRuntime(t *testing.T) {
	keep := []string{"PATH=/custom/bin", "HOME=/custom/home", "GIT_CONFIG_GLOBAL=/custom/gitconfig",
		"GIT_CONFIG_SYSTEM=/custom/system", "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH_COMMAND=custom-ssh",
		"GIT_AUTHOR_NAME=author", "CUSTOM_HOOK_TOKEN=opaque", "EMPTY="}
	source := append(append([]string(nil), keep...), "GIT_DIR=/foreign", "GIT_DIR=/other", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.worktree", "GIT_CONFIG_VALUE_0=/foreign")
	original := append([]string(nil), source...)
	require.Equal(t, keep, repositoryPathEnvironment(source))
	require.Equal(t, original, source, "must not mutate a shared environment slice")
	require.NotNil(t, repositoryPathEnvironment(nil), "nil Cmd.Env would inherit ambient selectors")
}

func TestRepositoryPathEnvironmentCoversGitLocalVariables(t *testing.T) {
	out, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
	require.NoError(t, err)
	names := strings.Fields(string(out))
	require.Contains(t, names, "GIT_DIR")
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			require.Empty(t, repositoryPathEnvironment([]string{name + "=foreign"}), "new Git local variable needs classification")
		})
	}
}

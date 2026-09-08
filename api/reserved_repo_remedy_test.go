package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

func TestSessionsCreateReservedTitleNamesResolvedRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	silenceStdio(t)
	caller := t.TempDir()
	t.Chdir(caller)
	resolvedCaller, err := filepath.EvalSymlinks(caller)
	require.NoError(t, err)
	for _, name := range []string{"B", "B's repo"} {
		t.Run(name, func(t *testing.T) {
			// macOS resolves /var to /private/var; exercise a symlinked parent on every platform.
			real := t.TempDir()
			link := filepath.Join(t.TempDir(), "link")
			require.NoError(t, os.Symlink(real, link))
			repo := filepath.Join(link, name)
			require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
			setSessionsCreateFlags(t, "", repo, false, false)
			err := sessionsCreateCmd.RunE(sessionsCreateCmd, []string{"root"})
			require.Error(t, err)
			resolvedRepo, resolveErr := filepath.EvalSymlinks(repo)
			require.NoError(t, resolveErr)
			quoted := config.ShellQuotePath(resolvedRepo)
			require.Contains(t, err.Error(), "af projects add "+quoted)
			require.Contains(t, err.Error(), "af config set --project "+quoted+` root_agent '{"enabled":true}'`)
			require.NotContains(t, err.Error(), config.ShellQuotePath(repo))
			require.NotContains(t, err.Error(), caller)
			require.NotContains(t, err.Error(), resolvedCaller)
			require.NotContains(t, err.Error(), "af projects add .")
			require.NotContains(t, err.Error(), "--project .")
		})
	}
}

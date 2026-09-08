package api

import (
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
	for _, name := range []string{"B", "B's repo"} {
		t.Run(name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), name)
			require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
			setSessionsCreateFlags(t, "", repo, false, false)
			err := sessionsCreateCmd.RunE(sessionsCreateCmd, []string{"root"})
			require.Error(t, err)
			quoted := config.ShellQuotePath(repo)
			require.Contains(t, err.Error(), "af projects add "+quoted)
			require.Contains(t, err.Error(), "af config set --project "+quoted+` root_agent '{"enabled":true}'`)
			require.NotContains(t, err.Error(), caller)
			require.NotContains(t, err.Error(), "af projects add .")
			require.NotContains(t, err.Error(), "--project .")
		})
	}
}

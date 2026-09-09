package commands

import (
	"bytes"
	"os"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestRootAgentRemedyPreservesPersonalProgram(t *testing.T) {
	_, repo := setupConfigExplainCommandTest(t, "schema_version = 1\n")
	t.Setenv("AF_DAEMON_URL", "")
	project, err := config.RegisterProject(repo)
	require.NoError(t, err)
	path, err := config.ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("[root_agent]\nenabled = false\nprogram = 'claude --model opus'\n"), 0644))
	t.Chdir(repo)
	oldProject := configSetProjectFlag
	configSetProjectFlag = "."
	t.Cleanup(func() { configSetProjectFlag = oldProject })
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, configSetCmd.RunE(cmd, []string{"root_agent", `{"enabled":true}`}))
	require.Contains(t, out.String(), "already-running root session is adopted as-is")
	require.Contains(t, out.String(), "program change also requires killing that session")
	cfg, err := config.LoadProjectConfig(project.ID)
	require.NoError(t, err)
	require.True(t, cfg.RootAgent.Enabled)
	require.Equal(t, "claude --model opus", cfg.RootAgent.Program)
}

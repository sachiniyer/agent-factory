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
	require.Contains(t, out.String(), "changing its program")
	require.Contains(t, out.String(), "disabling it")
	require.Contains(t, out.String(), "removing its enabling entry")
	cfg, err := config.LoadProjectConfig(project.ID)
	require.NoError(t, err)
	require.True(t, cfg.RootAgent.Enabled)
	require.Equal(t, "claude --model opus", cfg.RootAgent.Program)
}

func TestRootAgentDottedProjectUnsetNamesAdoptedSessionRemedy(t *testing.T) {
	_, repo := setupConfigExplainCommandTest(t, "schema_version = 1\n")
	t.Setenv("AF_DAEMON_URL", "")
	project, err := config.RegisterProject(repo)
	require.NoError(t, err)
	path, err := config.ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("[root_agent]\nenabled = true\nprogram = 'codex'\n"), 0644))
	t.Chdir(repo)
	oldProject := configUnsetProjectFlag
	configUnsetProjectFlag = "."
	t.Cleanup(func() { configUnsetProjectFlag = oldProject })
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, configUnsetCmd.RunE(cmd, []string{"root_agent.program"}))
	require.Contains(t, out.String(), "restart them to apply")
	require.Contains(t, out.String(), "already-running root session is adopted as-is")
	require.Contains(t, out.String(), "changing its program")
	require.Contains(t, out.String(), "disabling it")
	require.Contains(t, out.String(), "removing its enabling entry")
}

func TestRootAgentDottedDisableNamesAdoptedSessionRemedy(t *testing.T) {
	_, repo := setupConfigExplainCommandTest(t, "schema_version = 1\n")
	t.Setenv("AF_DAEMON_URL", "")
	_, err := config.RegisterProject(repo)
	require.NoError(t, err)
	t.Chdir(repo)
	oldProject := configSetProjectFlag
	configSetProjectFlag = "."
	t.Cleanup(func() { configSetProjectFlag = oldProject })
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, configSetCmd.RunE(cmd, []string{"root_agent.enabled", "false"}))
	require.Contains(t, out.String(), "disabling it")
	require.Contains(t, out.String(), "requires killing that session")
}

func TestRootAgentsWholeTableRemovalNamesAdoptedSessionRemedy(t *testing.T) {
	_, repo := setupConfigExplainCommandTest(t,
		"schema_version = 1\n[root_agents]\n\"/tmp/example\" = { program = 'codex' }\n")
	t.Setenv("AF_DAEMON_URL", "")
	t.Chdir(repo)
	oldProject := configSetProjectFlag
	configSetProjectFlag = ""
	t.Cleanup(func() { configSetProjectFlag = oldProject })
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, configSetCmd.RunE(cmd, []string{"root_agents", "{}"}))
	require.Contains(t, out.String(), "removing its enabling entry")
	require.Contains(t, out.String(), "requires killing that session")
}

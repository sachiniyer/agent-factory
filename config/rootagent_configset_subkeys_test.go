package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSetGlobalRootAgentDottedSubkeysMerge pins the convenient scalar forms to
// the same table-preserving edit contract as program_overrides.<agent> and
// limit_patterns.<name>.
func TestSetGlobalRootAgentDottedSubkeysMerge(t *testing.T) {
	path := writeTempConfig(t, "# keep me\n[root_agent]\nenabled = false\nprogram = 'claude --model opus'\n")

	result, err := SetGlobalConfigValue("root_agent.enabled", "true")
	require.NoError(t, err)
	require.Equal(t, "true", result.Value)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.True(t, cfg.RootAgent.Enabled)
	require.Equal(t, "claude --model opus", cfg.RootAgent.Program,
		"editing enabled must preserve the existing program field")

	result, err = SetGlobalConfigValue("root_agent.program", "codex --profile work")
	require.NoError(t, err)
	require.Equal(t, "codex --profile work", result.Value)
	cfg, err = LoadConfig()
	require.NoError(t, err)
	require.True(t, cfg.RootAgent.Enabled, "editing program must preserve enabled")
	require.Equal(t, "codex --profile work", cfg.RootAgent.Program)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(written), "# keep me", "the scalar merge must preserve unrelated comments")
}

func TestSetGlobalRootAgentEnabledRejectsInvalidBoolWithoutWriting(t *testing.T) {
	path := writeTempConfig(t, "[root_agent]\nenabled = false\nprogram = 'claude'\n")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	_, err = SetGlobalConfigValue("root_agent.enabled", "sometimes")
	require.ErrorContains(t, err, "boolean")
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, string(before), string(after))
}

func TestSetProjectRootAgentDottedSubkeysMerge(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[root_agent]\nenabled = false\nprogram = 'claude --model opus'\n")

	_, err := SetProjectConfigValue(project.ID, "root_agent.enabled", "true")
	require.NoError(t, err)
	_, err = SetProjectConfigValue(project.ID, "root_agent.program", "codex --profile work")
	require.NoError(t, err)

	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	require.True(t, cfg.RootAgent.Enabled)
	require.Equal(t, "codex --profile work", cfg.RootAgent.Program)
}

func TestRootAgentSettableFormsAreDiscoverable(t *testing.T) {
	keys := strings.Join(SettableKeys(), ",")
	require.Contains(t, keys, "root_agent")
	require.Contains(t, keys, "root_agent.enabled")
	require.Contains(t, keys, "root_agent.program")
}

func TestUnsettableTableSubkeyNamesWholeTableForm(t *testing.T) {
	writeTempConfig(t, "default_program = 'claude'\n")
	for _, key := range []string{"root_agent.unknown", "root_agents.some-repo"} {
		parent, _, _ := strings.Cut(key, ".")
		_, err := SetGlobalConfigValue(key, "value")
		require.Error(t, err)
		require.Contains(t, err.Error(), "whole table")
		require.Contains(t, err.Error(), "af config set "+parent)
	}
}

func TestUnsettableProjectTableSubkeyHintPreservesSelector(t *testing.T) {
	_, repoRoot, _ := registeredTestProject(t)
	want := "af config set root_agent '<compact-json>' --project " + ShellQuotePath(repoRoot)
	for _, attempt := range []struct {
		name string
		run  func() error
	}{
		{"set", func() error {
			_, err := SetProjectConfigValue(repoRoot, "root_agent.progarm", "codex")
			return err
		}},
		{"unset", func() error {
			_, err := UnsetProjectConfigValue(repoRoot, "root_agent.progarm")
			return err
		}},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			err := attempt.run()
			require.Error(t, err)
			require.Contains(t, err.Error(), want)
		})
	}
}

func TestUnsettableProjectGlobalTableSubkeyHintOmitsSelector(t *testing.T) {
	_, repoRoot, _ := registeredTestProject(t)

	_, err := SetProjectConfigValue(repoRoot, "root_agents.unknown", "codex")
	require.Error(t, err)
	require.Contains(t, err.Error(), "af config set root_agents '<compact-json>'")
	require.NotContains(t, err.Error(), "af config set root_agents '<compact-json>' --project",
		"the suggested whole-table command must not preserve a scope the parent rejects")
}

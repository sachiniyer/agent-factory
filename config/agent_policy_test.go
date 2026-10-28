package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The agent-server property (#5571 W2.5): with a policy installed, no config
// entry point reads or creates anything under the host's af home, and each
// answers from the daemon's resolution.
func TestInstalledAgentPolicyNeverTouchesTheHostAFHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Cleanup(ClearAgentPolicy)

	data, err := AgentPolicy{
		Version: AgentPolicyVersion,
		Global:  PolicyLayer{BranchPrefix: "ops/", GlobalAgentSkills: true, WorktreeRoot: WorktreeRootSubdirectory, SessionEnvPassthrough: []string{"MY_TOKEN_NAME"}},
		Repo:    PolicyLayer{BranchPrefix: "ops/", ProgramOverrides: map[string]string{"claude": "claude --model x"}},
		Hooks:   PolicyHooks{PostWorktreeCommands: []string{"make deps"}},
		Source:  PolicySource{ProjectRoot: "/daemon/repo"},
	}.Marshal()
	require.NoError(t, err)
	policy, err := ParseAgentPolicy(data)
	require.NoError(t, err)
	InstallAgentPolicy(policy)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.True(t, cfg.GlobalAgentSkills, "global_agent_skills comes from the daemon, not the host")
	assert.Equal(t, "ops/", cfg.BranchPrefix)
	assert.Equal(t, WorktreeRootSubdirectory, cfg.WorktreeRoot)
	assert.Equal(t, []string{"MY_TOKEN_NAME"}, cfg.SessionEnvPassthrough)

	global, err := ResolveGlobalConfig()
	require.NoError(t, err)
	assert.True(t, global.GlobalAgentSkills)

	for name, resolve := range map[string]func() (*ResolvedConfig, error){
		"ResolveConfig":        func() (*ResolvedConfig, error) { return ResolveConfig(t.TempDir()) },
		"ResolveConfigForRepo": func() (*ResolvedConfig, error) { return ResolveConfigForRepo(&RepoContext{}) },
	} {
		resolved, err := resolve()
		require.NoError(t, err, name)
		assert.Equal(t, []string{"make deps"}, resolved.PostWorktreeCommands, name)
		assert.Equal(t, "claude --model x", resolved.ProgramOverrides["claude"], name)
	}

	_, statErr := os.Stat(home)
	assert.True(t, os.IsNotExist(statErr), "the host af home must not be created, let alone a config.toml in it: %v", statErr)
}

// Without a policy the loader is untouched (the control for the test above:
// it does create the home, so the assertion there is not vacuous).
func TestLoadConfigWithoutPolicyStillMaterializesTheHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)
	ClearAgentPolicy()
	_, err := LoadConfig()
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(home, TomlConfigFileName))
	assert.NoError(t, statErr)
}

// The daemon's auto-detected built-in program override is an absolute path on
// the DAEMON's filesystem and must not travel; an operator-authored one must.
func TestNewAgentPolicyDropsBuiltInProgramOverrides(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(t.TempDir(), "af-home"))
	cfg, err := LoadConfig()
	require.NoError(t, err)
	cfg.ProgramOverrides = map[string]string{"codex": "codex --yolo"}
	require.NoError(t, SaveConfig(cfg))
	resolved, err := ResolveGlobalConfig()
	require.NoError(t, err)
	policy := NewAgentPolicy(resolved, resolved)
	assert.Equal(t, "codex --yolo", policy.Global.ProgramOverrides["codex"])
	for agent, command := range policy.Global.ProgramOverrides {
		assert.NotContains(t, command, "/usr/", "built-in detected paths must not travel: %s=%s", agent, command)
	}
}

func TestParseAgentPolicyRefusesWhatItDoesNotKnow(t *testing.T) {
	_, err := ParseAgentPolicy([]byte(`{"version":2}`))
	require.ErrorContains(t, err, "version 2")
	_, err = ParseAgentPolicy([]byte(`{"version":1,"sandbox_ssh":"ssh evil"}`))
	require.Error(t, err, "an unknown field is refused, not dropped")
	_, err = ParseAgentPolicy([]byte(`not json`))
	require.Error(t, err)
}

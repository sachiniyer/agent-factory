package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseConfigRootAgents covers the #1106 opt-in surface: root_agents
// parses from the global config with per-repo profiles, defaults to empty
// (no repo ever gets a surprise always-on agent), and preserves custom programs.
func TestParseConfigRootAgents(t *testing.T) {
	cfg, err := parseConfig([]byte(`{
		"default_program": "claude",
		"root_agents": {
			"/home/me/repo": {},
			"~/other": {"program": "claude --model opus"}
		}
	}`), "config.json")
	require.NoError(t, err)
	require.Len(t, cfg.RootAgents, 2)

	def := cfg.RootAgents["/home/me/repo"]
	assert.Empty(t, def.Program)

	custom := cfg.RootAgents["~/other"]
	assert.Equal(t, "claude --model opus", custom.Program)
}

// TestDefaultConfigHasNoRootAgents pins the conservative default: nothing is
// opted in until the user edits config.json.
func TestDefaultConfigHasNoRootAgents(t *testing.T) {
	assert.Empty(t, DefaultConfig().RootAgents)
}

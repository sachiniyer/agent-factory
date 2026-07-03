package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseConfigRootAgents covers the #1106 opt-in surface: root_agents
// parses from the global config with per-repo profiles, defaults to empty
// (no repo ever gets a surprise always-on agent), and the auto_yes profile
// flag defaults to TRUE only when left unset.
func TestParseConfigRootAgents(t *testing.T) {
	cfg, err := parseConfig([]byte(`{
		"default_program": "claude",
		"root_agents": {
			"/home/me/repo": {},
			"~/other": {"program": "claude --model opus", "auto_yes": false}
		}
	}`), "config.json")
	require.NoError(t, err)
	require.Len(t, cfg.RootAgents, 2)

	def := cfg.RootAgents["/home/me/repo"]
	assert.Empty(t, def.Program)
	assert.True(t, def.AutoYesEnabled(), "unset auto_yes must default to true for the root profile")

	custom := cfg.RootAgents["~/other"]
	assert.Equal(t, "claude --model opus", custom.Program)
	assert.False(t, custom.AutoYesEnabled(), "an explicit auto_yes=false must be honored")
}

// TestDefaultConfigHasNoRootAgents pins the conservative default: nothing is
// opted in until the user edits config.json.
func TestDefaultConfigHasNoRootAgents(t *testing.T) {
	assert.Empty(t, DefaultConfig().RootAgents)
}

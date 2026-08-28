package config

import (
	"strings"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"

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

// TestLegacyRootAgentsLoadWarning is the migration UX for #3222. The legacy
// path map remains a permanent compatibility input, but every source carrying
// it gets one actionable warning that names the existing singleton successor.
func TestLegacyRootAgentsLoadWarning(t *testing.T) {
	warnings := captureLog(t, &aflog.WarningLog)
	const source = "legacy-root-agents.toml"
	data := []byte("[root_agents.\"/home/me/repo\"]\nprogram = \"codex\"\n")

	cfg, err := parseConfigTOML(data, source)
	require.NoError(t, err)
	require.Contains(t, cfg.RootAgents, "/home/me/repo",
		"the deprecation warning must not change legacy decoding")
	_, err = parseConfigTOML(data, source)
	require.NoError(t, err)

	const want = "config legacy-root-agents.toml: root_agents is the legacy path map; use [root_agent], the current project profile, for new configuration; for exact per-path equivalence, register the project and set enabled = true plus the optional program in its personal [root_agent] config; no file was rewritten"
	assert.Equal(t, 1, strings.Count(warnings.String(), want),
		"repeat loads of one shared config file must not flood the daemon log")
}

func TestLegacyRootAgentsAbsentDoesNotWarn(t *testing.T) {
	warnings := captureLog(t, &aflog.WarningLog)

	_, err := parseConfigTOML([]byte("[root_agent]\nenabled = false\n"), "current-root-agent.toml")
	require.NoError(t, err)
	assert.NotContains(t, warnings.String(), "legacy path map")
}

func TestEmptyLegacyRootAgentsDoesNotWarn(t *testing.T) {
	cases := []struct {
		name  string
		parse func() (*Config, error)
	}{
		{"empty TOML table", func() (*Config, error) {
			return parseConfigTOML([]byte("[root_agents]\n"), "empty-root-agents.toml")
		}},
		{"null JSON map", func() (*Config, error) {
			return parseConfig([]byte(`{"root_agents":null}`), "empty-root-agents.json")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := tc.parse()
			require.NoError(t, err)
			assert.NotContains(t, warnings.String(), "legacy path map")
		})
	}
}

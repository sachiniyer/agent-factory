package config

import (
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/require"
)

func TestRootAgentStructuredWriteMergesFields(t *testing.T) {
	for _, source := range []string{
		"[root_agent]\nenabled = true\nprogram = 'claude --model opus'\nfuture_policy = 'keep'\n",
		"root_agent = { enabled = true, program = 'claude --model opus', future_policy = 'keep' }\n",
	} {
		for _, tc := range []struct {
			name, patch string
			enabled     bool
			program     string
		}{
			{"disable preserves program", "[root_agent]\nenabled = false\n", false, "claude --model opus"},
			{"clear program preserves enabled", "[root_agent]\nprogram = ''\n", true, ""},
			{"empty patch preserves profile", "[root_agent]\n", true, "claude --model opus"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				updated, err := setTOMLStructured(source, "root_agent", tc.patch)
				require.NoError(t, err)
				var result map[string]map[string]any
				require.NoError(t, toml.Unmarshal([]byte(updated), &result))
				require.Equal(t, tc.enabled, result["root_agent"]["enabled"])
				require.Equal(t, tc.program, result["root_agent"]["program"])
				require.Equal(t, "keep", result["root_agent"]["future_policy"])
			})
		}
	}
}

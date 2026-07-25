package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func candidateBySource(res RootAgentResolution, source RootAgentSource) (RootAgentCandidate, bool) {
	for _, c := range res.Candidates {
		if c.Source == source {
			return c, true
		}
	}
	return RootAgentCandidate{}, false
}

func TestResolveRootAgentBuiltInDisabledByDefault(t *testing.T) {
	res := ResolveRootAgent(RootAgentInputs{})
	assert.False(t, res.Enabled, "with no source configured, root agents stay opt-in off")
	assert.Equal(t, "", res.Program)

	builtin, ok := candidateBySource(res, RootAgentSourceBuiltIn)
	require.True(t, ok)
	assert.True(t, builtin.Present)
	assert.False(t, builtin.Enabled)
	assert.Equal(t, "base", builtin.Result)

	legacy, ok := candidateBySource(res, RootAgentSourceLegacy)
	require.True(t, ok, "the trace names the legacy source even when absent")
	assert.False(t, legacy.Present)
	assert.Equal(t, "absent", legacy.Result)
}

// TestResolveRootAgentLegacyEntryResolvesUnchanged is the load-bearing
// compatibility proof: every existing path-keyed root_agents entry resolves to
// exactly {Enabled: true, Program: entry.Program} — the same profile the daemon
// used when it read the map directly. If this table ever diverges, an existing
// user's always-ensured root would change.
func TestResolveRootAgentLegacyEntryResolvesUnchanged(t *testing.T) {
	cases := []struct {
		name  string
		entry RootAgentConfig
	}{
		{"default profile (empty program)", RootAgentConfig{}},
		{"bare agent name", RootAgentConfig{Program: "claude"}},
		{"full command with flags", RootAgentConfig{Program: "claude --model opus --dangerously-skip-permissions"}},
		{"a non-claude program", RootAgentConfig{Program: "/usr/local/bin/codex --profile work"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := tc.entry
			res := ResolveRootAgent(RootAgentInputs{Legacy: &entry})

			assert.True(t, res.Enabled, "a present legacy entry is always an enabled root")
			assert.Equal(t, tc.entry.Program, res.Program, "the effective program is the legacy program verbatim")
			assert.Equal(t, RootAgent{Enabled: true, Program: tc.entry.Program}, res.RootAgent)

			legacy, ok := candidateBySource(res, RootAgentSourceLegacy)
			require.True(t, ok)
			assert.True(t, legacy.Present)
			assert.True(t, legacy.Enabled)
			assert.Equal(t, tc.entry.Program, legacy.Program)
			assert.Equal(t, "winner", legacy.Result)
		})
	}
}

// TestResolveRootAgentMatchesDirectMapValueForEveryEntry pins the equivalence
// the daemon relies on: routing an entry through the resolver yields the same
// program the old direct map read passed to ensureRootAgent.
func TestResolveRootAgentMatchesDirectMapValueForEveryEntry(t *testing.T) {
	legacyMap := map[string]RootAgentConfig{
		"/home/me/repo-a": {},
		"~/work/repo-b":   {Program: "claude --model opus"},
		"/srv/repo-c":     {Program: "codex"},
	}
	for path, entry := range legacyMap {
		entry := entry
		res := ResolveRootAgent(RootAgentInputs{Legacy: &entry})
		require.True(t, res.Enabled, "%s: a configured legacy entry must ensure a root", path)
		assert.Equal(t, entry.Program, res.Program,
			"%s: the resolver must hand ensureRootAgent the same program the map held", path)
	}
}

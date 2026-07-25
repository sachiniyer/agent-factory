package config

import (
	"encoding/json"
	"testing"

	"github.com/pelletier/go-toml/v2"
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

// TestResolveRootAgentEmptyLegacyProgramIsUnset pins P3-a: an empty legacy
// program is UNSET, not set-to-empty, so once a lower-precedence layer supplies a
// program (PR2's global layer) the empty legacy entry does not clobber it back to
// empty. In PR1 the observable form is that an empty entry leaves Program "" and
// the legacy candidate records that its program is unset; a non-empty entry sets
// it. A regression to unconditional assignment drops the "unset" reason.
func TestResolveRootAgentEmptyLegacyProgramIsUnset(t *testing.T) {
	empty := ResolveRootAgent(RootAgentInputs{Legacy: &RootAgentConfig{}})
	assert.True(t, empty.Enabled, "a present legacy entry still enables the root")
	assert.Equal(t, "", empty.Program, "an empty legacy program leaves the effective program unset")
	legacyEmpty, ok := candidateBySource(empty, RootAgentSourceLegacy)
	require.True(t, ok)
	assert.Contains(t, legacyEmpty.Reason, "unset",
		"the trace must record that an empty legacy program is unset, not set-to-empty")

	set := ResolveRootAgent(RootAgentInputs{Legacy: &RootAgentConfig{Program: "codex"}})
	assert.Equal(t, "codex", set.Program, "a non-empty legacy program is the effective program")
	legacySet, ok := candidateBySource(set, RootAgentSourceLegacy)
	require.True(t, ok)
	assert.NotContains(t, legacySet.Reason, "unset")
}

// TestRootAgentEnabledSerializesExplicitFalse pins P3-b: RootAgent.Enabled has
// no omitempty, so an explicit `enabled = false` survives a full serialization
// (RegisterRootAgent/saveConfigLocked marshal the whole Config). With omitempty
// the false would be dropped and a disabling override erased on the next write
// — the #1700 zero-value-elision class.
func TestRootAgentEnabledSerializesExplicitFalse(t *testing.T) {
	tomlBytes, err := toml.Marshal(RootAgent{Enabled: false})
	require.NoError(t, err)
	assert.Contains(t, string(tomlBytes), "enabled = false",
		"an explicit enabled=false must be written to TOML, not omitted")

	jsonBytes, err := json.Marshal(RootAgent{Enabled: false})
	require.NoError(t, err)
	assert.Contains(t, string(jsonBytes), "\"enabled\":false",
		"an explicit enabled=false must be written to JSON (the --explain surface), not omitted")
}

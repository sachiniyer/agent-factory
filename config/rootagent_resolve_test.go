package config

import (
	"encoding/json"
	"reflect"
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

// TestResolveRootAgentEmptyLegacyDoesNotClobberGlobalProgram pins P3-a with PR2's
// global layer present: an empty legacy entry enables the root but must NOT clobber
// a global program back to the default. This is the case that would have silently
// broken — af writes an empty legacy entry for every registered project.
func TestResolveRootAgentEmptyLegacyDoesNotClobberGlobalProgram(t *testing.T) {
	res := ResolveRootAgent(RootAgentInputs{
		Global: &RootAgentLayer{Value: RootAgent{Program: "codex"}}, // program set, enabled unset
		Legacy: &RootAgentConfig{},                                  // empty: enables, program unset
	})
	assert.True(t, res.Enabled, "the legacy entry enables the root")
	assert.Equal(t, "codex", res.Program, "an empty legacy program must not clobber the global program")
	assert.Equal(t, RootAgentSourceGlobal, res.ProgramSource)
	assert.Equal(t, RootAgentSourceLegacy, res.EnabledSource)

	alone := ResolveRootAgent(RootAgentInputs{Legacy: &RootAgentConfig{}})
	assert.Equal(t, "", alone.Program, "an empty legacy alone supplies no program")
	assert.Equal(t, RootAgentSource(""), alone.ProgramSource)
}

// TestResolveRootAgentPrecedence pins the approved order built-in < global <
// legacy < personal, merged by field.
func TestResolveRootAgentPrecedence(t *testing.T) {
	res := ResolveRootAgent(RootAgentInputs{
		Global:   &RootAgentLayer{Value: RootAgent{Enabled: true, Program: "global-prog"}, EnabledSet: true},
		Legacy:   &RootAgentConfig{Program: "legacy-prog"},
		Personal: &RootAgentLayer{Value: RootAgent{Enabled: true, Program: "personal-prog"}, EnabledSet: true},
	})
	assert.True(t, res.Enabled)
	assert.Equal(t, "personal-prog", res.Program, "personal outranks legacy and global")
	assert.Equal(t, RootAgentSourcePersonal, res.ProgramSource)
	assert.Equal(t, RootAgentSourcePersonal, res.EnabledSource)
}

// TestResolveRootAgentPersonalDisablesLegacy is THE case the confirmed precedence
// exists for: a personal enabled=false must disable a root a legacy entry turned
// on. Since af writes an empty legacy entry (enabled=true) for every registered
// project, legacy-above-personal would make this impossible — the silent no-op
// #2216 kills.
func TestResolveRootAgentPersonalDisablesLegacy(t *testing.T) {
	res := ResolveRootAgent(RootAgentInputs{
		Legacy:   &RootAgentConfig{Program: "legacy-prog"},                            // enables
		Personal: &RootAgentLayer{Value: RootAgent{Enabled: false}, EnabledSet: true}, // explicit disable
	})
	assert.False(t, res.Enabled, "a personal enabled=false disables a legacy-enabled root")
	assert.Equal(t, RootAgentSourcePersonal, res.EnabledSource, "personal supplies the effective enabled")

	// personal wins the enabled decision; the legacy program is still the
	// effective program (moot while disabled), so its trace result is winner-by-
	// program, not shadowed — what matters is that Enabled is false.
	personal, _ := candidateBySource(res, RootAgentSourcePersonal)
	assert.Equal(t, "winner", personal.Result)
}

// TestResolveRootAgentGlobalEnables: a global enabled=true (no legacy, no personal)
// enables the root — the global default fanning out to a registered project.
func TestResolveRootAgentGlobalEnables(t *testing.T) {
	res := ResolveRootAgent(RootAgentInputs{
		Global: &RootAgentLayer{Value: RootAgent{Enabled: true}, EnabledSet: true},
	})
	assert.True(t, res.Enabled)
	assert.Equal(t, RootAgentSourceGlobal, res.EnabledSource)
}

// TestRootAgentConfigIsFullyAdapted is the struct-field guard (P2-a carry-forward):
// it fails when RootAgentConfig gains or loses a field, forcing rootAgentFromLegacy
// (and RootAgent) to carry it rather than silently dropping it — the AutoYes/#2335
// class where a new per-repo field vanishes from every ensured root while the
// suite stays green.
func TestRootAgentConfigIsFullyAdapted(t *testing.T) {
	var names []string
	for _, f := range reflect.VisibleFields(reflect.TypeOf(RootAgentConfig{})) {
		names = append(names, f.Name)
	}
	assert.Equal(t, []string{"Program"}, names,
		"RootAgentConfig's fields changed — update rootAgentFromLegacy and RootAgent to carry the new field, then this guard")
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

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rootAgentCandidateByLayer(rv ResolvedValue, layer RootAgentSource) (CandidateTrace, bool) {
	for _, c := range rv.Candidates {
		if c.Layer == string(layer) {
			return c, true
		}
	}
	return CandidateTrace{}, false
}

// TestRootAgentExplainTraceNamesLayerPerField is the load-bearing --explain
// proof: the rendered trace must name the RIGHT layer PER FIELD for the decisive
// interactions, because Enabled and Program can be supplied by different layers
// (merge is by field on presence). It reuses the exact fixtures the resolver
// tests pin, but asserts on the ResolvedValue the CLI renders, so a two-layer or
// wrong-winner explanation of a four-layer decision cannot ship.
func TestRootAgentExplainTraceNamesLayerPerField(t *testing.T) {
	t.Run("empty legacy does not clobber the global program", func(t *testing.T) {
		res := ResolveRootAgent(RootAgentInputs{
			Global: &RootAgentLayer{Value: RootAgent{Program: "codex"}}, // program set, enabled unset
			Legacy: &RootAgentConfig{},                                  // empty: enables, program unset
		})
		rv := rootAgentResolvedValue(res, rootAgentLocations{})

		// Per-field winners: enabled from legacy, program from global.
		require.Contains(t, rv.Origins, "enabled")
		require.Contains(t, rv.Origins, "program")
		assert.Equal(t, string(RootAgentSourceLegacy), rv.Origins["enabled"].Layer,
			"enabled must be attributed to the legacy entry, not the global layer")
		assert.Equal(t, string(RootAgentSourceGlobal), rv.Origins["program"].Layer,
			"program must be attributed to the global layer the empty legacy entry did not clobber")

		// Effective value is enabled with the global program preserved.
		assert.Equal(t, RootAgent{Enabled: true, Program: "codex"}, rv.Value)

		// The empty-legacy row is legible: enabled=true, no program of its own.
		legacy, ok := rootAgentCandidateByLayer(rv, RootAgentSourceLegacy)
		require.True(t, ok)
		assert.Equal(t, map[string]any{"enabled": true}, legacy.Value,
			"a present-but-empty legacy entry must read as enabled=true with no program")

		// The global row shows its program and does NOT imply it set enabled.
		global, ok := rootAgentCandidateByLayer(rv, RootAgentSourceGlobal)
		require.True(t, ok)
		assert.Equal(t, map[string]any{"program": "codex"}, global.Value)
	})

	t.Run("personal disables a legacy-enabled root", func(t *testing.T) {
		res := ResolveRootAgent(RootAgentInputs{
			Legacy:   &RootAgentConfig{Program: "legacy-prog"},                            // enables, program set
			Personal: &RootAgentLayer{Value: RootAgent{Enabled: false}, EnabledSet: true}, // explicit disable
		})
		rv := rootAgentResolvedValue(res, rootAgentLocations{})

		// The disable must be attributed to the personal layer.
		require.Contains(t, rv.Origins, "enabled")
		assert.Equal(t, string(RootAgentSourcePersonal), rv.Origins["enabled"].Layer,
			"a personal enabled=false must be attributed to the personal layer, not the legacy entry it overrode")
		assert.Equal(t, RootAgent{Enabled: false, Program: "legacy-prog"}, rv.Value,
			"the root resolves disabled even though the legacy program is still the effective program")

		personal, ok := rootAgentCandidateByLayer(rv, RootAgentSourcePersonal)
		require.True(t, ok)
		assert.Equal(t, map[string]any{"enabled": false}, personal.Value,
			"the personal row must show its explicit enabled=false")
	})
}

// TestRootAgentExplainTraceCoversAllFourLayers pins that the trace always names
// every layer the daemon considers — built-in, global, legacy, personal — so an
// absent layer reads as "not participating" rather than vanishing from the
// explanation.
func TestRootAgentExplainTraceCoversAllFourLayers(t *testing.T) {
	rv := rootAgentResolvedValue(ResolveRootAgent(RootAgentInputs{}), rootAgentLocations{})
	for _, layer := range []RootAgentSource{
		RootAgentSourceBuiltIn, RootAgentSourceGlobal, RootAgentSourceLegacy, RootAgentSourcePersonal,
	} {
		_, ok := rootAgentCandidateByLayer(rv, layer)
		assert.Truef(t, ok, "the trace must name the %s layer even when absent", layer)
	}
	assert.Equal(t, MergeTableByField.String(), rv.Merge)
	assert.Equal(t, []string{"built-in", "global", "legacy root_agents", "personal project"}, rv.Precedence)
	// With nothing configured, enabled is the built-in base and no layer supplies
	// a program (it falls through to the default profile — no config origin).
	assert.Equal(t, string(RootAgentSourceBuiltIn), rv.Origins["enabled"].Layer)
	assert.NotContains(t, rv.Origins, "program")
}

package config

import (
	"os"
	"path/filepath"
	"reflect"
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
		rv := rootAgentResolvedValue(res, rootAgentLocations{}, true)

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
		rv := rootAgentResolvedValue(res, rootAgentLocations{}, true)

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
	rv := rootAgentResolvedValue(ResolveRootAgent(RootAgentInputs{}), rootAgentLocations{}, false)
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

// TestRootAgentInspectionInputsPopulateEveryLayer is the drift guard for the
// --explain input assembly. assembleRootAgentInspectionInputs DUPLICATES the
// daemon's RootAgentInputs assembly (rootAgentInputsFor) on purpose — the two
// must read different sources (on-disk config vs the daemon's start-of-day
// snapshot) — and only the downstream ResolveRootAgent is shared. The hazard of
// that split: a FIFTH layer added to RootAgentInputs for the daemon could go
// unassembled here and silently vanish from the trace, reintroducing the
// wrong-precedence explanation this whole change fixes. So, given a fixture that
// configures every layer, every RootAgentInputs field MUST be populated; a new
// unset field fails this loudly, forcing the assembler and this fixture to carry
// it. It mirrors TestRootAgentConfigIsFullyAdapted, which guards the legacy
// adapter the same way.
func TestRootAgentInspectionInputsPopulateEveryLayer(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	require.NoError(t, os.MkdirAll(home, 0o755))
	// A fixture exercising every non-built-in layer: a global [root_agent], a
	// legacy root_agents entry that resolves to the repo, and the project's
	// personal [root_agent]. built-in is synthesized inside ResolveRootAgent, not
	// a RootAgentInputs field, so it needs no fixture.
	globalTOML := "schema_version = 1\n\n[root_agent]\nenabled = true\n\n[root_agents]\n\"" + repoRoot + "\" = {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName), []byte(globalTOML), 0o644))
	writePersonalConfig(t, project.ID, "[root_agent]\nenabled = false\n")

	inputs, _, _, _, err := assembleRootAgentInspectionInputs(repoRoot, true)
	require.NoError(t, err)

	v := reflect.ValueOf(inputs)
	require.NotZero(t, v.NumField(), "RootAgentInputs must have layer fields")
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		field := v.Field(i)
		require.Equalf(t, reflect.Ptr, field.Kind(),
			"RootAgentInputs.%s is not a pointer layer; the drift guard assumes every input is a nilable layer source — update it deliberately for a new shape", name)
		require.Falsef(t, field.IsNil(),
			"RootAgentInputs.%s was left nil by assembleRootAgentInspectionInputs even though the fixture configures every layer — a new root-agent layer must be assembled for --explain too, or it silently drops out of the trace (the drift this guard prevents)", name)
	}
}

func TestRootAgentInspectionErrorUsesCanonicalProjectWording(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName), []byte("schema_version = 1\n"), 0o644))

	_, err := ResolveRootAgentForInspection(t.TempDir(), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to resolve project path")
	assert.NotContains(t, err.Error(), "--project")
}

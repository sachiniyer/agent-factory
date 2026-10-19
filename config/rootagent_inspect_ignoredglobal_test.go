package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the cause-preservation contract for the ignored-global
// rendering of `af config get root_agent --explain`: a present global
// [root_agent] the daemon drops because the inspected project is not
// registered and has no legacy root_agents entry
// (markRootAgentGlobalIneligible). The full-table trace carries the cause on
// the global row; the dotted read (`af config get root_agent.enabled
// --explain`) must NOT relabel that row as manifest-disallowed, since the
// root_agent manifest entry DOES admit the global layer. The pre-fix path
// routed the dotted read through the generic ResolvedValuePath projector,
// whose `!Allowed` branch clobbered registry ineligibility with manifest
// policy — the "confidently wrong trace" the surface exists to prevent.

const ignoredGlobalReason = "project is not registered and has no legacy root_agents entry"

// setupUnregisteredProjectWithGlobalRootAgent mirrors the fixture of
// TestConfigGetRootAgentGlobalSingletonRequiresRegisteredProject: a global
// [root_agent] enabled=true program="codex" plus a deregistered project that
// carries no legacy root_agents entry. Produces the ignored-global parent
// the dotted-read projector consumes.
func setupUnregisteredProjectWithGlobalRootAgent(t *testing.T) (home, repoRoot string) {
	t.Helper()
	home, repoRoot, project := registeredTestProject(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName),
		[]byte("schema_version = 1\n\n[root_agent]\nenabled = true\nprogram = \"codex\"\n"), 0o644))
	removed, err := DeregisterProject(repoRoot)
	require.NoError(t, err)
	require.True(t, removed, "fixture: project must deregister so the registry-eligibility check drops the global layer")
	require.NoError(t, os.MkdirAll(filepath.Dir(project.Root), 0o755))
	return home, repoRoot
}

// ignoredGlobalParent resolves the ignored-global root_agent parent. Passed
// strictProjectLookup=false so the lenient lookup the bare-read path uses
// is the one that exercises the ignored-global branch; a strict lookup of a
// deregistered project's path errors out for unrelated reasons.
func ignoredGlobalParent(t *testing.T, repoRoot string) ResolvedValue {
	t.Helper()
	rv, err := ResolveRootAgentForInspection(repoRoot, false)
	require.NoError(t, err, "the ignored-global verdict is known; --explain must surface it rather than error")
	require.False(t, RootAgentValueFailsClosed(rv),
		"fixture must NOT trip the fail-closed path — these shapes are disjoint and have disjoint projectors")
	require.True(t, RootAgentValueHasIgnoredGlobal(rv),
		"fixture must trip the ignored-global detection helper or the projector assertions are vacuous")
	return rv
}

// TestRootAgentValueHasIgnoredGlobalDetectsUnregisteredProject is the
// end-to-end detection-contract fixture: a deregistered project with a global
// [root_agent], no legacy root_agents, and a loadable (just-empty) registry
// produces the ignored-global parent.
func TestRootAgentValueHasIgnoredGlobalDetectsUnregisteredProject(t *testing.T) {
	_, repoRoot := setupUnregisteredProjectWithGlobalRootAgent(t)
	rv := ignoredGlobalParent(t, repoRoot)

	// The global row carries the registry cause on Result/Reason; the
	// recognizing marker is Allowed=false, the only path that sets it on a
	// root_agent candidate (the manifest admits the global layer for
	// root_agent, so a normal resolution never marks it disallowed).
	global, ok := rootAgentCandidateByLayer(rv, RootAgentSourceGlobal)
	require.True(t, ok, "the trace must name the global layer")
	assert.True(t, global.Present, "the global [root_agent] is present on disk")
	assert.False(t, global.Allowed, "the global row carries the registry cause via Allowed=false")
	assert.Equal(t, "ignored", global.Result)
	assert.Equal(t, ignoredGlobalReason, global.Reason)

	// Origins survive (the built-in base supplies `enabled`), distinguishing
	// the ignored-global shape from the fail-closed (zero-provenance) shape.
	require.Contains(t, rv.Origins, "enabled")
	assert.Equal(t, string(RootAgentSourceBuiltIn), rv.Origins["enabled"].Layer)
}

// TestRootAgentValueHasIgnoredGlobalRejectsFailClosedShape pins the disjoint-
// shapes contract: a fail-closed parent ALSO marks present Layers as Result
// "ignored", but RootAgentValueFailsClosed returns true for it (zero-provenance
// shape), so the ignored-global detector must NOT also fire — the two shapes
// have disjoint projectors, and double-firing would route the fail-closed
// parent through the wrong projector.
func TestRootAgentValueHasIgnoredGlobalRejectsFailClosedShape(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName),
		[]byte("schema_version = 1\n\n[root_agents]\n\""+repoRoot+"\" = {}\n"), 0o644))
	corruptInspectionRegistry(t)

	rv, err := ResolveRootAgentForInspection(repoRoot, false)
	require.NoError(t, err)
	require.True(t, RootAgentValueFailsClosed(rv), "fixture must produce the fail-closed shape")
	assert.False(t, RootAgentValueHasIgnoredGlobal(rv),
		"the fail-closed parent must route through ProjectFailClosedRootAgentLeaf, not the ignored-global projector")
}

// TestRootAgentValueHasIgnoredGlobalRejectsNormalShape confirms the detector
// does not fire for a normal registered-project shape where the global row is
// `winner` or `shadowed` (not `ignored`).
func TestRootAgentValueHasIgnoredGlobalRejectsNormalShape(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	// Global [root_agent] enabled=true program=claude AND a legacy entry
	// (Enabled=true by virtue of presence); the global row is shadowed by
	// the legacy entry's enabled.
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName),
		[]byte("schema_version = 1\n\n[root_agent]\nenabled = true\nprogram = \"claude\"\n\n[root_agents]\n\""+
			repoRoot+"\" = {}\n"), 0o644))

	rv, err := ResolveRootAgentForInspection(repoRoot, false)
	require.NoError(t, err)
	assert.False(t, RootAgentValueHasIgnoredGlobal(rv),
		"a registered project carries no ignored-global marker, so the detector must not fire")
}

// TestRootAgentValueHasIgnoredGlobalRejectsNonRootAgentKey pins the obvious
// guardrail: the detector only inspects root_agent traces.
func TestRootAgentValueHasIgnoredGlobalRejectsNonRootAgentKey(t *testing.T) {
	rv := ResolvedValue{Key: "default_program", Candidates: []CandidateTrace{{
		Layer: string(RootAgentSourceGlobal), Allowed: false, Present: true, Result: "ignored",
	}}}
	assert.False(t, RootAgentValueHasIgnoredGlobal(rv),
		"non-root_agent keys are not subject to the ignored-global projector")
}

// requireIgnoredGlobalEnabledProjection projects the ignored-global parent's
// `enabled` leaf and asserts the cause-preservation invariants.
func requireIgnoredGlobalEnabledProjection(t *testing.T, parent ResolvedValue) {
	t.Helper()
	projected, ok := ProjectIgnoredGlobalRootAgentLeaf(parent, "root_agent.enabled")
	require.True(t, ok, "root_agent.enabled must project; the global layer dropped out but the leaf still answers")

	assert.Equal(t, "root_agent.enabled", projected.Key)
	assert.Equal(t, false, projected.Value, "the effective enabled is the built-in disabled default — the global layer dropped")
	assert.Empty(t, projected.Default, "a dotted read clears the parent table's default text")

	// The parent table's Origins are dropped: a dotted read projects ONE
	// leaf, and the parent's per-leaf origins must NOT leak into a single
	// leaf projection. The built-in's surviving `enabled` origin routes
	// through Winner, not Origins.
	assert.Empty(t, projected.Origins, "the dotted read must drop the parent table's per-leaf Origins to avoid leaking into unrelated leaves")
	require.NotNil(t, projected.Winner, "the surviving `enabled` origin (built-in) must surface as the single-leaf Winner")
	assert.Equal(t, string(RootAgentSourceBuiltIn), projected.Winner.Layer)
	assert.Equal(t, "root_agent.enabled", projected.Winner.KeyPath)

	// Each candidate row keeps its Result/Reason verbatim, with the leaf
	// value substituted.
	require.Len(t, projected.Candidates, 4, "the projection names every layer the daemon resolves")
	for _, candidate := range projected.Candidates {
		assert.Equal(t, "root_agent.enabled", candidate.KeyPath, "every candidate's KeyPath is the dotted key")
		switch candidate.Layer {
		case string(RootAgentSourceBuiltIn):
			assert.True(t, candidate.Present)
			assert.Equal(t, false, candidate.Value)
			assert.Equal(t, "base", candidate.Result)
			assert.Equal(t, "root agents are opt-in by default", candidate.Reason)
		case string(RootAgentSourceGlobal):
			assert.True(t, candidate.Present)
			assert.Equal(t, true, candidate.Value, "the ignored global row carries its configured enabled=true leaf")
			assert.Equal(t, "ignored", candidate.Result, "the registry cause stays on Result")
			assert.Equal(t, ignoredGlobalReason, candidate.Reason, "the registry cause stays on Reason verbatim")
		case string(RootAgentSourceLegacy), string(RootAgentSourcePersonal):
			assert.False(t, candidate.Present, "absent rows stay absent")
			assert.Nil(t, candidate.Value)
			assert.Equal(t, "absent", candidate.Result)
			assert.Equal(t, "this project has no entry", candidate.Reason)
		default:
			t.Fatalf("unexpected layer %q", candidate.Layer)
		}
	}
}

// requireIgnoredGlobalProgramProjection projects the ignored-global parent's
// `program` leaf and asserts the no-fail contract: the leaf's origin did not
// survive the global-layer drop, but the dotted read still answers with the
// cause rather than reporting an unknown key.
func requireIgnoredGlobalProgramProjection(t *testing.T, parent ResolvedValue) {
	t.Helper()
	projected, ok := ProjectIgnoredGlobalRootAgentLeaf(parent, "root_agent.program")
	require.True(t, ok, "root_agent.program must project; the cause is what this rendering exists to surface even when the leaf's origin did not survive")

	assert.Equal(t, "root_agent.program", projected.Key)
	assert.Equal(t, "", projected.Value, "the effective program is empty — the global layer (its only contributor) was dropped")
	assert.Empty(t, projected.Default)
	assert.Empty(t, projected.Origins, "the parent's `enabled` origin must NOT leak into a `program` projection")
	assert.Nil(t, projected.Winner, "no origin survives the global-layer drop for `program`; the projector does not invent one")

	require.Len(t, projected.Candidates, 4)
	for _, candidate := range projected.Candidates {
		assert.Equal(t, "root_agent.program", candidate.KeyPath)
		switch candidate.Layer {
		case string(RootAgentSourceBuiltIn):
			assert.True(t, candidate.Present)
			assert.Nil(t, candidate.Value, "the built-in base contributes no program leaf")
			assert.Equal(t, "base", candidate.Result)
			assert.Equal(t, "root agents are opt-in by default", candidate.Reason)
		case string(RootAgentSourceGlobal):
			assert.True(t, candidate.Present)
			assert.Equal(t, "codex", candidate.Value, "the ignored global row shows the configured program that did not survive — legible, not erased")
			assert.Equal(t, "ignored", candidate.Result, "the registry cause survives on Result, NOT the false `disallowed`")
			assert.Equal(t, ignoredGlobalReason, candidate.Reason)
		case string(RootAgentSourceLegacy), string(RootAgentSourcePersonal):
			assert.False(t, candidate.Present)
			assert.Nil(t, candidate.Value)
		}
	}
}

// TestProjectIgnoredGlobalRootAgentLeafPreservesCauses is the load-bearing
// unit-level proof: the dotted `root_agent.enabled` projection of an
// ignored-global parent carries the registry cause on the global row,
// matching the full-table trace. Pre-fix, this row was relabeled
// `disallowed · manifest policy does not allow this key at this source`.
func TestProjectIgnoredGlobalRootAgentLeafPreservesCauses(t *testing.T) {
	_, repoRoot := setupUnregisteredProjectWithGlobalRootAgent(t)
	parent := ignoredGlobalParent(t, repoRoot)
	requireIgnoredGlobalEnabledProjection(t, parent)
}

// TestProjectIgnoredGlobalRootAgentLeafProjectsProgramWithoutOrigin pins the
// no-fail contract for the `program` leaf. The generic ResolvedValuePath path
// gates on `parent.Origins[leaf]` and reports an unknown key when the leaf's
// origin did not survive the global-layer drop; the cause-preserving
// projector does not gate on Origins, mirroring the fail-closed precedent
// (ProjectFailClosedRootAgentLeaf's `root_agent.program` already answers
// with the disabled profile's empty program).
func TestProjectIgnoredGlobalRootAgentLeafProjectsProgramWithoutOrigin(t *testing.T) {
	_, repoRoot := setupUnregisteredProjectWithGlobalRootAgent(t)
	parent := ignoredGlobalParent(t, repoRoot)
	requireIgnoredGlobalProgramProjection(t, parent)
}

// TestProjectIgnoredGlobalRootAgentLeafRejectsNonLeafPaths asserts the
// projector only accepts `root_agent.<leaf>` keyPaths.
func TestProjectIgnoredGlobalRootAgentLeafRejectsNonLeafPaths(t *testing.T) {
	_, repoRoot := setupUnregisteredProjectWithGlobalRootAgent(t)
	parent := ignoredGlobalParent(t, repoRoot)

	for _, keyPath := range []string{
		"root_agent",                // bare key, no leaf
		"root_agent.",               // empty leaf
		"default_program",           // unrelated key
		"program_overrides.codex",   // unrelated dotted key
		"root_agent.enabled.suffix", // nested leaf (not a real root_agent shape)
	} {
		_, ok := ProjectIgnoredGlobalRootAgentLeaf(parent, keyPath)
		assert.Falsef(t, ok, "ProjectIgnoredGlobalRootAgentLeaf must reject %q", keyPath)
	}
}

// TestProjectIgnoredGlobalRootAgentLeafUnknownLeaf rejects a leaf that is
// not part of the root_agent table (the projector relies on
// configLeafValue's reflection-over-the-struct lookup to reject unknown
// leaves, the same gate ResolvedValuePath uses for unknown leaves).
func TestProjectIgnoredGlobalRootAgentLeafUnknownLeaf(t *testing.T) {
	_, repoRoot := setupUnregisteredProjectWithGlobalRootAgent(t)
	parent := ignoredGlobalParent(t, repoRoot)

	_, ok := ProjectIgnoredGlobalRootAgentLeaf(parent, "root_agent.not_a_real_leaf")
	assert.False(t, ok, "an unknown leaf of the root_agent table must not project")
}

// TestProjectIgnoredGlobalRootAgentLeafAbsentOptionalLeafIsUnknown pins the
// contract the ignored-global projector shares with the generic
// ResolvedValuePath and the registered-project lookup: a leaf no layer
// actually configured (e.g. `program` when the global [root_agent] sets only
// `enabled`) reports `unknown config key`, not an empty effective value. The
// global-layer drop removed the program origin, and the ignored global
// candidate never carried a program leaf either, so there is no cause to
// surface for that leaf — presenting the zero value as effective would be a
// configured-value fabrication the registered-project and generic lookups do
// not make.
func TestProjectIgnoredGlobalRootAgentLeafAbsentOptionalLeafIsUnknown(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName),
		[]byte("schema_version = 1\n\n[root_agent]\nenabled = true\n"), 0o644))
	removed, err := DeregisterProject(repoRoot)
	require.NoError(t, err)
	require.True(t, removed, "fixture: project must deregister so the registry-eligibility check drops the global layer")

	rv, err := ResolveRootAgentForInspection(repoRoot, false)
	require.NoError(t, err)
	require.True(t, RootAgentValueHasIgnoredGlobal(rv),
		"fixture must trip the ignored-global detection helper or the test is vacuous")

	// The global table set only `enabled`; the program leaf has no surviving
	// origin and the ignored global candidate never carried it.
	_, ok := ProjectIgnoredGlobalRootAgentLeaf(rv, "root_agent.program")
	assert.False(t, ok, "an optional leaf no layer configured must report unknown, not an empty effective value")

	// `enabled` is the leaf the global table actually set, so it still
	// projects with the registry cause — the gate is per-leaf, not
	// whole-table.
	enabled, ok := ProjectIgnoredGlobalRootAgentLeaf(rv, "root_agent.enabled")
	require.True(t, ok, "root_agent.enabled must still project — the global table set it")
	assert.Equal(t, false, enabled.Value, "the effective enabled is the built-in disabled default — the global layer was dropped")
}

// TestProjectIgnoredGlobalRootAgentLeafMarksAbsentGlobalLeafAbsent pins the
// contract the ignored-global projector shares with the generic
// ResolvedValuePath for a leaf the ignored global table never configured: a
// global [root_agent] that set only `program`, queried for `root_agent.enabled`,
// reaches the projector (the built-in base supplies the `enabled` origin), but
// the global candidate never carried `enabled`. The candidate must be marked
// absent — Present=false, Result="absent" — so the trace does not present a
// never-configured field as an effective value the source ignored, while the
// built-in base that does carry `enabled` keeps its row verbatim.
func TestProjectIgnoredGlobalRootAgentLeafMarksAbsentGlobalLeafAbsent(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName),
		[]byte("schema_version = 1\n\n[root_agent]\nprogram = \"codex\"\n"), 0o644))
	removed, err := DeregisterProject(repoRoot)
	require.NoError(t, err)
	require.True(t, removed, "fixture: project must deregister so the registry-eligibility check drops the global layer")

	rv, err := ResolveRootAgentForInspection(repoRoot, false)
	require.NoError(t, err)
	require.True(t, RootAgentValueHasIgnoredGlobal(rv),
		"fixture must trip the ignored-global detection helper or the test is vacuous")

	// `enabled` still projects: the built-in base supplies its origin, so the
	// dotted read answers (it does not gate on Origins alone).
	projected, ok := ProjectIgnoredGlobalRootAgentLeaf(rv, "root_agent.enabled")
	require.True(t, ok, "root_agent.enabled must project — the built-in base supplies its origin")

	assert.Equal(t, "root_agent.enabled", projected.Key)
	assert.Equal(t, false, projected.Value, "the effective enabled is the built-in disabled default — the global layer was dropped")
	require.NotNil(t, projected.Winner, "the surviving built-in `enabled` origin surfaces as the single-leaf Winner")
	assert.Equal(t, string(RootAgentSourceBuiltIn), projected.Winner.Layer)

	for _, candidate := range projected.Candidates {
		assert.Equal(t, "root_agent.enabled", candidate.KeyPath)
		switch candidate.Layer {
		case string(RootAgentSourceBuiltIn):
			assert.True(t, candidate.Present, "the built-in base carries enabled")
			assert.Equal(t, false, candidate.Value)
			assert.Equal(t, "base", candidate.Result)
			assert.Equal(t, "root agents are opt-in by default", candidate.Reason)
		case string(RootAgentSourceGlobal):
			assert.False(t, candidate.Present, "the ignored global candidate never carried enabled; mark it absent")
			assert.Nil(t, candidate.Value)
			assert.Equal(t, "absent", candidate.Result, "a never-configured leaf is absent, not ignored")
			assert.Equal(t, "leaf is not present at this source", candidate.Reason)
		case string(RootAgentSourceLegacy), string(RootAgentSourcePersonal):
			assert.False(t, candidate.Present, "absent rows stay absent")
			assert.Nil(t, candidate.Value)
		}
	}
}

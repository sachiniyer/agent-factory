package commands

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the fix for the ignored-global dotted-read bug end to end
// through `af config get root_agent.<leaf> --explain` on an UNREGISTERED
// project that carries a global [root_agent] table but no legacy root_agents
// entry. The fixture mirrors TestConfigGetRootAgentGlobalSingletonRequiresRegisteredProject.
//
// Pre-fix: the generic ResolvedValuePath projector relabeled the present
// global candidate as "disallowed · manifest policy does not allow this key
// at this source" — a factually false claim, since the root_agent manifest
// entry DOES admit the global layer — clobbering the true registry cause
// ("project is not registered and has no legacy root_agents entry") that the
// full-table `root_agent --explain` trace carries. The `root_agent.program`
// dotted read additionally failed outright because no program origin survived
// the global-layer drop. After the fix, the ignored-global parent routes
// through ProjectIgnoredGlobalRootAgentLeaf, which copies each candidate row
// verbatim with only the leaf value substituted, mirroring the
// ProjectFailClosedRootAgentLeaf precedent for the fail-closed shape.

const ignoredGlobalRegistryReason = "project is not registered and has no legacy root_agents entry"

// setupUnregisteredRootAgentProject mirrors the fixture of
// TestConfigGetRootAgentGlobalSingletonRequiresRegisteredProject: a global
// [root_agent] enabled=true program="codex", and a deregistered project with
// no legacy root_agents entry. Sets the test's repo flag to the repo root and
// chdir's into it so the bare-read path inspects the deregistered checkout.
func setupUnregisteredRootAgentProject(t *testing.T) (home, repoRoot string) {
	t.Helper()
	_, repoRoot = setupRootAgentProject(t,
		"schema_version = 1\n\n[root_agent]\nenabled = true\nprogram = \"codex\"\n",
		false, "")
	removed, err := config.DeregisterProject(repoRoot)
	require.NoError(t, err)
	require.True(t, removed, "fixture: project must deregister so the registry-eligibility check drops the global layer")
	t.Chdir(repoRoot)
	setConfigGetReadFlags(t, "", false, false)
	return home, repoRoot
}

// TestConfigGetRootAgentExplainDottedPreservesRegistryCause is the load-bearing
// end-to-end proof for the bug: the dotted `root_agent.enabled --explain` read
// on an unregistered project must carry the registry cause that the
// full-table trace carries, not the factually false manifest-disallowed claim.
func TestConfigGetRootAgentExplainDottedPreservesRegistryCause(t *testing.T) {
	_, repoRoot := setupUnregisteredRootAgentProject(t)
	setConfigGetReadFlags(t, "", true, false)

	out, err := runConfigGetForTest(t, "root_agent.enabled")
	require.NoError(t, err)
	t.Logf("root_agent.enabled --explain (unregistered):\n%s", out)

	assert.Contains(t, out, ignoredGlobalRegistryReason,
		"the dotted read must carry the registry cause the full-table trace carries — this is the bug fix")
	assert.NotContains(t, out, "manifest policy does not allow this key at this source",
		"the dotted read must NOT assert manifest policy blocked a source the root_agent manifest admits — that is the false cause this surface exists to prevent")
	// The header still names the inspected repository (the cause is
	// project-specific), and the effective value reflects the daemon's
	// decision: the global layer dropped out, so the built-in disabled
	// default won.
	assert.Contains(t, out, "project: "+repoRoot)
	assert.Contains(t, out, "root_agent.enabled = false")
}

// TestConfigGetRootAgentDottedEnabledNonExplainReturnsBuiltIn pins the
// concise-read contract for the ignored-global shape: with --explain off,
// the dotted read prints only the effective value, and that value is the
// built-in disabled default (the global layer was dropped). No regression
// from the pre-fix concise read.
func TestConfigGetRootAgentDottedEnabledNonExplainReturnsBuiltIn(t *testing.T) {
	setupUnregisteredRootAgentProject(t)
	out, err := runConfigGetForTest(t, "root_agent.enabled")
	require.NoError(t, err)
	assert.Equal(t, "false\n", out,
		"the concise dotted read prints the effective enabled=false (built-in wins after the global-layer drop)")
}

// TestConfigGetRootAgentDottedProgramExplainSucceedsOnUnregistered is the
// no-fail contract for the `program` leaf. Pre-fix, the dotted
// `root_agent.program --explain` read failed with
// `unknown config key "root_agent.program"` because no program origin
// survived the global-layer drop. After the fix, the cause-preserving
// projector does not gate on Origins (mirroring ProjectFailClosedRootAgentLeaf),
// so the dotted read answers with the cause and an empty effective program.
func TestConfigGetRootAgentDottedProgramExplainSucceedsOnUnregistered(t *testing.T) {
	setupUnregisteredRootAgentProject(t)
	setConfigGetReadFlags(t, "", true, false)

	out, err := runConfigGetForTest(t, "root_agent.program")
	require.NoError(t, err, "root_agent.program is a known leaf of the ignored-global profile, not an unknown key")
	t.Logf("root_agent.program --explain (unregistered):\n%s", out)

	assert.Contains(t, out, ignoredGlobalRegistryReason,
		"the global row must carry the registry cause on the program leaf too — the bug is per-shape, not just enabled")
	assert.NotContains(t, out, "manifest policy does not allow this key at this source")
	assert.Contains(t, out, `root_agent.program = ""`,
		"the effective program renders as the empty string (the only program contributor was dropped)")
	// The global row's value still shows the configured program that did not
	// survive, so the legibility contract from the full-table trace is
	// preserved at the dotted leaf.
	assert.Contains(t, out, "codex")
}

// TestConfigGetRootAgentDottedProgramNonExplainReturnsEmpty is the concise-read
// twin of TestConfigGetRootAgentDottedProgramExplainSucceedsOnUnregistered:
// the program dotted read prints an empty line (the effective program is
// empty after the global-layer drop), mirroring the fail-closed precedent's
// `root_agent.program` concise read.
func TestConfigGetRootAgentDottedProgramNonExplainReturnsEmpty(t *testing.T) {
	setupUnregisteredRootAgentProject(t)
	out, err := runConfigGetForTest(t, "root_agent.program")
	require.NoError(t, err)
	assert.Equal(t, "\n", out,
		"the concise program read prints an empty line — the global layer (its only contributor) was dropped")
}

// TestConfigGetRootAgentExplainStillShowsRegistryCauseOnFullTable pins the
// no-regression side-by-side: the full-table `root_agent --explain` read on
// the SAME unregistered project continues to carry the registry cause, so
// the full-table and dotted reads now AGREE (they previously disagreed — the
// bug report's "the two cannot both be true, and the second is false").
func TestConfigGetRootAgentExplainStillShowsRegistryCauseOnFullTable(t *testing.T) {
	setupUnregisteredRootAgentProject(t)
	setConfigGetReadFlags(t, "", true, false)

	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	t.Logf("root_agent --explain (unregistered, full-table):\n%s", out)
	assert.Contains(t, out, ignoredGlobalRegistryReason,
		"the full-table trace must still carry the registry cause — the fix changes the dotted read, not the table")
	assert.Contains(t, out, `root_agent = {"enabled":false}`)
	assert.NotContains(t, out, "manifest policy does not allow this key at this source",
		"the full-table trace must not have inherited the false manifest-disallowed claim either")
}

// TestConfigGetRootAgentDottedExplainJSONPreservesCause is the machine-readable
// contract: the {data,error} envelope from the dotted `root_agent.enabled
// --explain` read must carry the registry cause on the global candidate and
// must NOT carry a manifest-disallowed reason on any candidate.
func TestConfigGetRootAgentDottedExplainJSONPreservesCause(t *testing.T) {
	setupUnregisteredRootAgentProject(t)
	setConfigGetReadFlags(t, "", true, true)

	output, err := runConfigGetForTest(t, "root_agent.enabled")
	require.NoError(t, err)
	var envelope struct {
		Data  configGetExplanation `json:"data"`
		Error any                  `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &envelope))
	assert.Nil(t, envelope.Error)
	assert.Equal(t, "project", envelope.Data.Context.Scope)
	assert.Equal(t, "root_agent.enabled", envelope.Data.Key)
	assert.Equal(t, false, envelope.Data.Value)
	require.NotNil(t, envelope.Data.Winner,
		"the surviving built-in origin routes through Winner for the dotted leaf")
	assert.Equal(t, config.SourceBuiltIn.String(), envelope.Data.Winner.Layer)
	assert.Empty(t, envelope.Data.Origins,
		"the dotted read projects one leaf, so the parent table's per-leaf Origins are dropped")

	var sawGlobal bool
	for _, candidate := range envelope.Data.Candidates {
		if candidate.Layer == string(config.RootAgentSourceGlobal) {
			sawGlobal = true
			assert.True(t, candidate.Present, "the global [root_agent] is present on disk")
			assert.Equal(t, "ignored", candidate.Result,
				"the registry cause stays on Result in the JSON envelope — no `disallowed` relabel")
			assert.Equal(t, ignoredGlobalRegistryReason, candidate.Reason)
			assert.Equal(t, true, candidate.Value,
				"the ignored global row carries its configured enabled=true leaf in the envelope")
		}
		assert.NotEqual(t, "disallowed", candidate.Result,
			"no candidate may be labeled manifest-disallowed — the root_agent manifest admits the global layer")
		for _, marker := range []string{"manifest policy does not allow"} {
			assert.NotContains(t, candidate.Reason, marker,
				"the JSON envelope must not surface a manifest-policy claim the full-table trace does not carry")
		}
	}
	require.True(t, sawGlobal, "the envelope must include the global candidate row")
}

// TestConfigGetRootAgentDottedRegisteredProjectStillUsesGenericProjection is
// the no-regression guard: a REGISTERED project (which carries no
// ignored-global marker) routes its dotted `root_agent.enabled` read through
// the generic ResolvedValuePath projector as before. The cause-preserving
// projector is reserved for the ignored-global shape and must not be
// triggered here.
func TestConfigGetRootAgentDottedRegisteredProjectStillUsesGenericProjection(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", true, "")
	t.Chdir(repoRoot)
	setConfigGetReadFlags(t, "", true, false)

	out, err := runConfigGetForTest(t, "root_agent.enabled")
	require.NoError(t, err)
	t.Logf("root_agent.enabled --explain (registered, legacy enabled):\n%s", out)

	// The legacy entry carries enabled=true. The generic projector names
	// the winning layer as `winner`, NOT the cause-preserving `base` the
	// ignored-global projector would have carried.
	assert.Contains(t, out, "winner · supplies the effective leaf",
		"a registered project with a winning legacy entry must still route through the generic projector")
	assert.NotContains(t, out, ignoredGlobalRegistryReason,
		"the registered project must not trip the ignored-global detector — its global row is not ignored")
}

package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetProjectConfigValueWholeProgramOverridesPreservesPersonalTombstone is
// the project-path counterpart of TestSetGlobalConfigValueWholeProgramOverrides-
// CanRemoveDetectedDefault. A project-scoped program_overrides.<agent> = ""
// tombstone that disables an inherited global/auto-detected override for that
// project must survive a subsequent whole-map program_overrides write that does
// NOT name the agent. Before the in-lock re-canonicalization was mirrored onto
// the project path, setTOMLStructured removed the entire [program_overrides]
// table and dropped the tombstone, so the resolved config fell back to the
// lower-layer override on the next resolve — silently undoing the deliberate
// per-project disablement.
func TestSetProjectConfigValueWholeProgramOverridesPreservesPersonalTombstone(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "default_program = 'claude'\n[program_overrides]\nclaude = '/global/claude --flag'\n")
	writePersonalConfig(t, project.ID, "[program_overrides]\nclaude = \"\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	require.Equal(t, "claude", ResolveProgram(&resolved.Config, "claude"),
		"setup invariant: the personal tombstone disables the global override")

	res, err := SetProjectConfigValue(project.ID, "program_overrides", `{"codex":"codex --model gpt-5"}`)
	require.NoError(t, err)
	assert.Equal(t, `{"codex":"codex --model gpt-5"}`, res.Value,
		"the read-back value hides the preserved claude=\"\" tombstone and reports only the map the user submitted")

	resolved, err = ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "claude", ResolveProgram(&resolved.Config, "claude"),
		"the personal claude=\"\" tombstone must survive a whole-map write that omits claude")
	assert.Equal(t, "codex --model gpt-5", resolved.ProgramOverrides["codex"],
		"the user-supplied override is still written")

	path, _ := ProjectConfigTomlPath(project.ID)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `claude = ''`, "the tombstone is retained on disk")
	assert.Contains(t, content, `codex = 'codex --model gpt-5'`, "the requested override is on disk")
}

// TestSetProjectConfigValueWholeProgramOverridesReplacedTombstoneWhenNamed
// ensures preservation is not dogmatic: naming the tombstoned agent with a real
// value replaces the tombstone, letting the user explicitly re-enable an
// override for this project.
func TestSetProjectConfigValueWholeProgramOverridesReplacedTombstoneWhenNamed(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "default_program = 'claude'\n[program_overrides]\nclaude = '/global/claude --flag'\n")
	writePersonalConfig(t, project.ID, "[program_overrides]\nclaude = \"\"\n")

	_, err := SetProjectConfigValue(project.ID, "program_overrides", `{"claude":"/personal/claude --safe"}`)
	require.NoError(t, err)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "/personal/claude --safe", ResolveProgram(&resolved.Config, "claude"),
		"a named override replaces the tombstone")
}

// TestSetProjectConfigValueWholeProgramOverridesNoSpuriousTombstoneOnFreshFile
// pins the deliberate divergence from the global path: the project canonicalizer
// preserves only the "" tombstones already present on THIS layer and does NOT
// reseed from DefaultConfig().ProgramOverrides (a global-only concern that keeps
// an auto-detected GLOBAL default masked after removal). So a whole-map write
// against a personal file with no pre-existing tombstones invents none — a fresh
// project never gains a "" entry for an inherited override it never disabled.
func TestSetProjectConfigValueWholeProgramOverridesNoSpuriousTombstoneOnFreshFile(t *testing.T) {
	home, _, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "[program_overrides]\nclaude = '/global/claude'\ncodex = '/global/codex'\n")
	writePersonalConfig(t, project.ID, "[program_overrides]\ncodex = '/personal/codex'\n")

	_, err := SetProjectConfigValue(project.ID, "program_overrides", `{"gemini":"/personal/gemini"}`)
	require.NoError(t, err)

	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	_, hasClaude := cfg.ProgramOverrides["claude"]
	assert.False(t, hasClaude, "no spurious claude=\"\" tombstone invented for an inherited override this layer never disabled")
	_, hasCodex := cfg.ProgramOverrides["codex"]
	assert.False(t, hasCodex, "codex was simply not named in the new map, so it is absent — not promoted to a tombstone")
	assert.Equal(t, "/personal/gemini", cfg.ProgramOverrides["gemini"])
}

// TestSetProjectConfigValueDynamicLeafStillWorksAfterWholeMapProject is the
// project-path counterpart of TestSetGlobalConfigValueDynamicLeafStillWorksAfter-
// WholeMap: after a whole-map whole-table write, the dynamic leaf writer
// (program_overrides.<agent>) must still update a single agent in place without
// disturbing the preserved tombstone.
func TestSetProjectConfigValueDynamicLeafStillWorksAfterWholeMapProject(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "[program_overrides]\nclaude = '/global/claude'\n")
	writePersonalConfig(t, project.ID, "[program_overrides]\nclaude = \"\"\n")

	_, err := SetProjectConfigValue(project.ID, "program_overrides", `{"codex":"codex --model gpt-5"}`)
	require.NoError(t, err)

	_, err = SetProjectConfigValue(project.ID, "program_overrides.codex", "codex --model o1")
	require.NoError(t, err)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "codex --model o1", ResolveProgram(&resolved.Config, "codex"),
		"the dynamic leaf writer updates codex after the whole-map write")
	assert.Equal(t, "claude", ResolveProgram(&resolved.Config, "claude"),
		"the preserved claude tombstone survives the leaf write too")
}

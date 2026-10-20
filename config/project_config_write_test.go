package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetProjectConfigValueCreatesAndResolves(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "default_program = \"codex\"\n")

	res, err := SetProjectConfigValue(project.ID, "default_program", "gemini")
	require.NoError(t, err)
	assert.Equal(t, "default_program", res.Key)
	assert.Equal(t, "gemini", res.Value)
	assert.True(t, res.RequiresRestart)
	path, _ := ProjectConfigTomlPath(project.ID)
	assert.Equal(t, path, res.Path)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "gemini", resolved.DefaultProgram)
}

func TestSetProjectConfigValueByPathSelector(t *testing.T) {
	_, repoRoot, _ := registeredTestProject(t)
	res, err := SetProjectConfigValue(repoRoot, "default_program", "codex")
	require.NoError(t, err, "a repository path resolves to its registered project")
	assert.Equal(t, "codex", res.Value)
}

func TestSetProjectConfigValuePreservesComments(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "# my project prefs\ndefault_program = \"codex\" # keep me\nbranch_prefix = \"feat/\"\n")

	_, err := SetProjectConfigValue(project.ID, "default_program", "gemini")
	require.NoError(t, err)

	path, _ := ProjectConfigTomlPath(project.ID)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, "# my project prefs", "leading comments are preserved")
	assert.Contains(t, content, "# keep me", "the target line's trailing comment is preserved")
	assert.Contains(t, content, "default_program = 'gemini'")
	assert.Contains(t, content, "branch_prefix = \"feat/\"", "an untouched key keeps its exact bytes")
}

func TestSetProjectConfigValueDynamicOverride(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	_, err := SetProjectConfigValue(project.ID, "program_overrides.claude", "/opt/claude --x")
	require.NoError(t, err)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "/opt/claude --x", resolved.ProgramOverrides["claude"])
}

func TestSetProjectConfigValueRejectsGlobalOnlyKey(t *testing.T) {
	_, _, project := registeredTestProject(t)
	_, err := SetProjectConfigValue(project.ID, "listen_addr", "0.0.0.0:8443")
	require.Error(t, err)
	require.Contains(t, err.Error(), "global setting")
	require.Contains(t, err.Error(), "af config set network.listen_addr", "the error names how to set it correctly")
}

func TestSetProjectConfigValueRejectsGlobalOnlyStructuredKey(t *testing.T) {
	_, _, project := registeredTestProject(t)
	_, err := SetProjectConfigValue(project.ID, "root_agents", "x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "global setting")
	require.Contains(t, err.Error(), "af config set root_agents")
}

func TestSetProjectConfigValueValidatesValue(t *testing.T) {
	_, _, project := registeredTestProject(t)
	_, err := SetProjectConfigValue(project.ID, "default_program", "not-an-agent")
	require.Error(t, err, "the same enum validator the loader uses gates the write")
}

func TestSetProjectConfigValueUnregisteredSelector(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	loose := initProjectRegistryRepo(t, filepath.Join(base, "loose"))
	_, err := SetProjectConfigValue(loose, "default_program", "codex")
	require.Error(t, err)
	require.Contains(t, err.Error(), "af projects add", "an unregistered target never silently writes")
}

// TestSetProjectConfigValueEqualToGlobalIsStillOverride pins the distinction
// unset exists for: a personal value equal to the lower layer is a present,
// winning override, not the same as clearing it. (branch_prefix is the one
// personal key this does not hold for — its stored value never wins, #4539.)
func TestSetProjectConfigValueEqualToGlobalIsStillOverride(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "on_archive_command = \"same\"\n")

	_, err := SetProjectConfigValue(project.ID, "on_archive_command", "same")
	require.NoError(t, err)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	value, ok := resolved.ResolvedValue("on_archive_command")
	require.True(t, ok)
	require.NotNil(t, value.Winner)
	assert.Equal(t, SourceProjectPersonal.String(), value.Winner.Layer)
}

func TestSetProjectConfigValueConcurrent(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writes := []struct{ key, value string }{
		{"default_program", "codex"},
		{"branch_prefix", "feat/"},
		{"program_overrides.claude", "/a/claude"},
		{"program_overrides.codex", "/a/codex"},
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(writes))
	for _, w := range writes {
		wg.Add(1)
		go func(key, value string) {
			defer wg.Done()
			if _, err := SetProjectConfigValue(project.ID, key, value); err != nil {
				errs <- err
			}
		}(w.key, w.value)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err, "concurrent locked writes must leave a loadable file")
	require.NotNil(t, cfg)
	assert.Equal(t, "codex", cfg.DefaultProgram)
	assert.Equal(t, "feat/", cfg.BranchPrefix)
	assert.Equal(t, "/a/claude", cfg.ProgramOverrides["claude"])
	assert.Equal(t, "/a/codex", cfg.ProgramOverrides["codex"])
}

func TestUnsetProjectConfigRemovesOnlyTarget(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "# hdr\ndefault_program = \"codex\"\nbranch_prefix = \"feat/\"\n")

	res, err := UnsetProjectConfigValue(project.ID, "default_program")
	require.NoError(t, err)
	require.True(t, res.Removed)

	path, _ := ProjectConfigTomlPath(project.ID)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)
	assert.NotContains(t, content, "default_program", "only the target key is removed")
	assert.Contains(t, content, "branch_prefix = \"feat/\"", "sibling keys survive")
	assert.Contains(t, content, "# hdr", "comments survive")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	value, ok := resolved.ResolvedValue("default_program")
	require.True(t, ok)
	require.NotNil(t, value.Winner)
	assert.NotEqual(t, SourceProjectPersonal.String(), value.Winner.Layer, "the cleared key falls back to a lower layer")
}

func TestUnsetProjectConfigRemovesEmptiedFile(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "default_program = \"codex\"\n")

	res, err := UnsetProjectConfigValue(project.ID, "default_program")
	require.NoError(t, err)
	require.True(t, res.Removed)

	path, _ := ProjectConfigTomlPath(project.ID)
	require.NoFileExists(t, path, "an emptied personal file is removed so the project falls fully back")
}

func TestUnsetProjectConfigDynamicLeaf(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[program_overrides]\nclaude = \"/a/claude\"\ncodex = \"/a/codex\"\n")

	res, err := UnsetProjectConfigValue(project.ID, "program_overrides.claude")
	require.NoError(t, err)
	require.True(t, res.Removed)

	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	_, hasClaude := cfg.ProgramOverrides["claude"]
	assert.False(t, hasClaude, "only the named leaf is cleared")
	assert.Equal(t, "/a/codex", cfg.ProgramOverrides["codex"])
}

func TestUnsetProjectConfigStructuredTable(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "branch_prefix = \"feat/\"\n\n[program_overrides]\nclaude = \"/a/claude\"\n")

	res, err := UnsetProjectConfigValue(project.ID, "program_overrides")
	require.NoError(t, err)
	require.True(t, res.Removed, "the whole structured override must be removed")

	path, _ := ProjectConfigTomlPath(project.ID)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "program_overrides")
	assert.Contains(t, string(data), "branch_prefix = \"feat/\"", "unrelated personal keys survive")
}

// TestUnsetProjectConfigRootAgentCaseVariantLeafFoldsSpelling is the regression
// test for the #5397 review finding that UnsetProjectConfigValue(...,
// "root_agent.enabled") delegated to the case-sensitive deleteTOMLScalar, so a
// personal project file containing [root_agent] Enabled = true (a case variant
// the decoder folds onto RootAgent.Enabled) survived the unset: the exact
// "enabled" line match missed "Enabled" and the command reported Removed: false,
// leaving the root agent enabled instead of restoring inheritance. The unset
// path now locates the physical case-variant spelling the operator wrote, the
// same way resolution locates it, so the case-variant leaf is removed.
func TestUnsetProjectConfigRootAgentCaseVariantLeafFoldsSpelling(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[root_agent]\nEnabled = true\nprogram = \"custom\"\n")

	// Before the fix, deleteTOMLScalar matched the canonical "enabled" exactly
	// and missed the case-variant "Enabled", so the unset was a no-op.
	res, err := UnsetProjectConfigValue(project.ID, "root_agent.enabled")
	require.NoError(t, err)
	require.True(t, res.Removed, "the case-variant Enabled leaf must be removed, not missed by an exact-key match")

	path, _ := ProjectConfigTomlPath(project.ID)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "Enabled", "the case-variant enabled line must be gone")
	assert.Contains(t, string(data), `program = "custom"`, "the sibling program field survives")

	// The personal root_agent no longer sets enabled, so it falls back to the
	// lower layer (the built-in default, which is disabled).
	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	layer := cfg.RootAgentLayer()
	require.NotNil(t, layer)
	assert.False(t, layer.EnabledSet, "unsetting the case-variant enabled leaf must clear EnabledSet")
	// ResolveRootAgent with no global layer falls back to the built-in default (disabled).
	resolved := ResolveRootAgent(RootAgentInputs{Personal: layer})
	assert.False(t, resolved.Enabled, "the unset must restore inheritance: the root agent is no longer enabled")
}

// TestUnsetProjectConfigDynamicLeafStaysCaseSensitive pins the design boundary
// the #5397 unset fix draws: a dynamic map key (program_overrides.<name>) is a
// case-sensitive map entry, not a struct field the decoder folds, so unsetting
// a canonical-spelled leaf must not remove a differently-cased map entry.
func TestUnsetProjectConfigDynamicLeafStaysCaseSensitive(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[program_overrides]\nClaude = \"/a/claude\"\n")

	// The on-disk key is the case-variant "Claude"; unsetting the canonical
	// "claude" must NOT fold case and remove it, because program_overrides is a
	// map whose keys are case-sensitive (distinct entries), not a struct field.
	res, err := UnsetProjectConfigValue(project.ID, "program_overrides.claude")
	require.NoError(t, err)
	require.False(t, res.Removed, "a dynamic map key is case-sensitive: unsetting claude must not remove Claude")

	path, _ := ProjectConfigTomlPath(project.ID)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `Claude = "/a/claude"`, "the case-variant Claude map entry survives (map keys are case-sensitive)")
}

func TestSetProjectConfigValueRootAgentPreservesOmittedEnabled(t *testing.T) {
	_, _, project := registeredTestProject(t)

	res, err := SetProjectConfigValue(project.ID, "root_agent", `{"program":"codex"}`)
	require.NoError(t, err)
	assert.Equal(t, `{"program":"codex"}`, res.Value)

	path, _ := ProjectConfigTomlPath(project.ID)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "enabled", "an omitted field must keep inheriting from lower layers")

	cfg, err := LoadProjectConfig(project.ID)
	require.NoError(t, err)
	layer := cfg.RootAgentLayer()
	require.NotNil(t, layer)
	assert.False(t, layer.EnabledSet)
	assert.Equal(t, "codex", layer.Value.Program)
	resolved := ResolveRootAgent(RootAgentInputs{
		Global:   &RootAgentLayer{Value: RootAgent{Enabled: true}, EnabledSet: true},
		Personal: layer,
	})
	assert.True(t, resolved.RootAgent.Enabled, "the omitted personal field must inherit global enabled=true")
}

func TestSetProjectConfigValueRootAgentReportsMergedProfile(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[root_agent]\nenabled = false\nprogram = \"custom\"\n")

	res, err := SetProjectConfigValue(project.ID, "root_agent", `{"enabled":true}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"enabled":true,"program":"custom"}`, res.Value)

	res, err = SetProjectConfigValue(project.ID, "root_agent", `{"enabled":false,"program":"next"}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"enabled":false,"program":"next"}`, res.Value)
}

func TestSetProjectConfigValueRootAgentReadbackFoldsCaseVariantEnabled(t *testing.T) {
	_, _, project := registeredTestProject(t)
	// A personal project file already spells the leaf with a case variant
	// (Enabled = true). taggedFieldByKey's case fold makes the daemon treat the
	// setting as active, but projectStructuredCurrentValue's exact
	// shape["enabled"] lookup missed it and the whole-table set readback returned
	// only the supplied program, dropping the active Enabled member from the
	// merged profile the JSON caller receives. The readback now folds the
	// enabled/program presence lookup the same way CurrentValue does, so the
	// case-variant Enabled renders in the merged value (#5397 review).
	writePersonalConfig(t, project.ID, "[root_agent]\nEnabled = true\nprogram = \"custom\"\n")

	res, err := SetProjectConfigValue(project.ID, "root_agent", `{"program":"codex"}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"enabled":true,"program":"codex"}`, res.Value,
		"a case-variant Enabled on disk must render in the merged readback, not be dropped by an exact shape lookup")
}

func TestSetProjectConfigValueRootAgentReadbackOmitsUnknownMembers(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[root_agent]\nenabled = false\nprogram = \"custom\"\nfuture_policy = \"keep\"\n")

	res, err := SetProjectConfigValue(project.ID, "root_agent", `{"enabled":true}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"enabled":true,"program":"custom"}`, res.Value)
	assert.NotContains(t, res.Value, "future_policy")

	_, err = SetProjectConfigValue(project.ID, "root_agent", res.Value)
	require.NoError(t, err, "the reported value must be accepted by config set")
	path, err := ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `future_policy = 'keep'`)
}

func TestSetProjectConfigValueRootAgentReadbackFailureDoesNotWrite(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "[root_agent]\nenabled = false\nfuture_policy = nan\n")
	path, err := ProjectConfigTomlPath(project.ID)
	require.NoError(t, err)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	_, err = SetProjectConfigValue(project.ID, "root_agent", `{"enabled":true}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no changes written")
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after)
}

func TestUnsetProjectConfigAbsentKeyIsNoOp(t *testing.T) {
	_, _, project := registeredTestProject(t)
	writePersonalConfig(t, project.ID, "branch_prefix = \"feat/\"\n")

	res, err := UnsetProjectConfigValue(project.ID, "default_program")
	require.NoError(t, err)
	require.False(t, res.Removed, "clearing a key that was never set is a clean no-op")
	require.False(t, res.RequiresRestart)
}

func TestUnsetProjectConfigNoFileIsNoOp(t *testing.T) {
	_, _, project := registeredTestProject(t)
	res, err := UnsetProjectConfigValue(project.ID, "default_program")
	require.NoError(t, err)
	require.False(t, res.Removed)
}

func TestUnsetProjectConfigRejectsGlobalOnlyKey(t *testing.T) {
	_, _, project := registeredTestProject(t)
	_, err := UnsetProjectConfigValue(project.ID, "listen_addr")
	require.Error(t, err, "the same scope gate applies to unset")
	require.Contains(t, err.Error(), "global setting")
}

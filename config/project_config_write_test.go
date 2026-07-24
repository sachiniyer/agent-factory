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
	require.Contains(t, err.Error(), "af config set listen_addr", "the error names how to set it correctly")
}

func TestSetProjectConfigValueRejectsStructuralKey(t *testing.T) {
	_, _, project := registeredTestProject(t)
	_, err := SetProjectConfigValue(project.ID, "root_agents", "x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a settable config key")
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
	require.Contains(t, err.Error(), "af projects register", "an unregistered target never silently writes")
}

// TestSetProjectConfigValueEqualToGlobalIsStillOverride pins the distinction
// unset exists for: a personal value equal to the lower layer is a present,
// winning override, not the same as clearing it.
func TestSetProjectConfigValueEqualToGlobalIsStillOverride(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "branch_prefix = \"same/\"\n")

	_, err := SetProjectConfigValue(project.ID, "branch_prefix", "same/")
	require.NoError(t, err)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	value, ok := resolved.ResolvedValue("branch_prefix")
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

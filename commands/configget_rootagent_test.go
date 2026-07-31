package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive `af config get root_agent --explain` end to end (#2216
// Phase 6): the explanation must show the FOUR layers the daemon resolves
// (built-in/global/legacy/personal), not the generic global<personal view, so a
// user debugging a root agent sees the precedence model that actually decided it.

// TestConfigGetRootAgentExplainGlobalNamesFourLayers: the global-scope trace
// names every layer the daemon considers and attributes each effective field to
// its source, even though global-only has no project to key legacy/personal by.
func TestConfigGetRootAgentExplainGlobalNamesFourLayers(t *testing.T) {
	_, _ = setupConfigExplainCommandTest(t, "schema_version = 1\n\n[root_agent]\nenabled = true\nprogram = \"codex\"\n")
	t.Chdir(t.TempDir())
	setConfigGetReadFlags(t, "", true, false)

	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	t.Logf("root_agent --explain (global):\n%s", out)

	for _, layer := range []string{"built-in", "global", "legacy root_agents", "personal project"} {
		assert.Containsf(t, out, layer, "the trace must name the %q layer", layer)
	}
	assert.Contains(t, out, `{"enabled":true,"program":"codex"}`, "the effective value must render")
	assert.Contains(t, out, "enabled: global", "enabled must be attributed to the global layer")
	assert.Contains(t, out, "program: global", "program must be attributed to the global layer")
}

// setupRootAgentProject git-inits and registers a repo under a fresh AF home,
// writes the global config.toml (baseGlobalTOML, plus a [root_agents] entry keyed
// by the real repo path when addLegacyEntry is set), and — when personalTOML !=
// "" — the project's personal config. It returns the home and repo root.
func setupRootAgentProject(t *testing.T, baseGlobalTOML string, addLegacyEntry bool, personalTOML string) (home, repoRoot string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")

	repoRoot = t.TempDir()
	for _, args := range [][]string{
		{"-C", repoRoot, "init", "-q"},
		{"-C", repoRoot, "config", "user.email", "test@example.com"},
		{"-C", repoRoot, "config", "user.name", "Test User"},
		{"-C", repoRoot, "commit", "--allow-empty", "-m", "init"},
	} {
		require.NoError(t, exec.Command("git", args...).Run())
	}

	globalTOML := baseGlobalTOML
	if addLegacyEntry {
		// The legacy root_agents map is keyed by repo path; the real repo root is
		// known only now, so it is appended after the repo exists.
		globalTOML += "\n[root_agents]\n\"" + repoRoot + "\" = {}\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName), []byte(globalTOML), 0644))

	project, err := config.RegisterProject(repoRoot)
	require.NoError(t, err)
	if personalTOML != "" {
		path, err := config.ProjectConfigTomlPath(project.ID)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(personalTOML), 0644))
	}
	return home, repoRoot
}

// TestConfigGetRootAgentExplainProjectPersonalDisablesLegacy is the decisive
// end-to-end case: a registered project carries a legacy root_agents entry (the
// ubiquitous empty entry = enabled) AND a personal enabled=false. The trace must
// make the interaction legible — the empty legacy row reading enabled=true, the
// personal layer winning the enabled decision — and resolve the root disabled.
// The generic two-layer explanation cannot show the legacy layer at all.
func TestConfigGetRootAgentExplainProjectPersonalDisablesLegacy(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", true, "[root_agent]\nenabled = false\n")

	setConfigGetReadFlags(t, repoRoot, true, false)
	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	t.Logf("root_agent --explain (--project, personal disables legacy):\n%s", out)

	// The legacy layer the generic trace omits must appear, and the empty entry is
	// legible as enabled=true.
	assert.Contains(t, out, "legacy root_agents", "the legacy layer must appear")
	assert.Contains(t, out, `{"enabled":true}`, "the present-but-empty legacy entry must read as enabled=true")
	// The personal disable wins the enabled decision and the root resolves off.
	assert.Contains(t, out, "enabled: personal project", "enabled must be attributed to the personal layer")
	assert.Contains(t, out, `root_agent = {"enabled":false}`, "the root must resolve disabled")
}

// TestConfigGetRootAgentDefaultsToCurrentProject is the fabricated-negative
// regression guard from #2607. A bare read from inside a repo with a legacy
// root_agents opt-in must inspect that project, for both the concise value and
// the provenance trace; reporting the global disabled default could make a
// healthy running root agent look orphaned.
func TestConfigGetRootAgentDefaultsToCurrentProject(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", true, "")
	t.Chdir(repoRoot)
	currentRepo, err := config.CurrentRepo()
	require.NoError(t, err)

	setConfigGetReadFlags(t, "", false, false)
	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	assert.Equal(t, "{\"enabled\":true}\n", out)

	configGetExplainFlag = true
	out, err = runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	assert.Contains(t, out, "project: "+currentRepo.Root)
	assert.Contains(t, out, `root_agent = {"enabled":true}`)
	assert.Contains(t, out, "winner · supplies the effective enabled")
	assert.Contains(t, out, "this project has no entry")
	assert.NotContains(t, out, "no project in scope")
}

// TestConfigGetRootAgentGlobalTraceDoesNotInventAProject distinguishes a real
// selected project with no entry from global fallback outside git. The latter
// must never claim that "this project" is unconfigured because no project was
// inspected.
func TestConfigGetRootAgentGlobalTraceDoesNotInventAProject(t *testing.T) {
	_, _ = setupConfigExplainCommandTest(t, "schema_version = 1\n")
	t.Chdir(t.TempDir())
	setConfigGetReadFlags(t, "", true, false)

	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	assert.Contains(t, out, "scope: global defaults")
	assert.Contains(t, out, "no project in scope")
	assert.NotContains(t, out, "not configured for this project")
}

// TestConfigGetRootAgentGlobalSingletonRequiresRegisteredProject keeps the
// inspection surface aligned with the daemon's candidate set. A global
// singleton applies to registered projects; merely running the read command
// inside an unrelated git checkout must not claim that a root will materialize.
func TestConfigGetRootAgentGlobalSingletonRequiresRegisteredProject(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t,
		"schema_version = 1\n\n[root_agent]\nenabled = true\nprogram = \"codex\"\n",
		false, "")
	removed, err := config.DeregisterProject(repoRoot)
	require.NoError(t, err)
	require.True(t, removed)
	t.Chdir(repoRoot)

	setConfigGetReadFlags(t, "", false, false)
	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	assert.Equal(t, "{\"enabled\":false}\n", out)

	configGetExplainFlag = true
	out, err = runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	assert.Contains(t, out, "project is not registered and has no legacy root_agents entry")
	assert.Contains(t, out, `root_agent = {"enabled":false}`)
}

// TestConfigGetRootAgentImplicitScopeToleratesCorruptRegistry preserves the
// daemon resolver's additive fallback for a cwd-derived project. An unreadable
// registry must not hide a legacy root_agents opt-in; an explicit selector
// remains strict because the user asked to inspect that project.
func TestConfigGetRootAgentImplicitScopeToleratesCorruptRegistry(t *testing.T) {
	home, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", true, "")
	badRecordDir := filepath.Join(home, config.ProjectRegistryDirName, "corrupt")
	require.NoError(t, os.MkdirAll(badRecordDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(badRecordDir, "project.json"), []byte("{"), 0o644))
	t.Chdir(repoRoot)

	setConfigGetReadFlags(t, "", false, false)
	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	assert.Equal(t, "{\"enabled\":true}\n", out)

	configGetProjectFlag = repoRoot
	_, err = runConfigGetForTest(t, "root_agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project registry")
}

func TestConfigReadFlagsUseRepoAndKeepProjectAlias(t *testing.T) {
	for _, cmd := range []*cobra.Command{configGetCmd, configListCmd} {
		require.NotNil(t, cmd.Flags().Lookup("repo"), "%s must follow the shared --repo contract", cmd.Name())
		require.NotNil(t, cmd.Flags().Lookup("project"), "%s must retain --project as a compatibility alias", cmd.Name())
	}
}

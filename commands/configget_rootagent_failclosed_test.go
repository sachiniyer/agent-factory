package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive `af config get root_agent` END TO END through the real
// command in the fail-closed states (#3241/#3247): the unit-level rendering
// alone proved nothing about the CLI, whose generic layered load errors out on
// exactly these states before the specialized root_agent resolution runs
// (found by the #3305 review). The command — not just the resolver — must
// answer with the daemon's fail-closed verdict and its cause.

// breakRegisteredPersonalConfig overwrites the registered project's personal
// config with undecodable TOML, after registration succeeded with a valid one.
func breakRegisteredPersonalConfig(t *testing.T, repoRoot string) string {
	t.Helper()
	repo, err := config.RepoFromPath(repoRoot)
	require.NoError(t, err)
	resolved := config.ResolveProjectPath(repo.Root)
	require.NotEmpty(t, resolved.Root, "fixture: the project must resolve while breaking its config")
	projects, err := config.ListProjects()
	require.NoError(t, err)
	require.Len(t, projects, 1, "fixture: exactly one registered project expected")
	path, err := config.ProjectConfigTomlPath(projects[0].ID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("[root_agent]\nenabled = tru\n"), 0o644))
	return path
}

// corruptCommandRegistry makes ListProjects fail portably and probe-proves it.
func corruptCommandRegistry(t *testing.T) {
	t.Helper()
	dir, err := config.ProjectRegistryDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stray"), []byte("not a project record"), 0o644))
	_, err = config.ListProjects()
	require.Error(t, err, "fixture failed: ListProjects still succeeds on a corrupt registry")
}

// TestConfigGetRootAgentExplainCommandFailsClosedOnUnloadablePersonal: the
// full command renders the fail-closed verdict — disabled, cause naming the
// file — instead of dying on the generic layered load's error.
func TestConfigGetRootAgentExplainCommandFailsClosedOnUnloadablePersonal(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", true, "[root_agent]\nenabled = false\n")
	personalPath := breakRegisteredPersonalConfig(t, repoRoot)
	t.Chdir(repoRoot)
	setConfigGetReadFlags(t, "", true, false)

	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err, "the fail-closed verdict is known; the command must explain it rather than surface the load error")
	t.Logf("root_agent --explain (personal unreadable):\n%s", out)
	assert.Contains(t, out, "cannot be loaded", "the explanation must carry the fail-closed cause")
	assert.Contains(t, out, personalPath, "the explanation must name the file to fix")
	assert.Contains(t, out, `"enabled":false`, "the effective profile must render disabled")
	assert.NotContains(t, out, "scope: global defaults",
		"the degraded context must not claim global scope over project-specific fail-closed candidates")
	assert.Contains(t, out, "project:", "the header must keep naming the inspected repository")
}

// TestConfigGetRootAgentLeafCommandFailsClosed: the dotted read projects the
// known fail-closed leaf value instead of reporting an unknown key (the
// Origins-keyed generic projection cannot serve the zero-provenance shape).
func TestConfigGetRootAgentLeafCommandFailsClosed(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", true, "[root_agent]\nenabled = false\n")
	breakRegisteredPersonalConfig(t, repoRoot)
	t.Chdir(repoRoot)
	setConfigGetReadFlags(t, "", false, false)

	out, err := runConfigGetForTest(t, "root_agent.enabled")
	require.NoError(t, err, "a fail-closed root_agent.enabled is the known value false, not an unknown key")
	assert.Contains(t, out, "false")
}

// TestConfigGetRootAgentProgramLeafCommandFailsClosed: root_agent.program is
// a valid leaf even while fail-closed with no global program configured. The
// generic resolution carries no origin for an unset program, so gating on the
// generic lookup reported the leaf as an unknown key before the fail-closed
// projector could answer (#3305 review) — the specialized path owns
// unknown-key detection for root_agent keys.
func TestConfigGetRootAgentProgramLeafCommandFailsClosed(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", true, "[root_agent]\nenabled = false\n")
	breakRegisteredPersonalConfig(t, repoRoot)
	t.Chdir(repoRoot)
	setConfigGetReadFlags(t, "", false, false)

	out, err := runConfigGetForTest(t, "root_agent.program")
	require.NoError(t, err, "root_agent.program is a known leaf of the fail-closed profile, not an unknown key")
	assert.Equal(t, "\n", out, "the fail-closed profile has no program; the empty value renders as a blank line")
}

// TestConfigGetRootAgentExplainCommandFailsClosedOnUnlistableRegistry: same
// round-trip for the registry arm, legacy entry present — the pre-#3264
// rendering reported the root enabled via the legacy layer.
func TestConfigGetRootAgentExplainCommandFailsClosedOnUnlistableRegistry(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", true, "")
	corruptCommandRegistry(t)
	t.Chdir(repoRoot)
	setConfigGetReadFlags(t, "", true, false)

	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err, "the fail-closed verdict is known; the command must explain it rather than surface the registry error")
	t.Logf("root_agent --explain (registry unlistable):\n%s", out)
	assert.Contains(t, out, config.ProjectRegistryDirName, "the explanation must name the registry")
	assert.Contains(t, out, `"enabled":false`, "the effective profile must render disabled")
	assert.NotContains(t, out, "enabled: legacy root_agents", "no layer may claim to have decided a fail-closed verdict")
}

// TestConfigGetRootAgentExplainRepresentsUnreadablePersonalLayer pins the
// trace shape: the personal row is shown PRESENT and ignored with the cause —
// its file exists, it is merely undecodable — never "this project has no
// entry", which would deny the very source the cause names.
func TestConfigGetRootAgentExplainRepresentsUnreadablePersonalLayer(t *testing.T) {
	_, repoRoot := setupRootAgentProject(t, "schema_version = 1\n", true, "[root_agent]\nenabled = false\n")
	breakRegisteredPersonalConfig(t, repoRoot)
	t.Chdir(repoRoot)
	setConfigGetReadFlags(t, "", true, false)

	out, err := runConfigGetForTest(t, "root_agent")
	require.NoError(t, err)
	assert.NotContains(t, out, "this project has no entry", "the unreadable personal layer must not be rendered as absent")
}

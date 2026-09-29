package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ManifestWithRepoValues — the project-scoped counterpart of
// ManifestWithValues that the config editors' project scope (config.read-project)
// renders, and that must read the same layers `af config list --repo` resolves:
// built-in < global < in-repo < personal project.

// manifestEntryByKey finds one row in an entry list.
func manifestEntryByKey(t *testing.T, entries []ConfigEntry, key string) ConfigEntry {
	t.Helper()
	for _, e := range entries {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("no manifest entry for %q", key)
	return ConfigEntry{}
}

func TestManifestWithRepoValuesLayersTheProjectStack(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)

	home, err := GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName),
		[]byte("default_program = \"claude\"\n"), 0o600))

	inRepo := filepath.Join(repoRoot, InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(inRepo, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(inRepo, TomlConfigFileName),
		[]byte("default_program = \"codex\"\n"), 0o644))

	writePersonalConfig(t, project.ID, "branch_prefix = \"proj/\"\n")

	repo, err := RepoFromPath(repoRoot)
	require.NoError(t, err)
	entries, err := ManifestWithRepoValues(repo)
	require.NoError(t, err)

	// In-repo beats global; the personal layer lands on top of both.
	assert.Equal(t, "codex", manifestEntryByKey(t, entries, "default_program").Value)
	assert.Equal(t, "proj/", manifestEntryByKey(t, entries, "branch_prefix").Value)

	// The repo-only keys a global file cannot hold must be listed: AllManifest,
	// not Manifest. remote_hooks is repo-only, so its mere presence proves the
	// wider manifest drove the read.
	manifestEntryByKey(t, entries, "remote_hooks")

	// And the GLOBAL read of the same keys must still answer the global file —
	// the two scopes disagree by construction, which is the gap this closes.
	global, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "claude", manifestEntryByKey(t, ManifestWithValues(global), "default_program").Value)
}

// root_agent is the special case: it resolves through the four-layer inspection
// (built-in/global/legacy root_agents/personal project), not the generic
// manifest pass — the same substitution `af config list --repo` makes.
func TestManifestWithRepoValuesRootAgentUsesThePersonalLayer(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)

	writePersonalConfig(t, project.ID, "[root_agent]\nprogram = \"codex\"\n")

	repo, err := RepoFromPath(repoRoot)
	require.NoError(t, err)
	entries, err := ManifestWithRepoValues(repo)
	require.NoError(t, err)

	// root_agent renders as compact JSON ({"enabled":...,"program":...}) — assert
	// on the program the personal layer set, not the encoding's byte shape.
	got := manifestEntryByKey(t, entries, "root_agent").Value
	assert.True(t, strings.Contains(got, "codex"),
		"root_agent must reflect the personal project layer, got %q", got)
}

// A project scope the caller named explicitly must not degrade: an unreadable
// personal layer is an error, never silently "absent" — the strict-lookup
// contract `af config list --repo` applies (#3264).
func TestManifestWithRepoValuesFailsOnAnUnreadableProjectLayer(t *testing.T) {
	_, repoRoot, project := registeredTestProject(t)

	writePersonalConfig(t, project.ID, "default_program = [unterminated\n")

	repo, err := RepoFromPath(repoRoot)
	require.NoError(t, err)
	_, err = ManifestWithRepoValues(repo)
	require.Error(t, err, "a malformed personal layer must fail the read, not render as though unset")
}

// The selector side: a path that is not a repository is refused at RepoFromPath,
// so a scope pick can never resolve to a global read wearing a project label.
func TestRepoFromPathRejectsANonRepoForTheScopeRead(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_, err := RepoFromPath(t.TempDir())
	require.Error(t, err)
}

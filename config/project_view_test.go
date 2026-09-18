package config

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectEntriesCoversAllManifestKeys pins the view's denominator: one row
// per AllManifest key, in manifest order — the same set `af config list --repo`
// prints, so a UI rendering these rows cannot silently drop a key the CLI shows.
func TestProjectEntriesCoversAllManifestKeys(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoRoot := t.TempDir()
	require.NoError(t, exec.Command("git", "init", repoRoot).Run())
	repo, err := RepoFromPath(repoRoot)
	require.NoError(t, err)
	resolved, err := ResolveConfigForRepoInspection(repo)
	require.NoError(t, err)
	rootAgent, err := ResolveRootAgentForInspection(repoRoot, true)
	require.NoError(t, err)

	entries := ProjectEntries(resolved, rootAgent)
	require.Len(t, entries, len(AllManifest()))
	for i, entry := range entries {
		assert.Equal(t, AllManifest()[i].Key, entry.Key)
	}
}

// TestProjectEntriesSwapsInTheSpecializedRootAgent: the generic resolution's
// root_agent row must be replaced by the four-layer inspection answer, the
// same swap `af config list` makes (rootAgentAwareResolution), or a UI would
// report a root_agent the daemon does not resolve.
func TestProjectEntriesSwapsInTheSpecializedRootAgent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	resolved, err := ResolveGlobalConfig()
	require.NoError(t, err)
	specialized, err := ResolveRootAgentForInspection("", false)
	require.NoError(t, err)

	entries := ProjectEntries(resolved, specialized)
	var seen *ConfigEntry
	for i := range entries {
		if entries[i].Key == "root_agent" {
			seen = &entries[i]
		}
	}
	require.NotNil(t, seen)
	assert.Equal(t, FormatConfigValue(specialized.Value), seen.Value)
}

// TestProjectDisplayValue keeps the "(unset)" vs explicit-empty distinction:
// an empty value nothing configured displays as "" so the pane's "(unset)"
// convention labels it, while an explicitly-empty configured composite stays
// visible rather than reading as unset.
func TestProjectDisplayValue(t *testing.T) {
	repo := SourceRef{Layer: SourceRepoShared.String()}
	cases := []struct {
		name  string
		value ResolvedValue
		want  string
	}{
		{"nil unconfigured", ResolvedValue{Value: nil}, ""},
		{"empty string unconfigured", ResolvedValue{Value: ""}, ""},
		{"empty map unconfigured", ResolvedValue{Value: map[string]string{}}, ""},
		{
			"empty map configured",
			ResolvedValue{Value: map[string]string{}, Winner: &repo},
			"{}",
		},
		{"string", ResolvedValue{Value: "docker", Winner: &repo}, "docker"},
		{"bool", ResolvedValue{Value: true}, "true"},
		{"map", ResolvedValue{Value: map[string]string{"claude": "x"}}, `{"claude":"x"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, ProjectDisplayValue(c.value))
		})
	}
}

// TestResolveProjectConfigView exercises the whole selector-to-rows chain the
// daemon route and the TUI's local read share: resolve a repo path, resolve
// its layers, and project every AllManifest key — including the repo-scoped
// keys the global view does not carry.
func TestResolveProjectConfigView(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoRoot := t.TempDir()
	require.NoError(t, exec.Command("git", "init", repoRoot).Run())
	writeInRepoConfig(t, repoRoot, `{"backend": "docker"}`)

	entries, projectRoot, err := ResolveProjectConfigView(repoRoot)
	require.NoError(t, err)
	require.Len(t, entries, len(AllManifest()))
	assert.Equal(t, repoRoot, projectRoot)

	byKey := make(map[string]ConfigEntry, len(entries))
	for _, entry := range entries {
		byKey[entry.Key] = entry
	}
	// A repo-scoped key answers the in-repo value — the row the global editor
	// never shows.
	assert.Equal(t, "docker", byKey["backend"].Value)
	// A global key the repo does not override still shows its effective value.
	assert.Equal(t, "true", byKey["auto_update"].Value)
	// An unset composite displays as "" so the pane's "(unset)" labels it
	// rather than rendering "null" as a configured value.
	assert.Equal(t, "", byKey["docker"].Value)
	assert.Equal(t, "", byKey["remote_hooks"].Value)
}

// TestResolveProjectConfigViewRejectsANonRepo: the selector is the same
// contract as `af config get --repo` — a path that resolves no repository is
// an error, not a global-config fallback the caller did not ask for.
func TestResolveProjectConfigViewRejectsANonRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_, _, err := ResolveProjectConfigView(filepath.Join(t.TempDir(), "not-a-repo"))
	assert.Error(t, err)
}

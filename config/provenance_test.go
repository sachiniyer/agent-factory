package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupProvenanceTest(t *testing.T, globalTOML string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName), []byte(globalTOML), 0644))
	return t.TempDir()
}

func requireResolvedValue(t *testing.T, cfg *ResolvedConfig, key string) ResolvedValue {
	t.Helper()
	value, ok := cfg.ResolvedValue(key)
	require.True(t, ok, "resolution missing key %q", key)
	return value
}

func candidateForLayer(t *testing.T, value ResolvedValue, layer ConfigSource) CandidateTrace {
	t.Helper()
	for _, candidate := range value.Candidates {
		if candidate.Layer == layer.String() {
			return candidate
		}
	}
	t.Fatalf("%s resolution has no %s candidate", value.Key, layer)
	return CandidateTrace{}
}

func TestResolveGlobalConfigTracksPresenceInsteadOfComparingValues(t *testing.T) {
	setupProvenanceTest(t, `
schema_version = 1
default_program = "claude"
auto_update = false
listen_addr = ""
cors_allowed_origins = []
limit_patterns = {}
`)

	resolved, err := ResolveGlobalConfig()
	require.NoError(t, err)

	tests := []struct {
		key   string
		value any
	}{
		{key: "default_program", value: "claude"}, // equal to the built-in, but explicit
		{key: "auto_update", value: false},
		{key: "listen_addr", value: ""},
		{key: "cors_allowed_origins", value: []string{}},
	}
	for _, test := range tests {
		t.Run(test.key, func(t *testing.T) {
			value := requireResolvedValue(t, resolved, test.key)
			require.NotNil(t, value.Winner)
			assert.Equal(t, SourceGlobal.String(), value.Winner.Layer)
			assert.Equal(t, test.value, value.Value)
			global := candidateForLayer(t, value, SourceGlobal)
			assert.True(t, global.Present)
			assert.Equal(t, "winner", global.Result)
		})
	}

	// An absent key genuinely falls through, even though the typed Config has
	// a default-filled value by the time resolution starts.
	branch := requireResolvedValue(t, resolved, "branch_prefix")
	require.NotNil(t, branch.Winner)
	assert.Equal(t, SourceBuiltIn.String(), branch.Winner.Layer)
	globalBranch := candidateForLayer(t, branch, SourceGlobal)
	assert.False(t, globalBranch.Present)
	assert.Equal(t, "absent", globalBranch.Result)

	patterns := requireResolvedValue(t, resolved, "limit_patterns")
	assert.NotNil(t, patterns.Value, "an explicitly empty map must stay distinct from an absent nil map")
	assert.Len(t, patterns.Value, 0)
	globalPatterns := candidateForLayer(t, patterns, SourceGlobal)
	assert.True(t, globalPatterns.Present)
	assert.Equal(t, "empty", globalPatterns.Result)
}

func TestResolveConfigReportsPerLeafMapOrigins(t *testing.T) {
	repoRoot := setupProvenanceTest(t, `
schema_version = 1
default_program = "claude"

[program_overrides]
codex = "/global/codex"
aider = "/global/aider"
`)
	writeInRepoTomlConfig(t, repoRoot, `
[program_overrides]
codex = "/repo/codex"
gemini = "/repo/gemini"
`)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "/repo/codex", resolved.ProgramOverrides["codex"])
	assert.Equal(t, "/global/aider", resolved.ProgramOverrides["aider"])
	assert.Equal(t, "/repo/gemini", resolved.ProgramOverrides["gemini"])

	value := requireResolvedValue(t, resolved, "program_overrides")
	assert.Nil(t, value.Winner, "a merged map must not claim one source won the whole value")
	assert.Equal(t, SourceRepoShared.String(), value.Origins["codex"].Layer)
	assert.Equal(t, SourceGlobal.String(), value.Origins["aider"].Layer)
	assert.Equal(t, SourceRepoShared.String(), value.Origins["gemini"].Layer)
	assert.Equal(t, "partially-shadowed", candidateForLayer(t, value, SourceGlobal).Result)
	assert.Equal(t, "contributed", candidateForLayer(t, value, SourceRepoShared).Result)

	leaf, ok := resolved.ResolvedValuePath("program_overrides.codex")
	require.True(t, ok)
	assert.Equal(t, "/repo/codex", leaf.Value)
	require.NotNil(t, leaf.Winner)
	assert.Equal(t, SourceRepoShared.String(), leaf.Winner.Layer)
	assert.Equal(t, "shadowed", candidateForLayer(t, leaf, SourceGlobal).Result)
	assert.Equal(t, "winner", candidateForLayer(t, leaf, SourceRepoShared).Result)
	_, ok = resolved.ResolvedValuePath("program_overrides.opencode")
	assert.False(t, ok, "a missing map leaf must not masquerade as an empty configured value")
}

func TestResolveGlobalConfigPreservesLegacyRootAgentsAndReportsEachPath(t *testing.T) {
	setupProvenanceTest(t, `
schema_version = 1

[root_agents."/work/one"]
program = "codex"

[root_agents."/work/two"]
program = "claude"
auto_yes = false
`)

	resolved, err := ResolveGlobalConfig()
	require.NoError(t, err)
	require.Len(t, resolved.RootAgents, 2)
	assert.Equal(t, "codex", resolved.RootAgents["/work/one"].Program)
	assert.False(t, resolved.RootAgents["/work/two"].AutoYesEnabled())

	value := requireResolvedValue(t, resolved, "root_agents")
	for _, path := range []string{"/work/one", "/work/two"} {
		origin, present := value.Origins[path]
		require.True(t, present)
		assert.Equal(t, SourceGlobal.String(), origin.Layer)
		assert.Equal(t, "root_agents[\""+path+"\"]", origin.KeyPath)
	}
}

func TestResolveConfigExplainsLegacyAndExplicitEmptyRepoValue(t *testing.T) {
	repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
	require.NoError(t, SaveRepoConfig(RepoIDFromRoot(repoRoot), &RepoConfig{
		PostWorktreeCommands: []string{"legacy-command"},
	}))
	writeInRepoTomlConfig(t, repoRoot, "post_worktree_commands = []\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Empty(t, resolved.PostWorktreeCommands)

	value := requireResolvedValue(t, resolved, "post_worktree_commands")
	require.NotNil(t, value.Winner)
	assert.Equal(t, SourceRepoShared.String(), value.Winner.Layer)
	assert.Equal(t, "shadowed", candidateForLayer(t, value, SourceLegacyRepo).Result)
	repo := candidateForLayer(t, value, SourceRepoShared)
	assert.True(t, repo.Present)
	assert.Equal(t, []any{}, repo.Value)
	assert.Equal(t, "winner", repo.Result)
}

func TestResolveConfigShowsDisallowedExistingLayers(t *testing.T) {
	repoRoot := setupProvenanceTest(t, "schema_version = 1\nauto_update = false\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)

	globalOnly := requireResolvedValue(t, resolved, "auto_update")
	repoCandidate := candidateForLayer(t, globalOnly, SourceRepoShared)
	assert.False(t, repoCandidate.Allowed)
	assert.Equal(t, "disallowed", repoCandidate.Result)
	assert.Contains(t, repoCandidate.Reason, "manifest policy")
	assert.Equal(t, InRepoTomlConfigPath(repoRoot), repoCandidate.Path)

	repoOnly := requireResolvedValue(t, resolved, "backend")
	globalCandidate := candidateForLayer(t, repoOnly, SourceGlobal)
	assert.False(t, globalCandidate.Allowed)
	assert.Equal(t, "disallowed", globalCandidate.Result)
}

func TestResolveConfigReportsTableOriginsAndNormalizedEffectiveValue(t *testing.T) {
	setupProvenanceTest(t, `
schema_version = 1

[theme]
accent = "#8cd0d3"
`)

	resolved, err := ResolveGlobalConfig()
	require.NoError(t, err)
	value := requireResolvedValue(t, resolved, "theme")
	require.IsType(t, ThemeConfig{}, value.Value)
	theme := value.Value.(ThemeConfig)
	assert.Equal(t, "#8CD0D3", theme.Accent)
	assert.Equal(t, SourceGlobal.String(), value.Origins["accent"].Layer)
	assert.Equal(t, SourceBuiltIn.String(), value.Origins["foreground"].Layer)
	assert.Equal(t, "partially-shadowed", candidateForLayer(t, value, SourceBuiltIn).Result)
	assert.Contains(t, candidateForLayer(t, value, SourceGlobal).Reason, "load-time normalization changed 1 configured entry")

	accent, ok := resolved.ResolvedValuePath("theme.accent")
	require.True(t, ok)
	assert.Equal(t, "#8CD0D3", accent.Value)
	require.NotNil(t, accent.Winner)
	assert.Equal(t, SourceGlobal.String(), accent.Winner.Layer)
	assert.Contains(t, candidateForLayer(t, accent, SourceGlobal).Reason, "load-time normalization changed")
}

func TestResolveGlobalConfigExplainsNormalizedKeyBindingLeaves(t *testing.T) {
	setupProvenanceTest(t, "schema_version = 1\n[keys]\nquit = \"Q\"\n")

	resolved, err := ResolveGlobalConfig()
	require.NoError(t, err)
	value := requireResolvedValue(t, resolved, "keys")
	bindings, ok := value.Value.(map[string][]string)
	require.True(t, ok)
	assert.Equal(t, []string{"Q"}, bindings["quit"])
	assert.Contains(t, candidateForLayer(t, value, SourceGlobal).Reason, "effective key bindings normalize")

	leaf, ok := resolved.ResolvedValuePath("keys.quit")
	require.True(t, ok)
	assert.Equal(t, []string{"Q"}, leaf.Value)
	assert.Contains(t, candidateForLayer(t, leaf, SourceGlobal).Reason, "effective key bindings normalize")
}

func TestResolveConfigTableOriginsPreserveExplicitZeroAndEmptyFields(t *testing.T) {
	repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
	writeInRepoTomlConfig(t, repoRoot, `
[ssh]
host = "example.test"
user = ""
port = 0
`)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	require.NotNil(t, resolved.SSH)
	assert.Equal(t, "example.test", resolved.SSH.Host)
	assert.Equal(t, "", resolved.SSH.User)
	assert.Equal(t, 0, resolved.SSH.Port)

	value := requireResolvedValue(t, resolved, "ssh")
	assert.Nil(t, value.Winner)
	for _, leaf := range []string{"host", "user", "port"} {
		origin, present := value.Origins[leaf]
		require.True(t, present, "missing origin for explicit ssh.%s", leaf)
		assert.Equal(t, SourceRepoShared.String(), origin.Layer)
	}
	repo := candidateForLayer(t, value, SourceRepoShared)
	configured := repo.Value.(map[string]any)
	assert.Equal(t, "", configured["user"])
	assert.EqualValues(t, 0, configured["port"])
}

func TestResolveConfigExplicitEmptyTableStaysMaterialized(t *testing.T) {
	repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
	writeInRepoTomlConfig(t, repoRoot, "[docker]\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	require.NotNil(t, resolved.Docker, "an explicitly present empty table must not collapse into absence")
	value := requireResolvedValue(t, resolved, "docker")
	assert.NotNil(t, value.Value)
	assert.Empty(t, value.Origins)
	repo := candidateForLayer(t, value, SourceRepoShared)
	assert.True(t, repo.Present)
	assert.Equal(t, "empty", repo.Result)
}

func TestResolveConfigProvenanceUsesPostResolutionHookPaths(t *testing.T) {
	repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
	writeInRepoTomlConfig(t, repoRoot, `
[remote_hooks]
launch_cmd = "./hooks/launch.sh"
delete_cmd = "hook-delete"
`)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	value := requireResolvedValue(t, resolved, "remote_hooks")
	hooks, ok := value.Value.(*RemoteHooks)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(repoRoot, "hooks/launch.sh"), hooks.LaunchCmd)

	repo := candidateForLayer(t, value, SourceRepoShared)
	configured := repo.Value.(map[string]any)
	assert.Equal(t, "./hooks/launch.sh", configured["launch_cmd"])
	assert.Equal(t, "winner", repo.Result)
	assert.NotContains(t, repo.Reason, "load-time normalization changed")
	assert.Contains(t, repo.Reason, "relative command paths resolved against the project root")
}

func TestResolveConfigResolutionCoversManifestAndDoesNotAliasSources(t *testing.T) {
	repoRoot := setupProvenanceTest(t, `
schema_version = 1
default_program = "claude"

[program_overrides]
codex = "/global/codex"
`)
	writeInRepoTomlConfig(t, repoRoot, "[program_overrides]\ncodex = \"/repo/codex\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	wantKeys := make([]string, 0, len(AllManifest()))
	for _, entry := range AllManifest() {
		wantKeys = append(wantKeys, entry.Key)
	}
	gotKeys := make([]string, 0, len(resolved.Resolution))
	for _, value := range resolved.Resolution {
		gotKeys = append(gotKeys, value.Key)
	}
	assert.Equal(t, wantKeys, gotKeys)

	resolved.ProgramOverrides["codex"] = "mutated"
	global, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "/global/codex", global.ProgramOverrides["codex"])
	assert.False(t, reflect.DeepEqual(resolved.ProgramOverrides, global.ProgramOverrides))
}

func TestResolveManifestDoesNotTakePrecedenceFromDocumentOrder(t *testing.T) {
	setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"codex\"\n")
	global, err := LoadConfig()
	require.NoError(t, err)
	documents, err := globalResolutionDocuments(global)
	require.NoError(t, err)
	documents[0], documents[1] = documents[1], documents[0]

	computed, err := resolveManifest(Manifest(), documents, false)
	require.NoError(t, err)
	for _, value := range computed {
		if value.resolved.Key != "default_program" {
			continue
		}
		require.NotNil(t, value.resolved.Winner)
		assert.Equal(t, SourceGlobal.String(), value.resolved.Winner.Layer)
		return
	}
	t.Fatal("default_program missing from manifest resolution")
}

func TestLoadInRepoConfigRejectsPresentEmptyDefaultProgram(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoTomlConfig(t, repoRoot, "default_program = \"\"\n")

	_, _, err := LoadInRepoConfig(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default_program")
	assert.Contains(t, err.Error(), "must be one of")
}

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeGlobalTOML writes the global config.toml under the active AF home.
func writeGlobalTOML(t *testing.T, home, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName), []byte(content), 0o644))
}

func TestResolveConfigPersonalBeatsInRepoBeatsGlobal(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "default_program = \"codex\"\n")
	writeInRepoTomlConfig(t, repoRoot, "default_program = \"aider\"\n")
	writePersonalConfig(t, project.ID, "default_program = \"gemini\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "gemini", resolved.DefaultProgram, "the personal layer sits above the checked-in in-repo value")

	value, ok := resolved.ResolvedValue("default_program")
	require.True(t, ok)
	require.NotNil(t, value.Winner)
	assert.Equal(t, SourceProjectPersonal.String(), value.Winner.Layer)
}

func TestResolveConfigInRepoWinsWhenNoPersonal(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	writeGlobalTOML(t, home, "default_program = \"codex\"\n")
	writeInRepoTomlConfig(t, repoRoot, "default_program = \"aider\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "aider", resolved.DefaultProgram, "with no personal override, existing precedence is unchanged")
}

func TestResolveConfigPersonalBranchPrefixBeatsGlobal(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "branch_prefix = \"global/\"\n")
	writePersonalConfig(t, project.ID, "branch_prefix = \"local/\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "local/", resolved.BranchPrefix)

	value, ok := resolved.ResolvedValue("branch_prefix")
	require.True(t, ok)
	require.NotNil(t, value.Winner)
	assert.Equal(t, SourceProjectPersonal.String(), value.Winner.Layer,
		"branch_prefix has no in-repo layer, so the personal override sits directly above global")
}

func TestResolveConfigPersonalEmptyValueStillOverrides(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "branch_prefix = \"global/\"\n")
	writePersonalConfig(t, project.ID, "branch_prefix = \"\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "", resolved.BranchPrefix, "an explicit empty personal value is a present override, not absence")

	value, ok := resolved.ResolvedValue("branch_prefix")
	require.True(t, ok)
	require.NotNil(t, value.Winner)
	assert.Equal(t, SourceProjectPersonal.String(), value.Winner.Layer)
}

func TestResolveConfigPersonalProgramOverridesMergePerLeaf(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "[program_overrides]\nclaude = \"global-claude\"\ncodex = \"global-codex\"\n")
	writeInRepoTomlConfig(t, repoRoot, "[program_overrides]\ncodex = \"repo-codex\"\n")
	writePersonalConfig(t, project.ID, "[program_overrides]\nclaude = \"personal-claude\"\ngemini = \"personal-gemini\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	overrides := resolved.ProgramOverrides
	assert.Equal(t, "personal-claude", overrides["claude"], "the personal leaf wins")
	assert.Equal(t, "repo-codex", overrides["codex"], "the in-repo leaf wins over global with no personal entry")
	assert.Equal(t, "personal-gemini", overrides["gemini"], "a personal-only leaf applies")

	value, ok := resolved.ResolvedValue("program_overrides")
	require.True(t, ok)
	require.NotNil(t, value.Origins)
	assert.Equal(t, SourceProjectPersonal.String(), value.Origins["claude"].Layer)
	assert.Equal(t, SourceRepoShared.String(), value.Origins["codex"].Layer)
	assert.Equal(t, SourceProjectPersonal.String(), value.Origins["gemini"].Layer)
}

func TestResolveConfigPersonalExplainWinnerAndReason(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "default_program = \"codex\"\n")
	writeInRepoTomlConfig(t, repoRoot, "default_program = \"aider\"\n")
	writePersonalConfig(t, project.ID, "default_program = \"gemini\"\n")

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	value, ok := resolved.ResolvedValue("default_program")
	require.True(t, ok)

	byLayer := map[string]CandidateTrace{}
	for _, c := range value.Candidates {
		byLayer[c.Layer] = c
	}

	personal, present := byLayer[SourceProjectPersonal.String()]
	require.True(t, present, "the trace must include a personal-project candidate")
	assert.True(t, personal.Allowed)
	assert.True(t, personal.Present)
	assert.Equal(t, "winner", personal.Result)

	shared := byLayer[SourceRepoShared.String()]
	assert.Equal(t, "shadowed", shared.Result)
	assert.Contains(t, shared.Reason, "personal project", "the reason must name the layer that shadowed it")
}

// TestResolveConfigPersonalCandidateDisallowedForGlobalOnlyKey pins that a
// global-only key surfaces its personal-project candidate as disallowed (not
// merely absent), which is the distinction a user needs to see that a key
// cannot be overridden per project.
func TestResolveConfigPersonalCandidateDisallowedForGlobalOnlyKey(t *testing.T) {
	_, repoRoot, _ := registeredTestProject(t)
	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	value, ok := resolved.ResolvedValue("listen_addr")
	require.True(t, ok)

	for _, c := range value.Candidates {
		if c.Layer == SourceProjectPersonal.String() {
			assert.False(t, c.Allowed, "a global-only key must never admit the personal layer")
			assert.Equal(t, "disallowed", c.Result)
			return
		}
	}
	t.Fatal("expected a personal-project candidate in the trace")
}

// TestResolveConfigDegradesOnUnreadableRegistry pins that a corrupt or
// newer-schema project record disables the personal layer with a warning rather
// than breaking config resolution for every repo — the robustness ResolveConfig
// had before it ever consulted the registry.
func TestResolveConfigDegradesOnUnreadableRegistry(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)

	id := "prj_cccccccccccccccccccccccccccccccc"
	dir := filepath.Join(home, ProjectRegistryDirName, id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	metadata := `{
  "schema_version": 999,
  "id": "prj_cccccccccccccccccccccccccccccccc",
  "checkout_id": "chk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "root": "/repo",
  "checkout_root": "/repo",
  "relative_root": "."
}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, projectMetadataFileName), []byte(metadata), 0o644))
	_, listErr := ListProjects()
	require.Error(t, listErr, "the record must genuinely make the registry unreadable")

	repoRoot := initProjectRegistryRepo(t, filepath.Join(base, "some-repo"))
	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err, "a corrupt registry must not break config resolution for every repo")

	value, ok := resolved.ResolvedValue("default_program")
	require.True(t, ok)
	for _, c := range value.Candidates {
		if c.Layer == SourceProjectPersonal.String() {
			assert.False(t, c.Present, "the personal layer degrades to absent, not a hard failure")
			return
		}
	}
	t.Fatal("expected a personal-project candidate in the trace")
}

// TestResolveGlobalConfigUnaffectedByPersonalLayer confirms the bare global read
// path is unchanged: it never appends a personal document, so default_program
// resolves over built-in and global only.
func TestResolveGlobalConfigUnaffectedByPersonalLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeGlobalTOML(t, home, "default_program = \"codex\"\n")

	resolved, err := ResolveGlobalConfig()
	require.NoError(t, err)
	assert.Equal(t, "codex", resolved.DefaultProgram)

	value, ok := resolved.ResolvedValue("default_program")
	require.True(t, ok)
	for _, c := range value.Candidates {
		assert.NotEqual(t, SourceProjectPersonal.String(), c.Layer,
			"the global-only read path must not synthesize a personal candidate")
	}
}

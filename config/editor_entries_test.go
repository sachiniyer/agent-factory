package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The editors' project read (#7009). Both config editors used to render the
// GLOBAL value for a key a project overrides, with nothing on screen saying so —
// while `af config get --repo` reported the value af actually uses. These pin
// the shared read both editors now take: the effective value, a scope marker on
// every row a non-global layer supplies, and the global value that an edit would
// write next to it.

// editorEntriesFor resolves the editor rows for repoRoot against the global
// file as it is on disk now, the same two reads the daemon and the local TUI do.
func editorEntriesFor(t *testing.T, repoRoot string) ([]ConfigEntry, string) {
	t.Helper()
	repo, err := RepoFromPath(repoRoot)
	require.NoError(t, err)
	global, err := LoadConfig()
	require.NoError(t, err)
	entries, root, err := EditorEntriesForRepo(repo, global)
	require.NoError(t, err)
	return entries, root
}

func editorEntry(t *testing.T, entries []ConfigEntry, key string) ConfigEntry {
	t.Helper()
	for _, e := range entries {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("key %q is not among the editor rows", key)
	return ConfigEntry{}
}

func TestEditorEntriesForRepoShowsTheCheckedInOverride(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	writeGlobalTOML(t, home, "default_program = \"codex\"\n")
	writeInRepoTomlConfig(t, repoRoot, "default_program = \"aider\"\n")

	entries, root := editorEntriesFor(t, repoRoot)
	assert.Equal(t, repoRoot, root, "the resolved project root is echoed so a client can say which project it shows")

	e := editorEntry(t, entries, "default_program")
	assert.Equal(t, "aider", e.Value, "the row shows what af uses in this project, not the global file's value")
	assert.Equal(t, ScopeProject, e.Scope)
	assert.Equal(t, "codex", e.GlobalValue, "the global value stays reachable from the marker — it is what an edit writes")
}

func TestEditorEntriesForRepoMarksAPersonalOverride(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "default_program = \"codex\"\n")
	writePersonalConfig(t, project.ID, "default_program = \"gemini\"\n")

	e := editorEntry(t, mustEntries(editorEntriesFor(t, repoRoot)), "default_program")
	assert.Equal(t, "gemini", e.Value)
	assert.Equal(t, ScopePersonal, e.Scope)
	assert.Equal(t, "codex", e.GlobalValue)
}

func TestEditorEntriesForRepoMarksACompositeDrawnFromBothProjectLayers(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "[program_overrides]\nclaude = \"global-claude\"\n")
	writeInRepoTomlConfig(t, repoRoot, "[program_overrides]\ncodex = \"repo-codex\"\n")
	writePersonalConfig(t, project.ID, "[program_overrides]\nclaude = \"personal-claude\"\n")

	e := editorEntry(t, mustEntries(editorEntriesFor(t, repoRoot)), "program_overrides")
	assert.Equal(t, ScopeProjectPersonal, e.Scope,
		"a map whose leaves came from the checked-in AND the personal layer is neither one alone")
	assert.Contains(t, e.Value, "personal-claude")
	assert.Contains(t, e.Value, "repo-codex")
	assert.Equal(t, `{"claude":"global-claude"}`, e.GlobalValue)
}

// TestEditorEntriesForRepoMarksAStoredButIgnoredPersonalBranchPrefix pins the
// #4539 interim state through the editor: the effective value IS the global one,
// but a marker still says a personal value is stored, because hiding it would
// leave the user unable to tell why `af config get --repo` warns about it.
func TestEditorEntriesForRepoMarksAStoredButIgnoredPersonalBranchPrefix(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "branch_prefix = \"global/\"\n")
	writePersonalConfig(t, project.ID, "branch_prefix = \"local/\"\n")

	e := editorEntry(t, mustEntries(editorEntriesFor(t, repoRoot)), "branch_prefix")
	assert.Equal(t, "global/", e.Value, "a stored personal branch_prefix is not applied, so the effective value is global")
	assert.Equal(t, ScopePersonalIgnored, e.Scope)
	assert.Equal(t, "global/", e.GlobalValue)
}

// TestEditorEntriesForRepoUsesTheFourLayerRootAgentTrace pins that root_agent's
// row comes from the specialized resolver, not the generic two-layer one: a
// personal [root_agent] disabling a globally enabled profile is a personal
// override in exactly the shape the daemon will apply.
func TestEditorEntriesForRepoUsesTheFourLayerRootAgentTrace(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "schema_version = 1\n\n[root_agent]\nenabled = true\nprogram = \"codex\"\n")
	writePersonalConfig(t, project.ID, "[root_agent]\nenabled = false\n")

	e := editorEntry(t, mustEntries(editorEntriesFor(t, repoRoot)), "root_agent")
	assert.Equal(t, ScopePersonal, e.Scope)
	var effective map[string]any
	require.NoError(t, json.Unmarshal([]byte(e.Value), &effective), "root_agent's effective value is a table: %q", e.Value)
	assert.Equal(t, false, effective["enabled"], "the personal enabled=false is what the daemon applies")
	assert.Equal(t, `{"enabled":true,"program":"codex"}`, e.GlobalValue)
}

// TestEditorEntriesForRepoMarksAGlobalRootAgentTheProjectIgnores covers the
// ignored-global shape: an unregistered project with no legacy entry does not
// run the global root agent at all. The row must not show the global table as
// though it applied — the project decided its own value.
func TestEditorEntriesForRepoMarksAGlobalRootAgentTheProjectIgnores(t *testing.T) {
	_, repoRoot := setupUnregisteredProjectWithGlobalRootAgent(t)

	e := editorEntry(t, mustEntries(editorEntriesFor(t, repoRoot)), "root_agent")
	assert.Equal(t, ScopeProject, e.Scope)
	assert.NotEqual(t, e.GlobalValue, e.Value, "the global table is not what runs here")
	assert.Equal(t, `{"enabled":true,"program":"codex"}`, e.GlobalValue)
}

// TestEditorEntriesForRepoLeavesUnoverriddenRowsExactlyGlobal is the contract
// the open-set `scope` rests on: no marker means the row IS the global row, so
// a project with no overrides reads byte-identically to the global editor.
func TestEditorEntriesForRepoLeavesUnoverriddenRowsExactlyGlobal(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	writeGlobalTOML(t, home, "default_program = \"codex\"\n[program_overrides]\nclaude = \"global-claude\"\n")

	entries, _ := editorEntriesFor(t, repoRoot)
	global, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, ManifestWithValues(global), entries,
		"a project that overrides nothing must render exactly the global rows — no marker, no reformatted value")
}

func TestEditorEntriesOmitScopeFieldsFromTheWireWhenUnmarked(t *testing.T) {
	encoded, err := json.Marshal(ConfigEntry{Key: "default_program", Value: "claude"})
	require.NoError(t, err)
	for _, field := range []string{`"scope"`, `"global_value"`} {
		assert.NotContains(t, string(encoded), field, "an unmarked row keeps today's wire shape")
	}

	decoded := ConfigEntry{}
	require.NoError(t, json.Unmarshal([]byte(`{"key":"k","value":"v","scope":"project","global_value":"g"}`), &decoded))
	assert.Equal(t, ScopeProject, decoded.Scope)
	assert.Equal(t, "g", decoded.GlobalValue)
}

func TestExplainRepoValueAnswersForTheProjectNotTheGlobalFile(t *testing.T) {
	home, repoRoot, _ := registeredTestProject(t)
	writeGlobalTOML(t, home, "default_program = \"codex\"\n")
	writeInRepoTomlConfig(t, repoRoot, "default_program = \"aider\"\n")
	repo, err := RepoFromPath(repoRoot)
	require.NoError(t, err)

	project, err := ExplainRepoValue(repo, "default_program")
	require.NoError(t, err)
	assert.Equal(t, "aider", project.Value)
	require.NotNil(t, project.Winner)
	assert.Equal(t, SourceRepoShared.String(), project.Winner.Layer)

	global, err := ExplainGlobalValue("default_program")
	require.NoError(t, err)
	assert.Equal(t, "codex", global.Value, "the global explain is unchanged by the project read")

	_, err = ExplainRepoValue(repo, "no_such_key")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown config key")
}

func TestExplainRepoValueRoutesRootAgentThroughTheFourLayerTrace(t *testing.T) {
	home, repoRoot, project := registeredTestProject(t)
	writeGlobalTOML(t, home, "schema_version = 1\n\n[root_agent]\nenabled = true\n")
	writePersonalConfig(t, project.ID, "[root_agent]\nenabled = false\n")
	repo, err := RepoFromPath(repoRoot)
	require.NoError(t, err)

	value, err := ExplainRepoValue(repo, "root_agent.enabled")
	require.NoError(t, err)
	assert.Equal(t, false, value.Value)
	assert.Equal(t, []string{"built-in", "global", "legacy root_agents", "personal project"}, value.Precedence)
}

// TestProjectScopeErrorNamesOnlyTheRequestedPath pins the remote-safety half:
// the string crosses the wire to a client that may be on another machine, so it
// says what to do about the path the client sent and never echoes the
// resolver's error, which can name the daemon host's registry, AF home, or
// personal config files.
func TestProjectScopeErrorNamesOnlyTheRequestedPath(t *testing.T) {
	const requested = "/srv/checkouts/app"
	const hostPath = "/home/operator/.agent-factory/.agent-factory-projects/prj_x/config.toml"

	cases := []struct {
		name string
		err  error
		want []string
	}{
		{"not a repository", fmt.Errorf("failed to get git repo root for %s: %w", requested, ErrNotGitRepository),
			[]string{"not a git repository: " + requested}},
		{"git did not answer", fmt.Errorf("probe %s: %w", hostPath, ErrRepoProbeUnanswered),
			[]string{"could not check " + requested, "git did not answer", "try again"}},
		{"unusable path", fmt.Errorf("%w: path %q must be absolute", ErrProjectPathUnusable, "rel"),
			[]string{"not a usable path: " + requested, "absolute"}},
		{"anything else", fmt.Errorf("decode %s: %w", hostPath, errors.New("toml: bad key")),
			[]string{"af could not read the project config for " + requested, "af config get --repo " + requested}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ProjectScopeError(requested, tc.err)
			for _, want := range tc.want {
				assert.Contains(t, got, want)
			}
			assert.NotContains(t, got, hostPath, "the daemon host's own paths never cross the wire")
			assert.NotContains(t, got, "toml: bad key", "the resolver's error text is not echoed")
			assert.NotContains(t, got, "...", "copy uses the ellipsis glyph, never three dots")
		})
	}
}

func TestEditorEntriesForRepoRefusesAMissingGlobalSnapshot(t *testing.T) {
	_, repoRoot, _ := registeredTestProject(t)
	repo, err := RepoFromPath(repoRoot)
	require.NoError(t, err)
	_, _, err = EditorEntriesForRepo(repo, nil)
	require.Error(t, err, "a project read with no global layer would mark every row against nothing")
}

func mustEntries(entries []ConfigEntry, _ string) []ConfigEntry { return entries }

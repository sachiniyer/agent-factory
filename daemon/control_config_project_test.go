package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// GetConfig/ExplainConfig's optional project scope (#7009). The web and a remote
// TUI cannot read the project's config themselves, so they name the project and
// the daemon resolves it. Everything new is optional: a request without
// repo_path must answer exactly as it always has, because third-party clients
// read this route too.

// projectConfigFixture gives the daemon an AF home with a global value and a
// real checkout whose checked-in config overrides it.
func projectConfigFixture(t *testing.T) (home, repoRoot string) {
	t.Helper()
	base := t.TempDir()
	home = filepath.Join(base, "af-home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName), []byte("default_program = \"codex\"\n"), 0o644))

	repoRoot = filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, config.InRepoConfigDirName), 0o755))
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, config.InRepoConfigDirName, config.TomlConfigFileName),
		[]byte("default_program = \"aider\"\n"), 0o644))
	repo, err := config.RepoFromPath(repoRoot)
	require.NoError(t, err)
	return home, repo.Root
}

func configEntryByKey(t *testing.T, entries []config.ConfigEntry, key string) config.ConfigEntry {
	t.Helper()
	for _, e := range entries {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("key %q is not among GetConfig's entries", key)
	return config.ConfigEntry{}
}

// TestGetConfigWithoutRepoPathIsByteIdenticalToTheGlobalRead pins the
// compatibility promise: an empty request is today's response, field for field
// and byte for byte, so no client that never sends repo_path sees a change.
func TestGetConfigWithoutRepoPathIsByteIdenticalToTheGlobalRead(t *testing.T) {
	home, _ := projectConfigFixture(t)
	cs := &controlServer{}

	var resp GetConfigResponse
	require.NoError(t, cs.GetConfig(GetConfigRequest{}, &resp))

	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	want, err := json.Marshal(struct {
		Entries []config.ConfigEntry `json:"entries"`
		Path    string               `json:"path"`
	}{config.ManifestWithValues(cfg), filepath.Join(home, config.TomlConfigFileName)})
	require.NoError(t, err)
	got, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
	for _, field := range []string{`"scope"`, `"global_value"`, `"project_root"`, `"project_error"`} {
		assert.NotContains(t, string(got), field, "a global read carries none of the project fields")
	}
}

func TestGetConfigWithRepoPathReturnsTheProjectsEffectiveValues(t *testing.T) {
	_, repoRoot := projectConfigFixture(t)
	cs := &controlServer{}

	var resp GetConfigResponse
	require.NoError(t, cs.GetConfig(GetConfigRequest{RepoPath: repoRoot}, &resp))

	assert.Equal(t, repoRoot, resp.ProjectRoot, "the daemon echoes the project it resolved")
	assert.Empty(t, resp.ProjectError)
	e := configEntryByKey(t, resp.Entries, "default_program")
	assert.Equal(t, "aider", e.Value)
	assert.Equal(t, config.ScopeProject, e.Scope)
	assert.Equal(t, "codex", e.GlobalValue)
	assert.NotEmpty(t, resp.Path, "the file an edit writes is still named")
}

// TestGetConfigProjectErrorNamesOnlyTheRequestedPath: a remote caller learns
// what to do about the path it sent and nothing about the daemon host's own
// layout, and the form still opens on global rows.
func TestGetConfigProjectErrorNamesOnlyTheRequestedPath(t *testing.T) {
	home, _ := projectConfigFixture(t)
	cs := &controlServer{}

	for _, tc := range []struct {
		name, requested, want string
	}{
		{"not a repository", t.TempDir(), "not a git repository: "},
		{"relative path", "some/relative/dir", "not a usable path: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resp GetConfigResponse
			require.NoError(t, cs.GetConfig(GetConfigRequest{RepoPath: tc.requested}, &resp),
				"an unresolvable project degrades to global rows rather than failing the read")
			assert.Contains(t, resp.ProjectError, tc.want+tc.requested)
			assert.NotContains(t, resp.ProjectError, home, "the daemon's AF home never crosses the wire")
			assert.Empty(t, resp.ProjectRoot)
			e := configEntryByKey(t, resp.Entries, "default_program")
			assert.Equal(t, "codex", e.Value, "the degraded rows are the global ones")
			assert.Empty(t, e.Scope)
		})
	}
}

func TestExplainConfigWithRepoPathExplainsTheProject(t *testing.T) {
	_, repoRoot := projectConfigFixture(t)
	cs := &controlServer{}

	var global ExplainConfigResponse
	require.NoError(t, cs.ExplainConfig(ExplainConfigRequest{Key: "default_program"}, &global))
	assert.Equal(t, "global", global.Scope)
	assert.Equal(t, "codex", global.Explanation.Value)

	var project ExplainConfigResponse
	require.NoError(t, cs.ExplainConfig(ExplainConfigRequest{Key: "default_program", RepoPath: repoRoot}, &project))
	assert.Equal(t, "project", project.Scope)
	assert.Equal(t, "aider", project.Explanation.Value)
	require.NotNil(t, project.Explanation.Winner)
	assert.Equal(t, config.SourceRepoShared.String(), project.Explanation.Winner.Layer)
}

// TestExplainConfigRefusesAProjectItCannotResolve: unlike the form read, an
// explain that resolved nothing has nothing honest to say — a global trace
// labelled as the project's would be wrong — so it refuses, in the same
// requested-path-only wording.
func TestExplainConfigRefusesAProjectItCannotResolve(t *testing.T) {
	home, _ := projectConfigFixture(t)
	cs := &controlServer{}
	requested := t.TempDir()

	var resp ExplainConfigResponse
	err := cs.ExplainConfig(ExplainConfigRequest{Key: "default_program", RepoPath: requested}, &resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a git repository: "+requested)
	assert.False(t, strings.Contains(err.Error(), home), "the daemon's AF home never crosses the wire: %v", err)
}

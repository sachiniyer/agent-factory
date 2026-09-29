package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// The project-scope read behind GetConfig's repo_path (config.read-project):
// the DAEMON resolves the selector on its own filesystem and answers the same
// project-effective stack `af config list --repo` reads, so the TUI's remote
// `p` picker and the web scope select get the daemon host's truth — never a
// global read wearing a project label.

func getConfigEntry(t *testing.T, entries []config.ConfigEntry, key string) config.ConfigEntry {
	t.Helper()
	for _, e := range entries {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("GetConfig returned no entry for %q", key)
	return config.ConfigEntry{}
}

func TestGetConfigResolvesTheProjectScope(t *testing.T) {
	home, repoPath, project := defaultAccountFixture(t, "", "")

	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("default_program = \"claude\"\n"), 0o600))
	inRepo := filepath.Join(repoPath, ".agent-factory")
	require.NoError(t, os.MkdirAll(inRepo, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(inRepo, config.TomlConfigFileName),
		[]byte("default_program = \"codex\"\n"), 0o644))
	writeProjectAccounts(t, project, "branch_prefix = \"proj/\"\n")

	cs := &controlServer{}

	var scoped GetConfigResponse
	require.NoError(t, cs.GetConfig(GetConfigRequest{RepoPath: repoPath}, &scoped))
	assert.Equal(t, "codex", getConfigEntry(t, scoped.Entries, "default_program").Value,
		"the in-repo layer must win over the daemon's global file")
	assert.Equal(t, "proj/", getConfigEntry(t, scoped.Entries, "branch_prefix").Value,
		"the personal project layer must land on top")

	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	assert.Equal(t, repo.Root, scoped.ProjectRoot,
		"the answer names the canonical root it resolved, not the selector it was sent")
	assert.Equal(t, filepath.Join(home, config.TomlConfigFileName), scoped.Path,
		"Path stays the daemon's global file — the added layers live elsewhere")

	// The empty selector keeps the historical answer: global file, no scope.
	var global GetConfigResponse
	require.NoError(t, cs.GetConfig(GetConfigRequest{}, &global))
	assert.Equal(t, "claude", getConfigEntry(t, global.Entries, "default_program").Value)
	assert.Empty(t, global.ProjectRoot)
}

// A selector that does not resolve on the daemon host is an error — the
// alternative (silently answering global) would label the wrong file's values
// as a project's, which is the exact misread this scope exists to prevent.
func TestGetConfigRejectsAnUnresolvableProjectPath(t *testing.T) {
	home, _, _ := defaultAccountFixture(t, "", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("default_program = \"claude\"\n"), 0o600))

	cs := &controlServer{}
	var resp GetConfigResponse
	err := cs.GetConfig(GetConfigRequest{RepoPath: t.TempDir()}, &resp)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "cannot resolve project config"),
		"the error must name the failed scope, got %v", err)
}

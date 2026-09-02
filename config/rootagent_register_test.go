package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func TestDeregisterRootAgentsForRepoRemovesMatchAndPreservesOthers(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	// Two opt-ins for gone repos (RepoFromPath won't resolve, so matching falls
	// back to hashing the cleaned path) plus an unrelated config key to prove the
	// write preserves it.
	seed := DefaultConfig()
	seed.DefaultProgram = "codex"
	seed.RootAgents = map[string]RootAgentConfig{"/repos/gone": {}, "/repos/keep": {}}
	require.NoError(t, SaveConfig(seed))

	removed, err := DeregisterRootAgentsForRepo(RepoIDFromRoot("/repos/gone"))
	require.NoError(t, err)
	assert.Equal(t, []string{"/repos/gone"}, removed)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.NotContains(t, cfg.RootAgents, "/repos/gone", "the matched opt-in must be removed")
	assert.Contains(t, cfg.RootAgents, "/repos/keep", "an unrelated opt-in must survive")
	assert.Equal(t, "codex", cfg.DefaultProgram, "an unrelated config key must survive the write")
}

func TestDeregisterRootAgentsForRepoUnknownIsNoOp(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	seed := DefaultConfig()
	seed.RootAgents = map[string]RootAgentConfig{"/repos/keep": {}}
	require.NoError(t, SaveConfig(seed))

	removed, err := DeregisterRootAgentsForRepo(RepoIDFromRoot("/repos/never-registered"))
	require.NoError(t, err)
	assert.Nil(t, removed, "deregistering an unknown repo removes nothing")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Contains(t, cfg.RootAgents, "/repos/keep")
}

// TestDeregisterRootAgentsForRepoSweepsAllIdentitiesInOneWrite pins the #3299
// round-7 review: a re-attributed project carries two identities (real and
// derived recorded-path), and the delete's nothing-was-changed guarantee only
// holds if both are swept in ONE read-modify-write — two writes could remove
// one opt-in and then fail.
func TestDeregisterRootAgentsForRepoSweepsAllIdentitiesInOneWrite(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	seed := DefaultConfig()
	seed.RootAgents = map[string]RootAgentConfig{"/repos/real": {}, "/repos/derived": {}, "/repos/keep": {}}
	require.NoError(t, SaveConfig(seed))

	removed, err := DeregisterRootAgentsForRepo(RepoIDFromRoot("/repos/real"), RepoIDFromRoot("/repos/derived"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"/repos/real", "/repos/derived"}, removed)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.NotContains(t, cfg.RootAgents, "/repos/real")
	assert.NotContains(t, cfg.RootAgents, "/repos/derived")
	assert.Contains(t, cfg.RootAgents, "/repos/keep")
}

// TestDeregisterRootAgentsMatchesASymlinkSpelledKey pins #3530 review id
// 3918120733.
//
// A root_agents key is written by a human, through whatever symlink they had,
// while the id a delete supplies is derived from the path the registry
// RESOLVED. Comparing the two lexically makes them unequal wherever an ancestor
// is a symlink — macOS `/var` -> `/private/var` every time — so the durable
// opt-in survives a delete that reported success and recreates the root when
// the checkout returns.
func TestDeregisterRootAgentsMatchesASymlinkSpelledKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	base := testguard.CanonicalTempDir(t)
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	recorded := filepath.Join(real, "checkout")
	spelled := filepath.Join(link, "checkout")
	// The checkout is GONE — which is the only state in which the matcher's
	// hash fallback runs at all.
	if RepoIDFromRoot(spelled) == RepoIDFromRoot(recorded) {
		t.Fatalf("fixture must produce two spellings that hash differently")
	}

	cfg := DefaultConfig()
	cfg.RootAgents = map[string]RootAgentConfig{spelled: {}}
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	removed, err := DeregisterRootAgentsForRepo(RepoIDFromRoot(recorded))
	if err != nil {
		t.Fatalf("DeregisterRootAgentsForRepo: %v", err)
	}
	if len(removed) != 1 || removed[0] != spelled {
		t.Fatalf("a key spelled through a symlink must still be swept for the identity its resolved path derives: removed %v", removed)
	}
}

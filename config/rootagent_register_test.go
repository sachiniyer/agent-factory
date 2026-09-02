package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

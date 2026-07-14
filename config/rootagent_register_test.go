package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterRootAgentAddsAndIsIdempotent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	// Seed a config with an unrelated key so we can prove the write preserves it.
	seed := DefaultConfig()
	seed.DefaultProgram = "codex"
	require.NoError(t, SaveConfig(seed))

	added, err := RegisterRootAgent("/repos/new-project")
	require.NoError(t, err)
	assert.True(t, added, "first registration should add the entry")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Contains(t, cfg.RootAgents, "/repos/new-project")
	// The unrelated key must survive the load-modify-persist write.
	assert.Equal(t, "codex", cfg.DefaultProgram)

	// Second call is a no-op.
	added, err = RegisterRootAgent("/repos/new-project")
	require.NoError(t, err)
	assert.False(t, added, "re-registering the same repo should be a no-op")

	cfg, err = LoadConfig()
	require.NoError(t, err)
	assert.Len(t, cfg.RootAgents, 1)
}

func TestRegisterRootAgentPreservesExistingEntries(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	seed := DefaultConfig()
	seed.RootAgents = map[string]RootAgentConfig{"/repos/existing": {}}
	require.NoError(t, SaveConfig(seed))

	_, err := RegisterRootAgent("/repos/added")
	require.NoError(t, err)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Contains(t, cfg.RootAgents, "/repos/existing")
	require.Contains(t, cfg.RootAgents, "/repos/added")
}

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

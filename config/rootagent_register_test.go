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

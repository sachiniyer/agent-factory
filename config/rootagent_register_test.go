package config

import (
	"fmt"
	"sync"
	"testing"
	"time"

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

// TestRegisterRootAgentConcurrentRegistrationsAllPersist pins that the
// load-modify-persist sequence is atomic (#1838). Each call re-marshals the
// whole Config, so when the read is not inside the same lock as the write, the
// registrations interleave and all but the last writer's entry is lost.
func TestRegisterRootAgentConcurrentRegistrationsAllPersist(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	require.NoError(t, SaveConfig(DefaultConfig()))

	const registrations = 8
	errs := make([]error, registrations)
	var wg sync.WaitGroup
	for i := 0; i < registrations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = RegisterRootAgent(fmt.Sprintf("/repos/p%d", i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoErrorf(t, err, "registration %d", i)
	}

	cfg, err := LoadConfig()
	require.NoError(t, err)
	for i := 0; i < registrations; i++ {
		assert.Containsf(t, cfg.RootAgents, fmt.Sprintf("/repos/p%d", i),
			"registration %d was lost to a concurrent write", i)
	}
	assert.Len(t, cfg.RootAgents, registrations)
}

// TestRegisterRootAgentDoesNotClobberConcurrentConfigSet pins the reported
// #1838 scenario end to end: the TUI's "+ Add project…" write (RegisterRootAgent)
// racing `af config set` (SetGlobalConfigValue). The hook drives the config set
// into the exact window between this registration's read and its write, which
// is where the CLI's change used to be reverted by the TUI's stale snapshot.
func TestRegisterRootAgentDoesNotClobberConcurrentConfigSet(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	seed := DefaultConfig()
	seed.DefaultProgram = "claude"
	require.NoError(t, SaveConfig(seed))

	var setErr error
	setDone := make(chan struct{})
	rootAgentSaveRaceHookForTest = func() {
		go func() {
			defer close(setDone)
			_, setErr = SetGlobalConfigValue("default_program", "codex")
		}()
		// Give the racing writer every chance to land its write before we
		// persist. Holding the config lock, it cannot: it blocks until we
		// release and then edits the file we wrote. Without the lock it
		// completes here and our stale snapshot reverts it.
		select {
		case <-setDone:
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Cleanup(func() { rootAgentSaveRaceHookForTest = nil })

	added, err := RegisterRootAgent("/repos/added")
	require.NoError(t, err)
	require.True(t, added)

	select {
	case <-setDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the concurrent `af config set` never completed")
	}
	require.NoError(t, setErr)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Contains(t, cfg.RootAgents, "/repos/added", "the registration must persist")
	assert.Equal(t, "codex", cfg.DefaultProgram, "the concurrent `af config set` must not be reverted")
}

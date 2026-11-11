package session

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7080: a row loaded as Lost can go live WITHOUT Recover — the status poll
// lifts it to Ready or Running when it sees its pane alive — and a tab added
// after that is built from the agent's tmux handle. A spawn reads its own
// environment: the load sets the session environment on that handle as it
// always did, best-effort, and a list the load could not read is read by the
// spawn itself, which refuses if it still cannot.

const inertEnvName = "AF_7080_PASSTHROUGH"

// sessionEnvHome is a readable project whose global config passes inertEnvName
// through to the session's panes, with the variable set in the environment.
func sessionEnvHome(t *testing.T) *checkoutProbe {
	t.Helper()
	p := newCheckoutProbe(t)
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	cfg.SessionEnvPassthrough = []string{inertEnvName}
	require.NoError(t, config.SaveConfig(cfg))
	t.Setenv(inertEnvName, "present")
	t.Setenv("SHELL", "/bin/sh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))
	return p
}

// spawnCarriesSessionEnv reports whether the tmux chain whose new-session
// created sessionName admitted the pass-through name: tmux imports it from the
// client environment through the chain's update-environment list.
func spawnCarriesSessionEnv(commands []string, sessionName string) (spawned, carried bool) {
	for _, command := range commands {
		if !strings.Contains(command, "new-session -d -s "+sessionName+" ") {
			continue
		}
		spawned = true
		if strings.Contains(command, inertEnvName) {
			carried = true
		}
	}
	return spawned, carried
}

// loadLostThenRevive loads a row recorded Lost whose agent pane is in fact
// alive, then lifts it the way the status poll does on seeing that pane.
func loadLostThenRevive(t *testing.T, p *checkoutProbe, title, agentName string) (*Instance, func() []string) {
	t.Helper()
	commands := mockRestoredTmux(t, agentName)
	worktree := t.TempDir()
	instance, err := FromInstanceData(loadTestRow(p, title, agentName, Lost, worktree))
	require.NoError(t, err, "a Lost row loads whatever its project config says")
	require.NoError(t, instance.Transition(ObserveLiveness(LiveReady)), "the poll lifts the live pane's row")
	return instance, commands
}

func TestRevivedLostRowSpawnsATabWithTheSessionEnvironment(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	p := sessionEnvHome(t)
	instance, commands := loadLostThenRevive(t, p, "lost-revived", "af_lost_revived")

	tab, err := instance.AddShellTab()
	require.NoError(t, err)

	spawned, carried := spawnCarriesSessionEnv(commands(), tab.tmux.SanitizedName())
	require.True(t, spawned, "precondition: the tab was spawned")
	require.True(t, carried, "a tab spawned on a revived Lost row carries the session's environment pass-through")
}

// The load could not read the config: it still loads, and the tab spawn reads
// the environment itself once the config answers.
func TestLostRowLoadedOnAnUnreadableConfigReadsItsEnvironmentAtTabSpawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	p := sessionEnvHome(t)
	p.slow(t)
	instance, commands := loadLostThenRevive(t, p, "lost-unknown", "af_lost_unknown")
	require.NoError(t, os.Remove(p.flag)) // the box answers again

	tab, err := instance.AddShellTab()
	require.NoError(t, err)

	spawned, carried := spawnCarriesSessionEnv(commands(), tab.tmux.SanitizedName())
	require.True(t, spawned)
	require.True(t, carried, "the spawn read the environment the load could not")
}

// ...and while the config still cannot be read, the spawn refuses with the
// actionable unknown-config error rather than start a tab without the names.
func TestLostRowTabSpawnRefusesWhileItsEnvironmentIsUnknown(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	noRetryPause(t)
	p := sessionEnvHome(t)
	p.slow(t)
	instance, commands := loadLostThenRevive(t, p, "lost-still-unknown", "af_lost_still_unknown")

	tab, err := instance.AddShellTab()

	require.Nil(t, tab)
	require.True(t, isProjectConfigUnknown(err), "the spawn refuses on the unknown, actionably: %v", err)
	for _, command := range commands() {
		require.NotContains(t, command, "new-session", "nothing was spawned without its environment")
	}
}

// #7080 review: concurrent tab creates on a row whose environment the load
// could not read resolve it once. Each refresh publishes the worktree's hook
// list, which its git commands read; two at once raced on it.
func TestConcurrentSpawnsResolveAnUnreadEnvironmentOnce(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	p := sessionEnvHome(t)
	p.slow(t)
	instance, _ := loadLostThenRevive(t, p, "lost-concurrent", "af_lost_concurrent")
	require.True(t, instance.envUnresolved.Load(), "precondition: the load could not read the environment")
	require.NoError(t, os.Remove(p.flag)) // the box answers again
	instance.mu.RLock()
	agentTmux, gw := instance.tmuxLocked(), instance.gitWorktree
	instance.mu.RUnlock()
	before := p.count()

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for k := range errs {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			<-start
			errs[k] = instance.ensureSpawnEnvironment(agentTmux, gw)
		}(k)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.False(t, instance.envUnresolved.Load())
	require.Equal(t, 2, p.count()-before,
		"one spawn reads the session and worktree lists; the others find them resolved")
}

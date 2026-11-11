package session

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7083: a row saved startup-unknown loads INERT and returns before Start, so
// its agent handle never ran Provision — it carries no environment
// pass-through and no account. A handoff retry or confirm then resolves the
// startup state (started=true, no respawn), and a tab built from that handle
// started bare: no session_env_passthrough names, and the DAEMON's identity
// instead of the session's account. A handle counts as environment-resolved
// only after a refresh succeeded on it; a spawn from any other resolves first
// or refuses.

// spawnChain is the tmux chain whose new-session created sessionName.
func spawnChain(commands []string, sessionName string) (string, bool) {
	for _, command := range commands {
		if strings.Contains(command, "new-session -d -s "+sessionName+" ") {
			return command, true
		}
	}
	return "", false
}

// resolvedStartupUnknownRow loads an account-scoped row recorded
// startup-unknown whose pane is alive, then resolves its startup state the way
// a handoff retry or confirm does.
func resolvedStartupUnknownRow(t *testing.T, p *checkoutProbe, title, agentName string) (*Instance, func() []string) {
	t.Helper()
	commands := mockRestoredTmux(t, agentName)
	row := loadTestRow(p, title, agentName, Running, t.TempDir())
	row.Account = "work"
	row.StartupStateUnknown = true
	instance, err := FromInstanceData(row)
	require.NoError(t, err)
	require.True(t, instance.StartupStateUnknown(), "precondition: the row loaded inert, before Start")
	instance.ResolveStartupState()
	return instance, commands
}

func TestTabOnAResolvedStartupUnknownRowCarriesTheSessionEnvironmentAndAccount(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	p := sessionEnvHome(t)
	accountDir := registerAccount(t, tmux.ProgramCodex, "work")
	instance, commands := resolvedStartupUnknownRow(t, p, "unknown-startup", "af_unknown_startup")

	tab, err := instance.AddShellTab()
	require.NoError(t, err)

	chain, spawned := spawnChain(commands(), tab.tmux.SanitizedName())
	require.True(t, spawned, "precondition: the tab was spawned")
	require.Contains(t, chain, inertEnvName, "the tab carries the session's environment pass-through")
	require.Contains(t, chain, accountDir, "the tab runs as the session's account, never the daemon's own identity")
}

// While the environment still cannot be read, the spawn refuses rather than
// start the tab bare.
func TestTabOnAnUnprovisionedHandleRefusesWhileItsEnvironmentIsUnknown(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	noRetryPause(t)
	p := sessionEnvHome(t)
	registerAccount(t, tmux.ProgramCodex, "work")
	instance, commands := resolvedStartupUnknownRow(t, p, "unknown-startup-slow", "af_unknown_startup_slow")
	p.slow(t)

	tab, err := instance.AddShellTab()

	require.Nil(t, tab)
	require.True(t, isProjectConfigUnknown(err), "the spawn refuses on the unknown, actionably: %v", err)
	for _, command := range commands() {
		require.NotContains(t, command, "new-session", "nothing was spawned bare")
	}
}

// P3-1: Recover, a respawn and a swap refresh both lists before they spawn. A
// handle a refresh succeeded on is resolved — the next tab neither pays
// another config read nor is refused under load with fresh handles.
func TestARefreshThatSucceededResolvesTheHandleForTabSpawns(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	p := sessionEnvHome(t)
	p.slow(t)
	instance, commands := loadLostThenRevive(t, p, "lost-recovered", "af_lost_recovered")
	require.False(t, envResolvedOnHandles(instance), "precondition: the load could not read the environment")
	require.NoError(t, os.Remove(p.flag))

	// What Recover's respawn runs before it spawns (backend_local_respawn.go).
	instance.mu.RLock()
	agentTmux, gw := instance.tmuxLocked(), instance.gitWorktree
	instance.mu.RUnlock()
	require.NoError(t, refreshWorktreeEnvironment(instance, gw))
	require.NoError(t, refreshSessionEnvironment(instance, agentTmux))
	require.True(t, envResolvedOnHandles(instance))

	p.slow(t) // a load spike after the recovery
	before := p.count()
	tab, err := instance.AddShellTab()

	require.NoError(t, err, "fresh handles need no config read to spawn a tab")
	require.Equal(t, before, p.count(), "the spawn read nothing")
	chain, spawned := spawnChain(commands(), tab.tmux.SanitizedName())
	require.True(t, spawned)
	require.Contains(t, chain, inertEnvName)
}

// P3-2: an inert load with a readable config applies the worktree's list too —
// every git command its worktree runs reads it (#7080).
func TestInertLoadResolvesTheWorktreeEnvironment(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	p := sessionEnvHome(t)
	instance, _ := loadLostThenRevive(t, p, "lost-readable", "af_lost_readable")

	instance.mu.RLock()
	gw := instance.gitWorktree
	instance.mu.RUnlock()
	require.NotNil(t, gw)
	require.Same(t, gw, instance.envResolvedWorktree.Load(), "the inert load applied the worktree's pass-through")
	require.Contains(t, gw.HookEnvironment(), inertEnvName)
	require.True(t, envResolvedOnHandles(instance))
}

package daemon

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// #7088: follow-ups to the root heal's config snapshot (#7084, #7086).

// resetRootEnsureBackoff makes every root candidate due on the next pass.
func resetRootEnsureBackoff(m *Manager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, st := range m.rootEnsureStates {
		st.nextAttempt = time.Time{}
	}
}

// Item 1: the create's snapshot answers its config reads only while the create
// runs. Once it returns, the root reads its config afresh — or it would launch
// Recover, a respawn and every tab with the config as it stood at the create,
// until the daemon restarted.
func TestRootReadsItsConfigAfreshOnceItsCreateReturns(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()
	root := findRootInstance(t, manager, repoPath)
	require.NotNil(t, root, "precondition: the ensure created the root")

	const name = "AF_ROOT_AFTER_CREATE"
	before, err := session.SessionEnvPassthroughForInstance(root)
	require.NoError(t, err)
	require.NotContains(t, before, name, "precondition")
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	cfg.SessionEnvPassthrough = append(cfg.SessionEnvPassthrough, name)
	require.NoError(t, config.SaveConfig(cfg))

	after, err := session.SessionEnvPassthroughForInstance(root)
	require.NoError(t, err)
	require.True(t, slices.Contains(after, name), "the root reads the config as it is now, not the create's snapshot: %v", after)
}

// Item 2: a heal reaps the dead root's record, and the create can still fail
// on a transient after that — the in-place admission's `git worktree list`
// runs with a 10s bound. The next pass then found no record to reap and
// created a blank root. A heal keeps the carried state until a replacement
// launches: the next pass's create carries it.
func TestRootHealKeepsItsCarriedStateWhenItsCreateFailsOnATransientAfterTheReap(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	writeRootClaudeTranscript(t, claudeConfigDir, repoPath, priorRootConversationID)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first, "precondition: the first ensure creates the root")
	prior := seedRootConversation(t, first)

	// git stops answering the worktree listing right after the reap.
	var stalled atomic.Bool
	prevBindings, prevHook := worktreeBranchBindings, testHookRootAfterReap
	worktreeBranchBindings = func(repo string) ([]git.WorktreeBranchBinding, error) {
		if stalled.Load() {
			return nil, fmt.Errorf("git worktree list: %w", context.DeadlineExceeded)
		}
		return prevBindings(repo)
	}
	testHookRootAfterReap = func() { stalled.Store(true) }
	t.Cleanup(func() { worktreeBranchBindings, testHookRootAfterReap = prevBindings, prevHook })

	first.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()
	require.Nil(t, findRootInstance(t, manager, repoPath), "precondition: the heal's create failed after the reap")

	stalled.Store(false) // git answers again, and the next pass is due
	resetRootEnsureBackoff(manager)
	manager.ensureRootAgentsAndWait()

	healed := findRootInstance(t, manager, repoPath)
	require.NotNil(t, healed, "the next pass re-creates the root")
	last := (*seen)[len(*seen)-1]
	require.Equal(t, prior.ID, last.ResumeConversation.ID,
		"the replacement resumes the conversation the reaped root carried, not a blank one")
}

// Item 4: an explicit profile program needs no config to be picked, so a
// refusal on an unknown config must not say the program could not be picked —
// it names the config, the thing that could not be read.
func TestRootRefusalForAnExplicitProgramNamesTheConfigNotTheProgram(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	warnings := captureWarnings(t)
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	project := registerTestProject(t, repoPath)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/root\"")
	unknownLaunchConfig(t)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()

	require.Nil(t, findRootInstance(t, manager, repoPath), "precondition: the create refused")
	out := warnings.String()
	require.Contains(t, out, `cannot start the root agent "/opt/root"`)
	require.Contains(t, out, "couldn't read the project config")
	require.NotContains(t, out, "cannot pick the root agent's program")
}

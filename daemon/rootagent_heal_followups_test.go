package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
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

// #7090 review: a legacy root_agents entry has no checkout identity, and its
// path can be retargeted at another repository. A carry is bound to the
// repository and workspace it was reaped from, so a changed target drops it
// instead of handing one project's conversation and tabs to another's root.
func TestAHeldRootCarryIsBoundToTheRepositoryItCameFrom(t *testing.T) {
	m := &Manager{rootEnsureStates: map[string]*rootEnsureState{}}
	st := &rootEnsureState{}
	m.rootEnsureStates["/legacy/path"] = st
	a := &config.RepoContext{Root: "/repo-a", ID: "repo-a"}
	b := &config.RepoContext{Root: "/repo-b", ID: "repo-b"}
	carried := reapedRootState{conversation: session.AgentConversationData{Agent: tmux.ProgramClaude, ID: "conversation-a"}}

	holdCarryForTest(m, st, a, nil, carried)
	got, held := m.heldRootCarry(st, a, nil)
	require.True(t, held, "precondition: the same target gets its carry")
	require.Equal(t, carried.conversation.ID, got.conversation.ID)

	_, held = m.heldRootCarry(st, b, nil)
	require.False(t, held, "a path retargeted at another repository does not inherit the old one's carry")
	_, held = m.heldRootCarry(st, a, nil)
	require.False(t, held, "and the mismatched carry was dropped, not kept for a later pass")
}

// #7090 review: two root_agents spellings of one repository are two ensure
// candidates with their own retry state. A root one of them launches is newer
// than what the other holds, so the launch retires every candidate's carry for
// the repository — a later heal by the other reaps that root's state rather
// than replacing it with the older snapshot.
func TestARootLaunchedForTheRepositoryRetiresEveryCandidatesHeldCarry(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	// The other spelling's heal failed after its reap and holds what it carried.
	other := &rootEnsureState{}
	manager.mu.Lock()
	manager.rootEnsureStates["other spelling of the repository"] = other
	manager.mu.Unlock()
	holdCarryForTest(manager, other, repo, nil,
		reapedRootState{conversation: session.AgentConversationData{Agent: tmux.ProgramClaude, ID: "older-conversation"}})

	manager.ensureRootAgentsAndWait() // this candidate launches the root
	require.NotNil(t, findRootInstance(t, manager, repoPath), "precondition: the root launched")

	_, held := manager.heldRootCarry(other, repo, nil)
	require.False(t, held, "the launched root retires the other candidate's older carry")
}

// #7090 review, P3-2: deleting a project retires its held root carries. The
// ensure loop clears a candidate's carry only when it visits the candidate, and
// one in backoff goes unvisited for minutes; a tombstone released in that time
// — another clone proven at the same path — would otherwise hand the deleted
// project's conversation and tabs to the unrelated clone through a legacy
// root_agents carry, which has no checkout to tell the two apart.
func TestDeletingAProjectRetiresItsHeldRootCarries(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")
	repo := &config.RepoContext{Root: repoPath, ID: repoID}
	held := &rootEnsureState{}
	manager.mu.Lock()
	manager.rootEnsureStates[repoPath] = held // the legacy root_agents spelling
	manager.mu.Unlock()
	holdCarryForTest(manager, held, repo, nil,
		reapedRootState{conversation: session.AgentConversationData{Agent: tmux.ProgramClaude, ID: "deleted-projects-conversation"}})

	_, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.NoError(t, err)

	_, stillHeld := manager.heldRootCarry(held, repo, nil)
	require.False(t, stillHeld, "a deleted project's carry must not outlive it for a clone at the same path to inherit")
}

// #7090 review, P3-4: a held carry outranks a record reaped since. While it is
// held no replacement has launched, so any root record reaped in the meantime
// is a failed replacement (persistFailedCreate can leave one): the heal carries
// the last working root's conversation, not the failed one's.
func TestAHeldRootCarryOutranksARecordReapedSince(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	const heldID = "019f4a2e-7206-7fc2-803b-f7045e07a244"
	writeRootClaudeTranscript(t, claudeConfigDir, repoPath, priorRootConversationID)
	writeRootClaudeTranscript(t, claudeConfigDir, repoPath, heldID)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()
	failed := findRootInstance(t, manager, repoPath)
	require.NotNil(t, failed, "precondition: a root record exists")
	reaped := seedRootConversation(t, failed) // the failed replacement's conversation
	require.NotEqual(t, heldID, reaped.ID)

	// The last working root's state, held by a heal whose create failed.
	repo, err := resolveLegacyRootRepo(repoPath)
	require.NoError(t, err)
	manager.mu.Lock()
	st := manager.rootEnsureStates[repoPath]
	manager.mu.Unlock()
	require.NotNil(t, st, "precondition: the candidate's ensure state")
	holdCarryForTest(manager, st, repo, nil, reapedRootState{conversation: session.AgentConversationData{
		Agent: tmux.ProgramClaude, ID: heldID, CaptureKind: session.ConversationCaptureInjected,
	}})

	failed.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

	require.NotNil(t, findRootInstance(t, manager, repoPath), "the heal re-creates the root")
	last := (*seen)[len(*seen)-1]
	require.Equal(t, heldID, last.ResumeConversation.ID, "the held carry, not the record reaped since, is what the replacement resumes")
	_, stillHeld := manager.heldRootCarry(st, repo, nil)
	require.False(t, stillHeld, "the launch retires it")
}

// PR #7190 review F4 (P3): retiring carries at a deleted path compared the
// delete's resolved path against the carry's workspace as written. A
// registry-backed root's workspace is its recorded root as stored, which can
// run through a symlink, so a delete of that vanished checkout missed the carry.
// Both sides must be in the same resolved form.
func TestRetiringCarriesAtADeletedPathResolvesTheCarrySideToo(t *testing.T) {
	base := testguard.CanonicalTempDir(t)
	physical := filepath.Join(base, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(physical, "project"), 0o755))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(physical, link))
	recorded := filepath.Join(link, "project") // the registry root as stored

	m := &Manager{rootEnsureStates: map[string]*rootEnsureState{}}
	st := &rootEnsureState{}
	m.rootEnsureStates[recorded] = st
	repo := &config.RepoContext{Root: recorded, ID: "registry-repo"}
	holdCarryForTest(m, st, repo, nil, reapedRootState{conversation: session.AgentConversationData{
		Agent: tmux.ProgramClaude, ID: "deleted-projects-conversation",
	}})

	require.NoError(t, os.RemoveAll(filepath.Join(physical, "project")), "the checkout vanishes")
	deleted := pathutil.ResolveForCompare(filepath.Clean(recorded)) // what the delete computes
	require.NotEqual(t, recorded, deleted, "fixture: the delete's form resolves the symlink")
	m.mu.Lock()
	m.retireRootCarriesAtLocked(deleted)
	m.mu.Unlock()

	_, held := m.heldRootCarry(st, repo, nil)
	require.False(t, held, "a carry recorded through a symlink must be retired by a delete of the path it names")
}

// PR #7190 review F5 (P3): a tick on which the deletion tombstone applies used
// to clear the candidate's carry through rootEnsureSucceeded — a tombstone check
// by proxy. A released tombstone whose proof lapses for a tick then cost an
// occupant's heal its conversation. The deleted project's own carries are
// retired by suppressRootAgent at the delete; a tombstoned tick leaves the rest
// alone.
func TestATombstonedTickLeavesTheCarryToTheDelete(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager.mu.Lock()
	st := manager.rootEnsureStates[repoPath]
	manager.mu.Unlock()
	require.NotNil(t, st, "precondition: the candidate's ensure state")
	holdCarryForTest(manager, st, repo, nil, reapedRootState{conversation: session.AgentConversationData{
		Agent: tmux.ProgramClaude, ID: "occupants-conversation",
	}})

	// The tombstone applies again with no delete running — a released one whose
	// proof lapsed for a tick — so nothing retired any carry.
	manager.mu.Lock()
	manager.deletedRootRepos[repo.ID] = "a-deleted-claimant"
	manager.mu.Unlock()
	manager.ensureRootAgentsAndWait()

	got, held := manager.heldRootCarry(st, repo, nil)
	require.True(t, held, "a tombstoned tick must leave the carry to suppressRootAgent")
	require.Equal(t, "occupants-conversation", got.conversation.ID)
}

// holdCarryForTest holds carried on st the way a heal's reap does
// (installRootCarryLocked), for tests that start from a carry already held.
func holdCarryForTest(m *Manager, st *rootEnsureState, repo *config.RepoContext, identity *resolvedProjectRoot, carried reapedRootState) {
	compare := pathutil.ResolveForCompare(filepath.Clean(repo.WorkspacePath()))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.installRootCarryLocked(st, repo, identity, compare, carried)
}

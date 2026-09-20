package daemon

import (
	stdlog "log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #4400 review-round-7 suite. The reaped root's carry is keyed by
// repository but belongs to one checkout, and while a committed account swap
// is pending it owns the carried conversation. Each test here pins one of the
// places a create or a healthy pass used to act on a carry it could not speak
// for.

// setupBareRepoTwoWorktrees builds the one shape where two root_agents entries
// resolve to one repository ID and two workspaces: linked worktrees of a bare
// repository. An ordinary repository's linked worktrees all resolve to the main
// checkout, so they cannot tell the carry's workspaces apart. "another" sorts
// before "owner", so its ensure pass runs first on every tick.
func setupBareRepoTwoWorktrees(t *testing.T) (owner, another string) {
	t.Helper()
	parent := testguard.CanonicalTempDir(t)
	source := filepath.Join(parent, "source")
	bare := filepath.Join(parent, "bare.git")
	owner = filepath.Join(parent, "wt-owner")
	another = filepath.Join(parent, "wt-another")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run(parent, "init", source)
	run(source, "config", "user.email", "test@test.com")
	run(source, "config", "user.name", "Test")
	run(source, "commit", "--allow-empty", "-m", "init")
	run(parent, "clone", "--bare", source, bare)
	run(bare, "worktree", "add", owner)
	run(bare, "worktree", "add", another)
	return owner, another
}

// holdRootEnsure parks or releases one root_agents candidate's ensure pass, so
// a test decides which spelling of the repository acts on a tick.
func holdRootEnsure(m *Manager, key string, hold bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.rootEnsureStateForLocked(key)
	st.nextAttempt = time.Time{}
	if hold {
		st.nextAttempt = time.Now().Add(time.Hour)
	}
}

// requireParkedCarry reads the durable carry back and fails if there is none.
func requireParkedCarry(t *testing.T, m *Manager, repoID string) reapedRootState {
	t.Helper()
	parked, present, err := m.loadReapedRootCarry(repoID)
	require.NoError(t, err)
	require.True(t, present, "the carry file must still be on disk")
	return parked
}

// TestEnsureRootAgentsStartsAPendingSwapsUnwrittenConversationFresh is finding
// 1. A manual claude swap committed and started its panes but had not delivered
// the takeover brief, so its injected conversation has no transcript yet; the
// new account already holds an older conversation for this project. The
// recreate must not read "no transcript" as a rotation and resume that older
// conversation — the brief would land in it.
func TestEnsureRootAgentsStartsAPendingSwapsUnwrittenConversationFresh(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, warning := newManagerCapturingWarnings(t, rootTestConfig(repoPath, config.RootAgentConfig{}))
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first, "root instance missing after first ensure")
	require.Len(t, *seen, 1)

	injected := seedRootConversation(t, first)
	accountDir, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)
	first.ReconcileAccountHandoffSnapshot("work", false, &session.AccountSwapData{
		Manual: true, To: "work", ConversationID: injected.ID, ReplacementPanesStarted: true,
	})
	const olderConversationID = "0a1b2c3d-4444-4555-8666-777788889999"
	writeRootClaudeTranscript(t, accountDir, repoPath, olderConversationID)

	// The #1104 outage class: tmux vanished under a healthy daemon.
	first.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

	require.Len(t, *seen, 2, "the vanished root must be reaped and re-created once")
	recreate := (*seen)[1]
	assert.Equal(t, "work", recreate.Account, "the committed swap's account survives the heal")
	assert.NotEqual(t, olderConversationID, recreate.ResumeConversation.ID,
		"the recreate must not resume an older project conversation in place of the swap's unwritten one")
	assert.False(t, recreate.ResumeConversation.HasID(),
		"the swap's conversation has no transcript to resume, so the replacement starts clean")
	if assert.NotNil(t, recreate.PendingAccountSwap, "the committed swap rides into the replacement") {
		assert.Equal(t, "work", recreate.PendingAccountSwap.To)
		assert.Empty(t, recreate.PendingAccountSwap.ConversationID,
			"the swap's recorded id follows the fresh launch, so the settlement sync accepts the new pane")
	}
	assert.Contains(t, warning.String(), "belongs to a committed account swap")
	require.NotNil(t, findRootInstance(t, manager, repoPath), "always-ensure: the root must exist again")
}

// TestEnsureRootAgentsLeavesADeadSiblingRootsCarryParked is finding 2's reap
// half. Two enabled root_agents entries name linked worktrees of one bare
// repository, so they share the root's title slot. The owner's root dies while
// its own entry is backed off, and the sibling reaches the dead record first.
// The sibling may reap it — the repository needs its root back — but must not
// start the owner's account pin and pending swap in its own checkout, and what
// the record carried stays parked for the owner.
func TestEnsureRootAgentsLeavesADeadSiblingRootsCarryParked(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seen := installOptionsRecordingBackend(t)
	owner, another := setupBareRepoTwoWorktrees(t)
	repo, err := config.RepoFromPath(owner)
	require.NoError(t, err)

	cfg := config.DefaultConfig()
	cfg.RootAgents = map[string]config.RootAgentConfig{owner: {}, another: {}}
	manager, warning := newManagerCapturingWarnings(t, cfg)

	holdRootEnsure(manager, another, true)
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, owner)
	require.NotNil(t, first, "root instance missing after first ensure")
	require.Len(t, *seen, 1)
	require.Equal(t, owner, (*seen)[0].Path, "the owner's entry created the root")

	prior := seedRootConversation(t, first)
	_, err = agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)
	first.ReconcileAccountHandoffSnapshot("work", false, &session.AccountSwapData{
		Manual: true, To: "work", ConversationID: prior.ID, ReplacementPanesStarted: true,
	})

	holdRootEnsure(manager, another, false)
	holdRootEnsure(manager, owner, true)
	first.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

	require.Len(t, *seen, 2, "the sibling entry must still heal the repository's root")
	recreate := (*seen)[1]
	require.Equal(t, another, recreate.Path)
	assert.Empty(t, recreate.Account, "the owner's account pin must not start in the sibling's checkout")
	assert.Nil(t, recreate.PendingAccountSwap, "the owner's pending swap must not ride into the sibling's root")
	assert.False(t, recreate.ResumeConversation.HasID(), "the owner's conversation must not be resumed here")

	parked := requireParkedCarry(t, manager, repo.ID)
	assert.Equal(t, owner, parked.workspace)
	assert.Equal(t, "work", parked.account)
	assert.Equal(t, prior.ID, parked.conversation.ID)
	if assert.NotNil(t, parked.pendingSwap) {
		assert.Equal(t, "work", parked.pendingSwap.To)
	}
	assert.Contains(t, warning.String(), "stay parked for its own root_agents entry")
	require.NotNil(t, findRootInstance(t, manager, owner), "always-ensure: the repository has a root again")
}

// TestEnsureRootAgentsKeepsAnotherWorktreesParkedCarry is finding 2's retire
// half. The owner's carry is parked on disk — its replacement never published
// before a restart — and the sibling publishes the repository's root instead.
// Neither that publish nor the healthy passes that adopt the sibling's root
// may delete the owner's pin and pending swap, and the kept carry is warned
// about once rather than on every healthy tick.
func TestEnsureRootAgentsKeepsAnotherWorktreesParkedCarry(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seen := installOptionsRecordingBackend(t)
	owner, another := setupBareRepoTwoWorktrees(t)
	repo, err := config.RepoFromPath(owner)
	require.NoError(t, err)

	cfg := config.DefaultConfig()
	cfg.RootAgents = map[string]config.RootAgentConfig{owner: {}, another: {}}
	manager, warning := newManagerCapturingWarnings(t, cfg)
	require.NoError(t, manager.writeReapedRootCarry(repo.ID, reapedRootState{
		workspace:   owner,
		account:     "work",
		agent:       tmux.ProgramClaude,
		pendingSwap: &session.AccountSwapData{Manual: true, To: "work"},
	}))

	holdRootEnsure(manager, owner, true)
	manager.ensureRootAgentsAndWait()

	require.Len(t, *seen, 1)
	require.Equal(t, another, (*seen)[0].Path, "the sibling entry published the root")
	assert.Empty(t, (*seen)[0].Account, "the sibling does not consume the owner's carry")
	parked := requireParkedCarry(t, manager, repo.ID)
	assert.Equal(t, owner, parked.workspace, "the sibling's publish must not retire the owner's carry")
	assert.Equal(t, "work", parked.account)

	// Healthy passes from both spellings adopt the sibling's root; the carry is
	// the owner's, which only a root in the owner's checkout can consume.
	holdRootEnsure(manager, owner, false)
	manager.ensureRootAgentsAndWait()
	manager.ensureRootAgentsAndWait()
	require.Len(t, *seen, 1, "a healthy root is adopted, never re-created")
	parked = requireParkedCarry(t, manager, repo.ID)
	assert.Equal(t, owner, parked.workspace, "adopting the sibling's root must not retire the owner's carry")
	assert.Equal(t, 1, strings.Count(warning.String(), "leaving the root agent carry reaped in "+owner),
		"the parked carry is reported once, not on every healthy tick")
}

// TestEnsureRootAgentsNamesTheSiblingCarryAReapSupersedes covers the one way a
// kept carry still goes: the file holds one carry per repository, so when the
// sibling's own root dies and the sibling reaps it, the owner's parked carry is
// overwritten. That retirement has to be named before it happens.
func TestEnsureRootAgentsNamesTheSiblingCarryAReapSupersedes(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seen := installOptionsRecordingBackend(t)
	owner, another := setupBareRepoTwoWorktrees(t)
	repo, err := config.RepoFromPath(owner)
	require.NoError(t, err)

	cfg := config.DefaultConfig()
	cfg.RootAgents = map[string]config.RootAgentConfig{owner: {}, another: {}}
	manager, warning := newManagerCapturingWarnings(t, cfg)
	holdRootEnsure(manager, owner, true)
	manager.ensureRootAgentsAndWait()
	require.Len(t, *seen, 1)
	sibling := findRootInstance(t, manager, another)
	require.NotNil(t, sibling)
	require.Equal(t, another, (*seen)[0].Path)

	// The owner's carry lands on disk after the sibling's root is up, so the
	// reap below finds it however the healthy passes treat a kept carry.
	require.NoError(t, manager.writeReapedRootCarry(repo.ID, reapedRootState{
		workspace: owner, account: "work", agent: tmux.ProgramClaude,
	}))
	sibling.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

	require.Len(t, *seen, 2, "the sibling reaps and re-creates its own root")
	assert.Contains(t, warning.String(), "supersedes the root agent carry parked for "+owner)
	_, present, err := manager.loadReapedRootCarry(repo.ID)
	require.NoError(t, err)
	assert.False(t, present, "the sibling consumed its own carry and retired it")
}

// TestRetireReapedRootCarryDecisionTable pins retireReapedRootCarry and
// discardReapedRootCarry row by row: what a pass may retire, what it must keep,
// and what it says about each.
func TestRetireReapedRootCarryDecisionTable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const repoID = "deadbeefcafe03"
	path, err := reapedRootCarryPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	newManager := func() (*Manager, *logCapture) {
		capture := &logCapture{}
		return &Manager{
			warnLog:             stdlog.New(capture, "", 0),
			reapedRootCarries:   map[string]reapedRootState{},
			rootCreatesInFlight: map[string]string{},
		}, capture
	}
	onDisk := func() bool {
		_, err := os.Lstat(path)
		return err == nil
	}

	// Nothing parked: nothing to do, nothing to say.
	m, logs := newManager()
	m.retireReapedRootCarry(repoID, "/repos/a", false)
	assert.Empty(t, logs.String())

	// Bound to the healthy root's checkout: moot, retired from both halves.
	m, logs = newManager()
	require.NoError(t, m.writeReapedRootCarry(repoID, reapedRootState{workspace: "/repos/a", account: "work"}))
	m.retireReapedRootCarry(repoID, "/repos/a/", false)
	assert.False(t, onDisk(), "a carry bound to the healthy root's checkout is retired")
	assert.Empty(t, m.reapedRootCarries)
	assert.Empty(t, logs.String())

	// Written before binding existed: consumable anywhere, so moot anywhere.
	m, _ = newManager()
	require.NoError(t, m.writeReapedRootCarry(repoID, reapedRootState{account: "work"}))
	m.retireReapedRootCarry(repoID, "/repos/b", false)
	assert.False(t, onDisk(), "a pre-binding carry is retired as before")

	// Bound to another checkout: kept, hydrated, warned about once.
	m, logs = newManager()
	require.NoError(t, m.writeReapedRootCarry(repoID, reapedRootState{workspace: "/repos/a", account: "work"}))
	for range 3 {
		m.retireReapedRootCarry(repoID, "/repos/b", false)
	}
	assert.True(t, onDisk(), "another checkout's carry is not this pass's to retire")
	assert.Equal(t, "/repos/a", m.reapedRootCarries[repoID].workspace, "the kept carry is hydrated")
	assert.Equal(t, 1, strings.Count(logs.String(), "leaving the root agent carry reaped in /repos/a"))

	// consumed: the caller's own create restored it, whatever it is bound to.
	m.retireReapedRootCarry(repoID, "/repos/b", true)
	assert.False(t, onDisk(), "a consumed carry is retired")
	assert.Empty(t, m.reapedRootCarries)

	// Unreadable — a link or a directory at the path: binding unknown, kept,
	// warned about once per cause rather than once per healthy tick.
	const unreadable = "leaving the parked root agent carry at "
	m, logs = newManager()
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "elsewhere.json"), path))
	for range 3 {
		m.rootEnsureSucceeded(repoID, "/repos/a", &rootEnsureState{})
	}
	assert.True(t, onDisk(), "an unreadable carry is left for the operator")
	assert.Equal(t, 1, strings.Count(logs.String(), unreadable))
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.MkdirAll(filepath.Join(path, "entry"), 0o755))
	for range 3 {
		m.rootEnsureSucceeded(repoID, "/repos/a", &rootEnsureState{})
	}
	assert.True(t, onDisk())
	assert.Equal(t, 2, strings.Count(logs.String(), unreadable),
		"a new cause is reported once more")

	// Once the condition clears the notice re-arms: the same cause, back
	// again, is reported again.
	require.NoError(t, os.RemoveAll(path))
	m.rootEnsureSucceeded(repoID, "/repos/a", &rootEnsureState{})
	require.NoError(t, os.MkdirAll(filepath.Join(path, "entry"), 0o755))
	m.rootEnsureSucceeded(repoID, "/repos/a", &rootEnsureState{})
	assert.Equal(t, 3, strings.Count(logs.String(), unreadable))
	require.NoError(t, os.RemoveAll(path))

	// A create still in flight owns the carry: a healthy pass leaves it alone.
	m, _ = newManager()
	require.NoError(t, m.writeReapedRootCarry(repoID, reapedRootState{workspace: "/repos/a"}))
	m.rootCreatesInFlight[repoID] = "/repos/a"
	m.rootEnsureSucceeded(repoID, "/repos/a", &rootEnsureState{})
	assert.True(t, onDisk(), "the in-flight create retires its own carry")

	// discard retires whatever it is bound to, and names what it dropped.
	m, logs = newManager()
	m.discardReapedRootCarry(repoID, "its project was deleted")
	assert.False(t, onDisk())
	assert.Contains(t, logs.String(), "discarded the root agent carry reaped in /repos/a")
	assert.Contains(t, logs.String(), "its project was deleted")
	logs.Reset()
	m.discardReapedRootCarry(repoID, "its project was deleted")
	assert.Empty(t, logs.String(), "nothing parked, nothing discarded")
}

// TestSweepOrphanedRootCarriesRetiresOnlyWhatNoCandidateCanConsume is the
// round-8 finding: every other retirement is driven by a candidate the sweep
// still enumerates, so a repository whose last root_agents entry was DELETED
// keeps its carry forever — and re-adding the entry later resumes an obsolete
// account pin, conversation and pending swap. Each guard below fails toward
// KEEPING the carry, because a wrong retirement destroys exactly the pin this
// mechanism exists to preserve.
func TestSweepOrphanedRootCarriesRetiresOnlyWhatNoCandidateCanConsume(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const repoID = "deadbeefcafe04"

	newManager := func(rootAgents map[string]config.RootAgentConfig, snap rootAgentSnapshot) *Manager {
		m := &Manager{
			cfg:                 &config.Config{RootAgents: rootAgents},
			instances:           map[string]*session.Instance{},
			reapedRootCarries:   map[string]reapedRootState{},
			rootCreatesInFlight: map[string]string{},
		}
		m.rootAgentLayers.Store(&snap)
		require.NoError(t, m.writeReapedRootCarry(repoID, reapedRootState{workspace: "/repos/gone", account: "work"}))
		return m
	}
	onDisk := func(m *Manager) bool {
		_, present, err := m.loadReapedRootCarry(repoID)
		require.NoError(t, err)
		return present
	}
	enabled := rootAgentSnapshot{legacy: legacyRepoDedup{byPath: map[string]string{"/a": repoID}}}

	// An enabled candidate still owns it.
	m := newManager(map[string]config.RootAgentConfig{"/a": {}}, enabled)
	m.sweepOrphanedRootCarries(m.rootAgentLayers.Load())
	require.True(t, onDisk(m), "a carry its own entry can still consume must stay")

	// The entry is gone from config entirely: nothing enumerates this repo.
	m = newManager(map[string]config.RootAgentConfig{}, rootAgentSnapshot{})
	m.sweepOrphanedRootCarries(m.rootAgentLayers.Load())
	require.False(t, onDisk(m), "a carry no candidate can consume is retired")

	// An unanswered legacy probe may still resolve to this repository.
	m = newManager(map[string]config.RootAgentConfig{"/a": {}},
		rootAgentSnapshot{legacy: legacyRepoDedup{unknownPaths: map[string]bool{"/a": true}}})
	m.sweepOrphanedRootCarries(m.rootAgentLayers.Load())
	require.True(t, onDisk(m), "unknown is never absent — the sweep waits for the probe")

	// A create in flight owns the carry it is about to consume.
	m = newManager(map[string]config.RootAgentConfig{}, rootAgentSnapshot{})
	m.rootCreatesInFlight[repoID] = "/repos/gone"
	m.sweepOrphanedRootCarries(m.rootAgentLayers.Load())
	require.True(t, onDisk(m), "the in-flight create retires its own carry")

	// A live root means the repository is ensured whatever this tick's config says.
	m = newManager(map[string]config.RootAgentConfig{}, rootAgentSnapshot{})
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: session.RootSessionTitle, Path: t.TempDir(), Program: "claude",
	})
	require.NoError(t, err)
	m.instances[daemonInstanceKey(repoID, session.RootSessionTitle)] = inst
	m.sweepOrphanedRootCarries(m.rootAgentLayers.Load())
	require.True(t, onDisk(m), "a live root's carry is not orphaned")

	// The cadence holds: a second pass inside the interval does not re-read.
	m = newManager(map[string]config.RootAgentConfig{}, rootAgentSnapshot{})
	m.nextRootCarrySweep = time.Now().Add(time.Hour)
	m.sweepOrphanedRootCarries(m.rootAgentLayers.Load())
	require.True(t, onDisk(m), "a sweep that is not due does nothing")
}

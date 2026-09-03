package daemon

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The #3699 regression suite: a root agent whose tmux vanished leaving ANY
// marker-carrying process behind was permanently unrecoverable.
//
// What the daemon actually did on 2026-09-02, once per ensure tick, forever:
//
//	rootagent.go:427: root agent for … is gone (tmux vanished); attempting to reap and re-create it in place
//	close.go:332: tmux session af_0f8fc14c_root: captured live pid 1091038 marks generation
//	              8d6d4cf664efb073354d5f41b3e5f207 outside the vanished session
//	              af_0f8fc14c_root generation cohort; refusing worktree cleanup
//	rootagent.go:836: … failed to remove dead root record: … its teardown did not complete safely
//	rootagent.go:833: … failed 6 consecutive times … will keep retrying every 5m0s
//
// The survivor carried the vanished session's OWN generation — it was
// `claude daemon run`, which inherits AF_SESSION/AF_HOME/AF_SESSION_GEN from the
// pane and outlives it — and the strict blind sweep refused it anyway, because a
// vanished session reaches sweepVanishedSessionProcesses with no captured
// predecessor at all, so the generation cohort is EMPTY and markedOrphanProcesses
// refuses every generation-marked candidate it finds. That branch signals nothing
// by design (#3309), so the survivor never died, so every retry refused
// identically, so the record was never deleted, so the always-ensure loop could
// never re-create the title.

const (
	// The exact refusal wording the daemon logged, so a red run in CI is
	// recognizable as the reported outage rather than as a generic teardown error.
	vanishedRootTmuxName    = "af_0f8fc14c_root"
	vanishedRootGeneration  = "8d6d4cf664efb073354d5f41b3e5f207"
	vanishedRootSurvivorPID = 1091038
)

// markedSurvivorBackend models, at the daemon's teardown boundary, the one thing
// the local tmux backend does differently for the #3699 shape: a vanished
// session that left a marker-carrying process behind is refused while the caller
// asks for the STRICT empty-cohort guard, and reaped once the caller vouches for
// exclusive ownership of the session's lifecycle so the live marker scan may seed
// the cohort (session/tmux/cleanup.go's reapVanishedSessionProcessCohort).
//
// That layer's behavior is pinned against real tmux and real escaped processes in
// session/tmux/reap_generation_test.go — TestBlindVanishedSessionSweepDoesNotAdopt
// ReplacementGeneration for the strict refusal, TestTrustedBlindVanishedSession
// SweepReapsNewerGeneration for the trusted reap — so this double is a model of
// tested behavior, not an invention. What is untested until now, and what this
// file is about, is which of the two reapDeadRoot asks for.
type markedSurvivorBackend struct {
	readyFakeBackend

	mu sync.Mutex
	// survivorAlive is the escaped process carrying this session's own
	// AF_SESSION/AF_HOME/AF_SESSION_GEN. Only a trusted sweep can reap it; while
	// it lives, every teardown reports ErrPaneMayBeLive.
	survivorAlive bool
	// reapableWhenTrusted false models the residual unknown trust CANNOT settle —
	// an unreadable process table, a wedged tmux server. The record must still be
	// retained there (#1917), which is the boundary the fix must not trade away.
	reapableWhenTrusted bool
	// strictRefusals counts empty-cohort refusals, for the red run's message.
	strictRefusals int
}

func (b *markedSurvivorBackend) Kill(instance *session.Instance, trustLiveGeneration bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.survivorAlive {
		return b.readyFakeBackend.Kill(instance, trustLiveGeneration)
	}
	if !trustLiveGeneration {
		b.strictRefusals++
		return b.refusal()
	}
	if !b.reapableWhenTrusted {
		return b.refusal()
	}
	b.survivorAlive = false
	return b.readyFakeBackend.Kill(instance, trustLiveGeneration)
}

func (b *markedSurvivorBackend) refusal() error {
	return fmt.Errorf("captured live pid %d marks generation %s outside the vanished session %s generation cohort: %w",
		vanishedRootSurvivorPID, vanishedRootGeneration, vanishedRootTmuxName, session.ErrPaneMayBeLive)
}

func (b *markedSurvivorBackend) survivorIsAlive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.survivorAlive
}

func (b *markedSurvivorBackend) strictRefusalCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.strictRefusals
}

// installMarkedSurvivorRootBackend gives only the FIRST session created in the
// test a marked survivor: the replacement root must come up clean, or a green
// result could not distinguish "the wedge cleared" from "nothing was ever wedged".
func installMarkedSurvivorRootBackend(t *testing.T, reapableWhenTrusted bool) (*[]session.InstanceOptions, *[]*markedSurvivorBackend) {
	t.Helper()
	var (
		seen     []session.InstanceOptions
		backends []*markedSurvivorBackend
	)
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		fake := session.NewFakeBackend()
		fake.CompleteStart()
		backend := &markedSurvivorBackend{
			readyFakeBackend:    readyFakeBackend{fake},
			survivorAlive:       len(backends) == 0,
			reapableWhenTrusted: reapableWhenTrusted,
		}
		seen = append(seen, opts)
		backends = append(backends, backend)
		return backend, nil
	})
	t.Cleanup(restore)
	return &seen, &backends
}

// requireOnlyRootRecordIs asserts the repo's durable rows hold exactly one root,
// and that it is the given instance. Identity, not title: the heal re-creates the
// SAME title, so "a root row exists" cannot tell a completed reap from a refused
// one — only the stable id can.
func requireOnlyRootRecordIs(t *testing.T, repoID string, inst *session.Instance) {
	t.Helper()
	data, err := loadRepoInstanceData(repoID)
	require.NoError(t, err)
	var roots []string
	for _, d := range data {
		if d.Title == session.RootSessionTitle {
			roots = append(roots, d.ID)
		}
	}
	require.Equal(t, []string{inst.ID}, roots,
		"the durable root row must be the replacement's, with the reaped one gone")
}

// TestEnsureRootAgentsHealsARootWhoseVanishedSessionLeftAMarkedSurvivor is the
// #3699 regression test. It drives the real production path — the poll loop's
// EnsureRootAgents, which is what actually ran 6 times and gave up at the 5m cap —
// and asserts the property the outage lacked: convergence. The root comes back.
//
// It fails on the unfixed code at the first assertion below, because reapDeadRoot
// called the strict inst.Kill(): the survivor is refused, deleteSessionRecord
// refuses to drop the record over an unsettled teardown, ensureResolvedRoot
// returns early on that error, and no second create is ever attempted.
func TestEnsureRootAgentsHealsARootWhoseVanishedSessionLeftAMarkedSurvivor(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen, backends := installMarkedSurvivorRootBackend(t, true)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.EnsureRootAgents()
	require.Len(t, *seen, 1, "precondition: the first ensure must create the root")

	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first, "precondition: the root instance must exist after the first ensure")
	require.Len(t, *backends, 1, "precondition: exactly one backend was handed out")
	wedged := (*backends)[0]
	require.True(t, wedged.survivorIsAlive(),
		"precondition: the root's teardown needs a marked survivor to refuse over")

	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	// The #1104 outage class with the #3699 twist: tmux vanished under a healthy
	// daemon, and a process carrying that session's own markers outlived it.
	first.SetStatusForTest(session.Lost)

	manager.EnsureRootAgents()

	require.Len(t, *seen, 2,
		"the vanished root was never reaped, so the always-ensure loop could not re-create it "+
			"(%d empty-cohort refusal(s)); this is the #3699 wedge: every retry refuses identically",
		wedged.strictRefusalCount())
	second := findRootInstance(t, manager, repoPath)
	require.NotNil(t, second, "always-ensure: the root must exist again after the heal")
	require.NotSame(t, first, second, "the heal must replace the wedged instance, not adopt it")
	require.False(t, wedged.survivorIsAlive(),
		"the reap must actually clear the marked survivor, not delete the record around it "+
			"and leave the escaped process running")
	requireOnlyRootRecordIs(t, repo.ID, second)
}

// TestReapDeadRootRetainsTheRecordWhenTrustCannotSettleTheTeardown pins the
// boundary the #3699 fix must not cross. Trusting the live generation removes ONE
// refusal — the empty generation cohort, which exclusive ownership of the title
// makes unnecessary — and nothing else. A teardown that is still unsettled after
// that (an unreadable process table, a wedged tmux server) keeps its record,
// because the record is the only handle left on a workspace that may still be
// live (#1917).
//
// This one passes both before and after the fix, on purpose: it exists so a
// later "just delete the record" shortcut cannot pass the suite.
func TestReapDeadRootRetainsTheRecordWhenTrustCannotSettleTheTeardown(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen, backends := installMarkedSurvivorRootBackend(t, false)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.EnsureRootAgents()
	require.Len(t, *seen, 1, "precondition: the first ensure must create the root")

	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first)
	first.SetStatusForTest(session.Lost)

	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)

	_, reaped, err := manager.reapDeadRoot(repo.ID, first)
	require.Error(t, err, "an unsettled teardown must be reported, not swallowed")
	require.ErrorContains(t, err, "its teardown did not complete safely")
	require.False(t, reaped)
	require.True(t, (*backends)[0].survivorIsAlive(),
		"nothing settled the teardown, so the survivor must still be there")
	requireSessionRecordRetained(t, manager, repo.ID, key, first)
}

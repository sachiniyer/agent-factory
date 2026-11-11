package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #6769 loop suite. Before it, an armed capture asked once and stopped: a
// Codex rollout minted after that one window — the pane's first prompt, a
// daemon-restart gap — stayed invisible to every later restore. The loop
// re-asks while the row stays live and unrecorded, and the armed marker is
// keyed by runtime generation so a replacement's arm can never be refused by a
// stale loop winding down.

// codexLoopTestInstance stands up a tracked, running codex session with no
// recorded conversation — the exact input the deferred arming and the re-ask
// loop exist for. It mirrors captureArchiveTestInstance minus the seeded
// rollout.
func codexLoopTestInstance(t *testing.T) (*Manager, *session.Instance, string, string, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	// Join the capture workers before the timing-global cleanup runs: a loop
	// still winding down reads conversationCaptureRearmInterval when it builds
	// its next timer, and a surviving worker can also leak into the next test
	// after its temporary store is gone (#6769 review). Registered after
	// shrinkConversationCaptureForTest's cleanup so it runs first.
	t.Cleanup(manager.stopAndWaitBackgroundMutationsForShutdown)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "codex-worker",
		Path:    repoPath,
		Program: tmux.ProgramCodex,
	})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSession("codex-worker", tmux.ProgramCodex))
	// Attribution needs a runtime holding the rollout open; the fixture's
	// pane is a name-only handle, so the test hands the walk its store.
	inst.SetCodexRuntimeHeldOverrideForTest(heldAllCodexRollouts(codexHome))
	// A real worktree: the deferred baseline resolves the launch directory
	// through GetWorktreePath, which stays empty without one.
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, repoPath, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	inst.SetStatusForTest(session.Running)
	key := daemonInstanceKey(repo.ID, inst.Title)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()
	require.NoError(t, appendInstanceData(repo.ID, inst.ToInstanceData()))
	return manager, inst, repo.ID, key, codexHome
}

// shrinkConversationCaptureForTest shortens the loop's two clocks so a re-ask
// cycle runs inside an Eventually window rather than tens of seconds.
func shrinkConversationCaptureForTest(t *testing.T) {
	t.Helper()
	oldTimeout := conversationCaptureTimeout
	oldRearm := conversationCaptureRearmInterval
	conversationCaptureTimeout = 100 * time.Millisecond
	conversationCaptureRearmInterval = 50 * time.Millisecond
	t.Cleanup(func() {
		conversationCaptureTimeout = oldTimeout
		conversationCaptureRearmInterval = oldRearm
	})
}

func armedConversationToken(t *testing.T, manager *Manager, inst *session.Instance) (session.AgentRuntimeToken, bool) {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	token, ok := manager.armedConversationCaptures[inst]
	return token, ok
}

func pendingConversationCaptureCount(manager *Manager, inst *session.Instance) int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.pendingConversationCaptures[inst]
}

// TestConversationCaptureLoopRecordsARolloutMintedAfterTheFirstWindow is the
// property-1 headline: the first capture attempt can legitimately find
// nothing — the pane has not prompted yet — and the recorded id must still
// land when the rollout finally appears, without an operator or a daemon
// restart intervening.
func TestConversationCaptureLoopRecordsARolloutMintedAfterTheFirstWindow(t *testing.T) {
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)

	snap := session.BeginConversationCapture()
	manager.captureAgentConversationAsync(repoID, key, inst, snap)

	// The first bounded attempt finds an empty store and stands down between
	// attempts — pending must reach zero or the fence semantics regressed to
	// "armed means in-flight".
	require.Eventually(t, func() bool {
		return pendingConversationCaptureCount(manager, inst) == 0
	}, 2*time.Second, 10*time.Millisecond,
		"the re-arm loop must drop the reap fence between attempts")

	writeDaemonCodexRolloutFile(t, codexHome,
		"rollout-2026-08-10T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")

	require.Eventually(t, func() bool {
		return inst.AgentConversation().ID == "019f386f-7206-7fc2-803b-f7045e07a242"
	}, 2*time.Second, 10*time.Millisecond,
		"a rollout minted after the first capture window must still be recorded")
	// The in-memory commit lands before the locked persist it precedes, so the
	// disk check gets its own Eventually rather than racing the write.
	require.Eventually(t, func() bool {
		return persistedConversationID(t, repoID) == "019f386f-7206-7fc2-803b-f7045e07a242"
	}, 2*time.Second, 10*time.Millisecond,
		"the recorded id must reach the repo's instance data")

	// Recording no longer ends the loop (#7101): it keeps the generation's
	// marker for the follow check, and between checks it still drops the
	// reap fence, so a recorded row is never pinned busy.
	token, armed := armedConversationToken(t, manager, inst)
	require.True(t, armed, "a recorded row keeps its loop for the follow check")
	require.Equal(t, inst.AgentRuntimeToken(), token)
	require.Eventually(t, func() bool {
		return pendingConversationCaptureCount(manager, inst) == 0
	}, 2*time.Second, 10*time.Millisecond,
		"the follow loop must drop the reap fence between checks")
}

// TestConversationCaptureLoopExitsWhenTheRowDies: a dead or archived pane
// mints no rollouts; the loop must leave so pending and the armed marker drain
// instead of pinning the row's reap fence forever.
func TestConversationCaptureLoopExitsWhenTheRowDies(t *testing.T) {
	for name, status := range map[string]session.Status{"dead": session.Dead, "archived": session.Archived} {
		t.Run(name, func(t *testing.T) {
			shrinkConversationCaptureForTest(t)
			manager, inst, repoID, key, _ := codexLoopTestInstance(t)

			manager.captureAgentConversationAsync(repoID, key, inst, session.BeginConversationCapture())
			inst.SetStatusForTest(status)

			require.Eventually(t, func() bool {
				_, armed := armedConversationToken(t, manager, inst)
				return !armed && pendingConversationCaptureCount(manager, inst) == 0
			}, 2*time.Second, 10*time.Millisecond,
				"a row that can no longer mint a rollout must lose its capture loop")
		})
	}
}

// TestConversationCaptureLoopExitsWhenItsMarkerIsDisplaced is the race the
// generation-keyed marker exists for: a newer runtime's arm replaces the
// armed token, the displaced goroutine must notice and leave — and its disarm
// must NOT clear the marker it no longer owns.
func TestConversationCaptureLoopExitsWhenItsMarkerIsDisplaced(t *testing.T) {
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, key, _ := codexLoopTestInstance(t)

	manager.captureAgentConversationAsync(repoID, key, inst, session.BeginConversationCapture())
	token, armed := armedConversationToken(t, manager, inst)
	require.True(t, armed, "the create-path arm must register before this test steals its marker")

	// Stand in for a newer generation's arm: whatever the marker becomes, it
	// is no longer the loop's token. The displaced goroutine's next qualifier
	// check must end it.
	manager.mu.Lock()
	manager.armedConversationCaptures[inst] = session.AgentRuntimeToken{}
	manager.mu.Unlock()

	require.Eventually(t, func() bool {
		return pendingConversationCaptureCount(manager, inst) == 0
	}, 2*time.Second, 10*time.Millisecond,
		"a displaced loop must stop polling")

	// The marker still names the replacement: only the owning goroutine clears
	// its entry, and this loop's token is not what is recorded.
	recorded, ok := armedConversationToken(t, manager, inst)
	require.True(t, ok, "a displaced loop must not clear the newer arm's marker")
	require.NotEqual(t, token, recorded)
}

// TestArmConversationCapturesArmsExactlyTheRowsThatNeedIt is the deferred-half
// contract: post-restart and capture-predating rows get an arm, and so does a
// row that already recorded — for the follow check (#7101); rows that died or
// run a different provider are left alone.
func TestArmConversationCapturesArmsExactlyTheRowsThatNeedIt(t *testing.T) {
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, _, codexHome := codexLoopTestInstance(t)

	recorded, err := session.NewInstance(session.InstanceOptions{
		Title: "codex-done", Path: inst.Path, Program: tmux.ProgramCodex,
	})
	require.NoError(t, err)
	recorded.SetTmuxSession(tmux.NewTmuxSession("codex-done", tmux.ProgramCodex))
	// The follow loop asks the runtime what it holds; the fixture's pane is a
	// name-only handle, so hand it the (empty) store like the primary row.
	recorded.SetCodexRuntimeHeldOverrideForTest(heldAllCodexRollouts(codexHome))
	recordedWorktree, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, recorded.Title, "main", "", false, true)
	require.NoError(t, err)
	recorded.SetGitWorktreeForTest(recordedWorktree)
	recorded.SetStatusForTest(session.Running)
	recorded.SetAgentConversation(session.AgentConversationData{
		Agent: tmux.ProgramCodex, ID: "5299e00d-1111-4222-8333-f7045e07a242",
	})

	lost, err := session.NewInstance(session.InstanceOptions{
		Title: "codex-lost", Path: inst.Path, Program: tmux.ProgramCodex,
	})
	require.NoError(t, err)
	lost.SetTmuxSession(tmux.NewTmuxSession("codex-lost", tmux.ProgramCodex))
	lost.SetStatusForTest(session.Lost)

	claude, err := session.NewInstance(session.InstanceOptions{
		Title: "claude-worker", Path: inst.Path, Program: tmux.ProgramClaude,
	})
	require.NoError(t, err)
	claude.SetTmuxSession(tmux.NewTmuxSession("claude-worker", tmux.ProgramClaude))
	claude.SetStatusForTest(session.Running)

	manager.mu.Lock()
	for _, other := range []*session.Instance{recorded, lost, claude} {
		manager.instances[daemonInstanceKey(repoID, other.Title)] = other
	}
	manager.mu.Unlock()

	manager.ArmConversationCaptures()

	token, armed := armedConversationToken(t, manager, inst)
	require.True(t, armed, "a live unrecorded codex row must gain a capture loop")
	require.Equal(t, inst.AgentRuntimeToken(), token)
	token, armed = armedConversationToken(t, manager, recorded)
	require.True(t, armed, "a live recorded codex row must gain a follow loop")
	require.Equal(t, recorded.AgentRuntimeToken(), token)
	for name, other := range map[string]*session.Instance{
		"lost": lost, "claude": claude,
	} {
		_, armed := armedConversationToken(t, manager, other)
		require.False(t, armed, "a %s row must not be armed", name)
	}
}

// TestArmConversationCapturesReArmsOverAStaleMarker: a marker the running
// generation does not own — left by a swap's displaced loop — must not refuse
// the live row's next arm, or the rollout the new runtime mints falls between
// baselines forever.
func TestArmConversationCapturesReArmsOverAStaleMarker(t *testing.T) {
	shrinkConversationCaptureForTest(t)
	manager, inst, _, _, _ := codexLoopTestInstance(t)

	manager.mu.Lock()
	if manager.armedConversationCaptures == nil {
		manager.armedConversationCaptures = make(map[*session.Instance]session.AgentRuntimeToken)
	}
	manager.armedConversationCaptures[inst] = session.AgentRuntimeToken{}
	manager.mu.Unlock()

	manager.ArmConversationCaptures()

	token, armed := armedConversationToken(t, manager, inst)
	require.True(t, armed, "a stale marker must not refuse the live generation's arm")
	require.Equal(t, inst.AgentRuntimeToken(), token)
}

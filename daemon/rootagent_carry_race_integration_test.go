package daemon

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// End-to-end embodiment of the delete/create carry race (#7090 review, P3-2):
// the root create runs on its own goroutine (#3721), so a create that launched
// before DeleteProject and reaches its failure path AFTER suppressRootAgent ran
// must not re-hold the carry that suppression just retired. Once the deletion
// tombstone later releases (a different clone proven at the same path), the
// next ensure create must come up BLANK — not resuming the deleted project's
// conversation and tabs.
//
// This drives the real ensure loop (EnsureRootAgents + the root-create
// goroutine) against the in-process fake tmux backend, using testHookRootAfterReap
// to park the heal create at the post-reap point where the bug's failure path
// runs, and DeleteProject as the concurrent delete.
func TestRootHealCreateFailingAfterReapDoesNotReHoldCarryWhenDeleteRaces(t *testing.T) {
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

	// Park the heal create at the post-reap point and make the post-reap create
	// fail (git stops answering the worktree listing, exactly like the #7088
	// fixture). A channel gates the hook so the test can run DeleteProject
	// while the create goroutine is parked between reap and CreateSession.
	var stalled atomic.Bool
	reapReached := make(chan struct{})
	releaseCreate := make(chan struct{})
	prevBindings, prevHook := worktreeBranchBindings, testHookRootAfterReap
	worktreeBranchBindings = func(repo string) ([]git.WorktreeBranchBinding, error) {
		if stalled.Load() {
			return nil, fmt.Errorf("git worktree list: %w", context.DeadlineExceeded)
		}
		return prevBindings(repo)
	}
	testHookRootAfterReap = func() {
		stalled.Store(true)
		close(reapReached)
		<-releaseCreate
	}
	t.Cleanup(func() { worktreeBranchBindings, testHookRootAfterReap = prevBindings, prevHook })

	// Vanish the root's tmux so the next ensure pass heals (reap + recreate).
	first.SetStatusForTest(session.Lost)

	// Launch the heal create. EnsureRootAgents returns once the create goroutine
	// is launched (it does not wait for it); the create goroutine reaps the dead
	// root and then parks at the hook.
	manager.EnsureRootAgents()

	// Wait for the create goroutine to reach the post-reap park point — i.e. the
	// dead root's record is already reaped and gone, the exact window the bug
	// describes (the create is invisible to DeleteProject's phase-1 gate).
	select {
	case <-reapReached:
	case <-time.After(10 * time.Second):
		t.Fatalf("heal create never reached the post-reap hook")
	}
	// Sanity: the dead root's record is gone, so DeleteProject's phase-1 gate
	// sees nothing in flight for the repo and proceeds (the structural
	// precondition for the race).
	require.Nil(t, findRootInstance(t, manager, repoPath),
		"the reaped root's record must be gone before the delete races")

	// Run the delete concurrently with the parked create. DeleteProject's
	// phase-1 gate sees no sessions (the reap deleted the only row), proceeds to
	// suppressRootAgent (tombstone + retire carries) and deregister.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := manager.DeleteProject(DeleteProjectRequest{RepoID: config.RepoIDFromRoot(repoPath), RepoPath: repoPath})
		require.NoError(t, err, "DeleteProject must succeed: no in-flight session after the reap")
	}()

	// Give the delete a moment to install the projectDeletes fence and run
	// suppressRootAgent (retiring any held carry + stamping the tombstone).
	time.Sleep(200 * time.Millisecond)

	// Release the parked create. Its post-reap create now runs; the worktree
	// listing is still stalled, so it fails and reaches the failure path that
	// calls holdRootCarry. The fix makes holdRootCarry refuse while the
	// tombstone applies / the delete fence is up.
	close(releaseCreate)
	manager.waitRootAgentCreates()
	wg.Wait()

	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	st := manager.rootEnsureStates[repoPath]
	require.NotNil(t, st, "the candidate's ensure state exists")
	_, reHeld := manager.heldRootCarry(st, repo, nil)
	require.False(t, reHeld,
		"a create whose failure path runs after suppressRootAgent must NOT re-hold "+
			"the retired carry — otherwise the deleted project's conversation and tabs "+
			"leak into an unrelated clone re-registered at the same path once the tombstone releases")

	// The carried conversation was never resumed: no further create happened
	// after the failure, and the recorded conversation id is not in any later
	// create request. (Only the first-ever create and the failed heal ran.)
	for _, opts := range *seen {
		require.NotEqual(t, prior.ID, opts.ResumeConversation.ID,
			"the deleted project's carried conversation must not be resumed by any create")
	}
}

// Control for the race test above: with NO concurrent delete, the same
// post-reap-failed heal MUST still hold its carry so the next pass resumes the
// conversation — the #7088 behavior the carry exists for. This is the existing
// TestRootHealKeepsItsCarriedStateWhenItsCreateFailsOnATransientAfterTheReap,
// restated here against the same hook plumbing to keep the two halves of the
// race in one place.
func TestRootHealCreateFailingAfterReapHoldsCarryWhenNoDeleteRaces(t *testing.T) {
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
	require.Nil(t, findRootInstance(t, manager, repoPath), "the heal's create failed after the reap")

	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	st := manager.rootEnsureStates[repoPath]
	require.NotNil(t, st)
	held, ok := manager.heldRootCarry(st, repo, nil)
	require.True(t, ok, "with no delete racing, the failed heal holds its carry for the next pass")
	require.Equal(t, prior.ID, held.conversation.ID, "the held carry is the reaped root's conversation")

	// The next pass, with git answering again, resumes the carried conversation.
	stalled.Store(false)
	resetRootEnsureBackoff(manager)
	manager.ensureRootAgentsAndWait()
	healed := findRootInstance(t, manager, repoPath)
	require.NotNil(t, healed, "the next pass re-creates the root")
	last := (*seen)[len(*seen)-1]
	require.Equal(t, prior.ID, last.ResumeConversation.ID,
		"the replacement resumes the conversation the reaped root carried, not a blank one")
}

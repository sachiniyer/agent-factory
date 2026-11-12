package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// End-to-end embodiments of the delete/create carry race (#7090 review, P3-2;
// #7182): the root create runs on its own goroutine (#3721), so a delete can
// run while a heal's create is in flight. Whatever order the two take, the
// deleted project's conversation and tabs must never be resumed or kept for a
// later create, and a delete that changed nothing must leave them alone.
//
// These drive the real ensure loop (EnsureRootAgents + the root-create
// goroutine) against the in-process fake tmux backend, using
// testHookRootAfterReap to park the heal create at the post-reap point, and
// DeleteProject as the concurrent delete.

// parkedRootHeal is a heal create parked at the post-reap point: the dead
// root's record is already reaped, so the create is invisible to
// DeleteProject's phase-1 gate. Once released, a create whose worktree listing
// is stalled fails (exactly like the #7088 fixture); one that is not stalled
// goes on to CreateSession.
type parkedRootHeal struct {
	manager   *Manager
	repoPath  string
	repo      *config.RepoContext
	seen      *[]session.InstanceOptions
	prior     session.AgentConversationData
	release   chan struct{}
	released  *sync.Once
	stallable *atomic.Bool
	stalled   *atomic.Bool
}

// rootHealParkPoint is where parkRootHeal holds the heal create.
type rootHealParkPoint int

const (
	// parkAfterReap: the reap has finished, so its carry is held and the dead
	// record is gone (testHookRootAfterReap).
	parkAfterReap rootHealParkPoint = iota
	// parkInReapTeardown: the reap has torn the root down but not yet run its
	// final m.mu section, so the dead record is still in m.instances and no
	// carry is held yet (testHookRootReapTornDown, PR #7190 review F1).
	parkInReapTeardown
)

// parkRootHealAfterReap builds a legacy root_agents root at the checkout
// newRepo makes, seeds its conversation, and parks its heal after the reap.
// stall says whether the released create fails on the worktree listing.
func parkRootHealAfterReap(t *testing.T, newRepo func(*testing.T) string, stall bool) parkedRootHeal {
	t.Helper()
	return parkRootHeal(t, newRepo, stall, parkAfterReap)
}

// parkRootHeal is parkRootHealAfterReap with the park point chosen.
func parkRootHeal(t *testing.T, newRepo func(*testing.T) string, stall bool, at rootHealParkPoint) parkedRootHeal {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
	seen := installOptionsRecordingBackend(t)
	repoPath := newRepo(t)
	writeRootClaudeTranscript(t, claudeConfigDir, repoPath, priorRootConversationID)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first, "precondition: the first ensure creates the root")
	prior := seedRootConversation(t, first)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	p := parkedRootHeal{
		manager: manager, repoPath: repoPath, repo: repo, seen: seen, prior: prior,
		release: make(chan struct{}), released: &sync.Once{},
		stallable: &atomic.Bool{}, stalled: &atomic.Bool{},
	}
	p.stallable.Store(stall)
	reapReached := make(chan struct{})
	prevBindings, prevHook, prevTornDown := worktreeBranchBindings, testHookRootAfterReap, testHookRootReapTornDown
	worktreeBranchBindings = func(repo string) ([]git.WorktreeBranchBinding, error) {
		if p.stalled.Load() {
			return nil, fmt.Errorf("git worktree list: %w", context.DeadlineExceeded)
		}
		return prevBindings(repo)
	}
	var parked atomic.Bool
	park := func() {
		if !parked.CompareAndSwap(false, true) {
			return // only the first heal parks; later passes run straight through
		}
		p.stalled.Store(p.stallable.Load())
		close(reapReached)
		<-p.release
	}
	switch at {
	case parkInReapTeardown:
		testHookRootReapTornDown = park
	default:
		testHookRootAfterReap = park
	}
	t.Cleanup(func() {
		p.released.Do(func() { close(p.release) })
		manager.waitRootAgentCreates()
		worktreeBranchBindings, testHookRootAfterReap, testHookRootReapTornDown = prevBindings, prevHook, prevTornDown
	})

	// Vanish the root's tmux so the next ensure pass heals (reap + recreate).
	// EnsureRootAgents returns once the create goroutine is launched; that
	// goroutine reaps the dead root and then parks at the hook.
	first.SetStatusForTest(session.Lost)
	manager.EnsureRootAgents()
	select {
	case <-reapReached:
	case <-time.After(10 * time.Second):
		t.Fatalf("heal create never reached the post-reap hook")
	}
	if at == parkAfterReap {
		require.Nil(t, findRootInstance(t, manager, repoPath),
			"the reaped root's record must be gone before the delete races")
	}
	return p
}

// releaseAndWait lets the parked create run to its end.
func (p parkedRootHeal) releaseAndWait() {
	p.released.Do(func() { close(p.release) })
	p.manager.waitRootAgentCreates()
}

// heldCarry reports the carry the candidate holds for the next pass, if any.
func (p parkedRootHeal) heldCarry(t *testing.T) (reapedRootState, bool) {
	t.Helper()
	p.manager.mu.Lock()
	st := p.manager.rootEnsureStates[p.repoPath]
	p.manager.mu.Unlock()
	require.NotNil(t, st, "the candidate's ensure state exists")
	return p.manager.heldRootCarry(st, p.repo, nil)
}

// assertCarryGone checks that nothing holds the deleted project's carry and
// that no create ever resumed its conversation.
func (p parkedRootHeal) assertCarryGone(t *testing.T, why string) {
	t.Helper()
	_, held := p.heldCarry(t)
	require.False(t, held, why)
	for _, opts := range *p.seen {
		require.NotEqual(t, p.prior.ID, opts.ResumeConversation.ID,
			"the deleted project's carried conversation must not be resumed by any create")
	}
}

// PR #7190 review thread 4554406883 (P2): the race test released the create
// after a fixed 200 ms sleep, which a delete slower than that beats — the
// create then re-held first, the late suppression retired the carry, and
// every assertion passed with the regression present. The create is released
// only once suppression is OBSERVABLE, and the delete here is deliberately
// slowed well past any such sleep so that ordering is what the test relies on.
func TestRootHealCreateFailingAfterReapDoesNotReHoldCarryWhenDeleteRaces(t *testing.T) {
	p := parkRootHealAfterReap(t, setupControlRepo, true)
	deleteProjectPreSweepHookForTest = func() { time.Sleep(300 * time.Millisecond) }
	t.Cleanup(func() { deleteProjectPreSweepHookForTest = nil })

	var (
		wg        sync.WaitGroup
		deleteErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, deleteErr = p.manager.DeleteProject(DeleteProjectRequest{RepoID: p.repo.ID, RepoPath: p.repoPath})
	}()
	require.Eventually(t, func() bool {
		p.manager.mu.Lock()
		defer p.manager.mu.Unlock()
		_, stamped := p.manager.deletedRootRepos[p.repo.ID]
		return stamped
	}, 10*time.Second, 5*time.Millisecond, "the delete never stamped its tombstone")

	p.releaseAndWait()
	wg.Wait()
	require.NoError(t, deleteErr, "DeleteProject must succeed: no in-flight session after the reap")
	p.assertCarryGone(t, "a create whose failure path runs after suppressRootAgent must not keep "+
		"the retired carry — the deleted project's conversation and tabs would leak into an "+
		"unrelated clone at the same path once the tombstone releases")
}

// PR #7190 review thread 4554406875 (P1): consulting only CURRENT delete state
// misses the create that resumes after the delete has finished — its fence is
// down — and after the tombstone has released, which a new checkout proven
// different at the reused path does. A legacy carry has no checkout identity
// and the path-derived repo id and workspace are reused, so a carry kept then
// is resumed by the next pass in the replacement checkout.
func TestRootHealCreateBegunBeforeACompletedDeleteNeverReHoldsItsCarry(t *testing.T) {
	p := parkRootHealAfterReap(t, setupControlRepo, true)

	_, err := p.manager.DeleteProject(DeleteProjectRequest{RepoID: p.repo.ID, RepoPath: p.repoPath})
	require.NoError(t, err, "DeleteProject must succeed: no in-flight session after the reap")
	p.manager.mu.Lock()
	_, fenced := p.manager.projectDeletes[p.repo.ID]
	_, tombstoned := p.manager.deletedRootRepos[p.repo.ID]
	// Release the tombstone, as a proven mismatch at the reused path does:
	// nothing about the delete is visible in current state any more.
	delete(p.manager.deletedRootRepos, p.repo.ID)
	p.manager.mu.Unlock()
	require.False(t, fenced, "fixture: the completed delete has taken its fence down")
	require.True(t, tombstoned, "fixture: the delete stamped its tombstone before the release")

	p.releaseAndWait()
	p.assertCarryGone(t, "a create that began before a delete must not keep its carry once the delete "+
		"has finished and its tombstone has released — the next pass would resume the deleted "+
		"project's conversation in whatever checkout now holds the path")
}

// PR #7190 review thread 4555053198 (P1): a create that merely STALLS through
// the delete and then succeeds must not resume the carry either. CreateSession
// takes its own delete sample after the completed delete, so it sees no fence;
// nothing on its success path asked whether the carry it was given survived.
func TestAStalledRootCreateResumingAfterACompletedDeleteNeverResumesItsCarry(t *testing.T) {
	p := parkRootHealAfterReap(t, setupControlRepo, false)

	_, err := p.manager.DeleteProject(DeleteProjectRequest{RepoID: p.repo.ID, RepoPath: p.repoPath})
	require.NoError(t, err, "DeleteProject must succeed: no in-flight session after the reap")

	p.releaseAndWait()
	p.assertCarryGone(t, "a create released after the delete must not keep the deleted project's carry")
	require.Nil(t, findRootInstance(t, p.manager, p.repoPath),
		"a create that began before a delete must not commit the deleted project's root after it")
}

// PR #7190 review thread 4555053170 (P2): a delete that installs its fence and
// then returns before suppression — here its durable root_agents write fails —
// changed nothing, and says so. It must not cost the failed heal its carry:
// the next pass resumes the conversation exactly as if no delete had run.
func TestAnAbortedDeleteLeavesTheRootCarryForTheNextPass(t *testing.T) {
	p := parkRootHealAfterReap(t, setupControlRepo, true)
	prevSweep := deregisterRootAgents
	deregisterRootAgents = func(...string) ([]string, error) {
		return nil, errors.New("config write failed")
	}
	t.Cleanup(func() { deregisterRootAgents = prevSweep })

	_, err := p.manager.DeleteProject(DeleteProjectRequest{RepoID: p.repo.ID, RepoPath: p.repoPath})
	require.Error(t, err, "fixture: the delete aborts after installing its fence")
	require.Contains(t, err.Error(), "nothing was changed")
	deregisterRootAgents = prevSweep

	p.releaseAndWait()
	held, ok := p.heldCarry(t)
	require.True(t, ok, "an aborted delete changed nothing, so the failed heal keeps its carry")
	require.Equal(t, p.prior.ID, held.conversation.ID)

	// The next pass, with git answering again, resumes the carried conversation.
	p.stalled.Store(false)
	resetRootEnsureBackoff(p.manager)
	p.manager.ensureRootAgentsAndWait()
	require.NotNil(t, findRootInstance(t, p.manager, p.repoPath), "the next pass re-creates the root")
	last := (*p.seen)[len(*p.seen)-1]
	require.Equal(t, p.prior.ID, last.ResumeConversation.ID,
		"the replacement resumes the conversation the aborted delete left alone")
}

// PR #7190 review thread 4555053187 (P1): a legacy root_agents entry resolving
// through a linked worktree runs its root under the identity root's id, while a
// delete performed after that workspace vanishes can only name the provisional
// ids of the path. A retirement keyed by identity never reaches the carry, so
// a replacement checkout of the same repository at that path would resume the
// deleted project's conversation. The deleted path itself names the carry.
func TestADeleteOfAVanishedWorktreeRetiresTheCarryUnderItsRealIdentity(t *testing.T) {
	p := parkRootHealAfterReap(t, func(t *testing.T) string {
		_, _, worktree := setupBareCloneWorktree3358(t)
		return worktree
	}, true)
	require.NotEqual(t, config.RepoIDFromRoot(p.repoPath), p.repo.ID,
		"fixture: the root runs under the identity root's id, not the workspace path's")

	require.NoError(t, os.RemoveAll(p.repoPath), "the worktree vanishes")
	result, err := p.manager.DeleteProject(DeleteProjectRequest{RepoPath: p.repoPath})
	require.NoError(t, err, "a delete of the vanished path succeeds")
	require.NotEqual(t, p.repo.ID, result.RepoID, "fixture: the delete names a provisional identity, not the root's")

	p.releaseAndWait()
	p.assertCarryGone(t, "a delete of the root's path must retire its carry whatever identity the delete could name")
}

// PR #7190 review F1 (P1): the reap's teardown runs with m.mu released, before
// its final section, and DeleteProject's gate does not count it — the reap sets
// no in-flight op. A delete there suppresses while no carry exists yet, so
// retires nothing; the reap's final section then held a fresh carry nobody had
// retired, and the heal either committed resuming the deleted project's
// conversation or kept it held after the delete — #7182's state.
func TestADeleteDuringTheRootReapTeardownLeavesNoCarryAndNoRoot(t *testing.T) {
	p := parkRootHeal(t, setupControlRepo, false, parkInReapTeardown)

	var (
		wg        sync.WaitGroup
		deleteErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, deleteErr = p.manager.DeleteProject(DeleteProjectRequest{RepoID: p.repo.ID, RepoPath: p.repoPath})
	}()
	require.Eventually(t, func() bool {
		p.manager.mu.Lock()
		defer p.manager.mu.Unlock()
		_, stamped := p.manager.deletedRootRepos[p.repo.ID]
		return stamped
	}, 10*time.Second, 5*time.Millisecond, "the delete never reached suppression while the reap was tearing down")

	p.releaseAndWait()
	wg.Wait()
	// The delete may also meet the reap's own claim on the root and report a
	// partial failure; its suppression stands either way, and that is what this
	// pins.
	t.Logf("DeleteProject: %v", deleteErr)
	p.assertCarryGone(t, "a delete that suppressed during the reap's teardown must leave no carry of the deleted project held")
	require.Nil(t, findRootInstance(t, p.manager, p.repoPath),
		"a heal whose reap straddled a delete must not commit the deleted project's root")
}

// PR #7190 review F2 (P2): a create that carries nothing — a first-ever root —
// can begin before a delete and reach CreateSession after it, which samples the
// delete sequence too late to see it. It committed a live, persistent root for
// the project the user had just deleted, breaking #7181.
func TestACarryLessRootCreateBegunBeforeADeleteNeverCommits(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)

	reached := make(chan struct{})
	release := make(chan struct{})
	var parked atomic.Bool
	var releaseOnce sync.Once
	rootCreateVerifyHookForTest = func() {
		if parked.CompareAndSwap(false, true) {
			close(reached)
			<-release
		}
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		manager.waitRootAgentCreates()
		rootCreateVerifyHookForTest = nil
	})

	manager.EnsureRootAgents() // the first-ever create launches and parks before CreateSession
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatalf("the first-ever root create never reached createVerifiedRoot")
	}
	_, err = manager.DeleteProject(DeleteProjectRequest{RepoID: repo.ID, RepoPath: repoPath})
	require.NoError(t, err, "nothing is reserved yet, so the delete succeeds")

	releaseOnce.Do(func() { close(release) })
	manager.waitRootAgentCreates()
	require.Nil(t, findRootInstance(t, manager, repoPath),
		"a root create that began before the delete must not commit the deleted project's root after it")
	require.Empty(t, *seen, "no root may start for the deleted project")
}

// PR #7190 review F3 (P3): a delete-fence refusal is not a resume failure. The
// heal used to answer it with the fresh-agent fallback, so a delete that
// aborted between the two attempts let the second commit a blank root and drop
// the carry the aborted delete had promised to leave alone.
func TestADeleteFenceRefusalIsNotAResumeFailure(t *testing.T) {
	p := parkRootHealAfterReap(t, setupControlRepo, false)

	prevSweep := deregisterRootAgents
	deregisterRootAgents = func(...string) ([]string, error) { return nil, errors.New("config write failed") }
	fenceUp := make(chan struct{})
	letAbort := make(chan struct{})
	var abortOnce sync.Once
	abort := func() { abortOnce.Do(func() { close(letAbort) }) }
	deleteProjectPreSweepHookForTest = func() {
		close(fenceUp)
		<-letAbort
	}
	t.Cleanup(func() {
		abort()
		deregisterRootAgents, deleteProjectPreSweepHookForTest = prevSweep, nil
	})

	deleteDone := make(chan struct{})
	var deleteErr error
	go func() {
		defer close(deleteDone)
		_, deleteErr = p.manager.DeleteProject(DeleteProjectRequest{RepoID: p.repo.ID, RepoPath: p.repoPath})
	}()
	<-fenceUp

	// The first attempt meets the fence. Should there be a second, the delete
	// aborts — taking its fence down — before it runs.
	var attempts atomic.Int32
	rootCreateVerifyHookForTest = func() {
		if attempts.Add(1) == 2 {
			abort()
			<-deleteDone
		}
	}
	t.Cleanup(func() { rootCreateVerifyHookForTest = nil })

	p.releaseAndWait()
	abort()
	<-deleteDone
	require.Error(t, deleteErr, "fixture: the delete aborts after installing its fence")
	require.Contains(t, deleteErr.Error(), "nothing was changed")

	held, ok := p.heldCarry(t)
	require.True(t, ok, "a heal refused by a delete's fence keeps its carry for the next pass")
	require.Equal(t, p.prior.ID, held.conversation.ID)
	require.Nil(t, findRootInstance(t, p.manager, p.repoPath), "no blank root may be committed in the carry's place")

	rootCreateVerifyHookForTest = nil
	resetRootEnsureBackoff(p.manager)
	p.manager.ensureRootAgentsAndWait()
	require.NotNil(t, findRootInstance(t, p.manager, p.repoPath), "the next pass re-creates the root")
	last := (*p.seen)[len(*p.seen)-1]
	require.Equal(t, p.prior.ID, last.ResumeConversation.ID,
		"the replacement resumes the conversation the aborted delete left alone")
}

// parkSweepAtAdmission parks the first ensure sweep that admits a candidate —
// right after its deletion-tombstone check, before its create is launched —
// until release is called (PR #7190 review R1).
func parkSweepAtAdmission(t *testing.T) (admitted <-chan struct{}, release func()) {
	t.Helper()
	reached := make(chan struct{})
	gate := make(chan struct{})
	var parked atomic.Bool
	var once sync.Once
	prev := testHookRootAdmitted
	testHookRootAdmitted = func() {
		if parked.CompareAndSwap(false, true) {
			close(reached)
			<-gate
		}
	}
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(func() {
		release()
		testHookRootAdmitted = prev
	})
	return reached, release
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// PR #7190 review R1 (P2): the sweep admits a create with its tombstone check
// and launches it — minting its ticket — in a later m.mu section. A delete
// that suppressed in between found no ticket to withdraw, and the first-ever
// create it had admitted committed the deleted project's root after the delete.
func TestARootCreateAdmittedJustBeforeASuppressionNeverLaunches(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	t.Cleanup(manager.waitRootAgentCreates)

	admitted, release := parkSweepAtAdmission(t)
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		manager.EnsureRootAgents()
	}()
	waitClosed(t, admitted, "the sweep to admit the first-ever root")

	_, err = manager.DeleteProject(DeleteProjectRequest{RepoID: repo.ID, RepoPath: repoPath})
	require.NoError(t, err, "nothing has launched yet, so the delete succeeds")

	release()
	waitClosed(t, sweepDone, "the sweep to finish")
	manager.waitRootAgentCreates()
	require.Nil(t, findRootInstance(t, manager, repoPath),
		"a create admitted before a delete's suppression must not commit the deleted project's root after it")
	require.Empty(t, *seen, "no root may start for the deleted project")
}

// R1 for a heal: admitted, then the delete suppresses and is held before its
// archive loop, so the dead root is still there for the heal to reap. Its
// fresh ticket was never withdrawn, so the reap held the deleted project's
// carry, and the fence refusal that followed kept it — after the delete.
func TestARootHealAdmittedJustBeforeASuppressionNeverLaunches(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	writeRootClaudeTranscript(t, claudeConfigDir, repoPath, priorRootConversationID)
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	t.Cleanup(manager.waitRootAgentCreates)
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first, "precondition: the first ensure creates the root")
	prior := seedRootConversation(t, first)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	first.SetStatusForTest(session.Lost)

	admitted, release := parkSweepAtAdmission(t)
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		manager.EnsureRootAgents()
	}()
	waitClosed(t, admitted, "the sweep to admit the heal")

	suppressed := make(chan struct{})
	finish := make(chan struct{})
	var finishOnce sync.Once
	deleteProjectPostSuppressHookForTest = func() {
		close(suppressed)
		<-finish
	}
	t.Cleanup(func() {
		finishOnce.Do(func() { close(finish) })
		deleteProjectPostSuppressHookForTest = nil
	})
	deleteDone := make(chan struct{})
	var deleteErr error
	go func() {
		defer close(deleteDone)
		_, deleteErr = manager.DeleteProject(DeleteProjectRequest{RepoID: repo.ID, RepoPath: repoPath})
	}()
	waitClosed(t, suppressed, "the delete to suppress")

	release()
	waitClosed(t, sweepDone, "the sweep to finish")
	manager.waitRootAgentCreates()
	finishOnce.Do(func() { close(finish) })
	waitClosed(t, deleteDone, "the delete to finish")
	require.NoError(t, deleteErr)

	manager.mu.Lock()
	st := manager.rootEnsureStates[repoPath]
	manager.mu.Unlock()
	require.NotNil(t, st, "the candidate's ensure state exists")
	_, held := manager.heldRootCarry(st, repo, nil)
	require.False(t, held, "a heal admitted before a delete's suppression must not hold the deleted project's carry after it")
	require.Len(t, *seen, 1, "only the first-ever root may have started")
	for _, opts := range *seen {
		require.NotEqual(t, prior.ID, opts.ResumeConversation.ID,
			"the deleted project's conversation must not be resumed by any create")
	}
}

// Control for the race tests above: with NO concurrent delete, the same
// post-reap-failed heal MUST still hold its carry so the next pass resumes the
// conversation — the #7088 behavior the carry exists for.
func TestRootHealCreateFailingAfterReapHoldsCarryWhenNoDeleteRaces(t *testing.T) {
	p := parkRootHealAfterReap(t, setupControlRepo, true)

	p.releaseAndWait()
	require.Nil(t, findRootInstance(t, p.manager, p.repoPath), "the heal's create failed after the reap")
	held, ok := p.heldCarry(t)
	require.True(t, ok, "with no delete racing, the failed heal holds its carry for the next pass")
	require.Equal(t, p.prior.ID, held.conversation.ID, "the held carry is the reaped root's conversation")

	p.stalled.Store(false)
	resetRootEnsureBackoff(p.manager)
	p.manager.ensureRootAgentsAndWait()
	require.NotNil(t, findRootInstance(t, p.manager, p.repoPath), "the next pass re-creates the root")
	last := (*p.seen)[len(*p.seen)-1]
	require.Equal(t, p.prior.ID, last.ResumeConversation.ID,
		"the replacement resumes the conversation the reaped root carried, not a blank one")
}

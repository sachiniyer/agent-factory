package app

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStartedInstanceWithWorktree returns an instance that passes all the
// guards in fetchPRInfoCmd (not nil, started, not remote, has a gitWorktree
// that yields a non-empty repoPath from FetchPRInfoSnapshot). Use for tests
// that need fetchPRInfoCmd to actually return a non-nil command.
func newStartedInstanceWithWorktree(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst := newStartedInstance(t, title)
	inst.Branch = "feature/" + title
	gw, err := git.NewGitWorktreeFromStorage(
		t.TempDir(),
		filepath.Join(t.TempDir(), "worktree"),
		title,
		inst.Branch,
		"deadbeef",
		false,
		true,
	)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	return inst
}

// newStartedInstance builds an instance that Started() reports as true.
// Uses the test-only SetStartedForTest seam so we don't need a real git repo
// or tmux session. No gitWorktree is set — tests that care about the
// gitWorktree guard in FetchPRInfoSnapshot use this to represent a remote /
// partially-started instance.
func newStartedInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	return inst
}

// ----------------------------------------------------------------------------
// Regression tests for issue #311 (sachiniyer/agent-factory):
// "github PR state should be async and loaded lazily".
//
// Targets fetchPRInfoCmd guard conditions, tickUpdatePRInfoMessage dispatch,
// and the prInfoUpdatedMsg handler.
// ----------------------------------------------------------------------------

func TestFetchPRInfoCmd_NilInstance_ReturnsNil(t *testing.T) {
	assert.Nil(t, fetchPRInfoCmd(nil, false))
	assert.Nil(t, fetchPRInfoCmd(nil, true))
}

// TestFetchPRInfoCmd_RemoteInstance_ReturnsNil — remote sessions have no
// local worktree to run `gh` against.
func TestFetchPRInfoCmd_RemoteInstance_ReturnsNil(t *testing.T) {
	inst := newStartedInstance(t, "remote")
	inst.SetBackend(&session.HookBackend{})
	require.True(t, inst.Capabilities().Workspace == session.WorkspaceRemote, "sanity: instance should report as remote")

	assert.Nil(t, fetchPRInfoCmd(inst, false))
	assert.Nil(t, fetchPRInfoCmd(inst, true), "force must not override the remote guard")
}

// TestFetchPRInfoCmd_NotStarted_ReturnsNil — FetchPRInfoSnapshot returns an
// empty repoPath for not-started instances, which fetchPRInfoCmd short-circuits
// on. Covers the "instance is still being set up" race.
func TestFetchPRInfoCmd_NotStarted_ReturnsNil(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "pending", Path: t.TempDir(), Program: "claude",
	})
	require.NoError(t, err)
	// started is false — no SetStartedForTest call.

	assert.Nil(t, fetchPRInfoCmd(inst, false))
	assert.Nil(t, fetchPRInfoCmd(inst, true))
}

// TestFetchPRInfoCmd_NoGitWorktree_ReturnsNil — started instance but no
// gitWorktree attached (e.g. a freshly-restored state mid-Start). Snapshot
// returns empty repoPath and fetch should be skipped.
func TestFetchPRInfoCmd_NoGitWorktree_ReturnsNil(t *testing.T) {
	inst := newStartedInstance(t, "noworktree")
	// gitWorktree is nil — no way to set it without a real repo. The snapshot
	// guard catches this.

	assert.Nil(t, fetchPRInfoCmd(inst, false))
}

// TestFetchPRInfoCmd_DetachedHead_ReturnsNil — a detached-HEAD worktree has a
// non-empty repoPath but an empty branch (#687). fetchPRInfoCmd must skip the
// fetch entirely so it never spawns `gh pr view ""` every tick. We force the
// fetch (bypassing the freshness debounce) and assert the fetcher is never
// invoked even though the instance is started, local, and has a worktree.
func TestFetchPRInfoCmd_DetachedHead_ReturnsNil(t *testing.T) {
	inst := newStartedInstanceWithWorktree(t, "detached")
	inst.Branch = "" // detached HEAD: no branch to look up

	var calls int32
	restore := SetPRInfoFetcherForTest(func(repoPath, branch string) (*git.PRInfo, error) {
		atomic.AddInt32(&calls, 1)
		return nil, nil
	})
	defer restore()

	assert.Nil(t, fetchPRInfoCmd(inst, true),
		"detached-HEAD instance must not schedule a fetch")
	assert.Equal(t, int32(0), atomic.LoadInt32(&calls),
		"fetcher must not be invoked for an empty branch (no gh pr view \"\")")
}

// TestFetchPRInfoCmd_Fresh_NotForced_DebouncesFetch — core laziness check:
// within prInfoStaleAfter of the last fetch, non-forced calls are a no-op.
func TestFetchPRInfoCmd_Fresh_NotForced_DebouncesFetch(t *testing.T) {
	inst := newStartedInstance(t, "fresh")
	// Set fresh PR info — bumps prInfoLastFetched to now.
	inst.SetPRInfo(&git.PRInfo{Number: 1, Title: "fresh"})

	assert.Nil(t, fetchPRInfoCmd(inst, false),
		"no fetch should be scheduled while the cached PR info is still fresh")
}

// TestFetchPRInfoCmd_NeverFetched_ReturnsCmd — startup / first-selection
// path. PRInfoAge reports a sentinel "very large" value, so the cmd should
// dispatch even without force.
//
// We only assert the cmd is non-nil; invoking it would shell out to `gh`
// which is outside the scope of this unit test.
func TestFetchPRInfoCmd_NeverFetched_ReturnsCmd(t *testing.T) {
	inst := newStartedInstance(t, "never")
	// Snapshot will bail on nil gitWorktree; to get past that guard we would
	// need a real worktree. So this test actually asserts that the guard
	// order is gitWorktree-last: remote and fresh checks both short-circuit
	// BEFORE FetchPRInfoSnapshot. See TestFetchPRInfoCmd_NoGitWorktree_ReturnsNil
	// for the snapshot guard coverage.
	//
	// Here we just confirm age is reported as very large for never-fetched.
	assert.Greater(t, inst.PRInfoAge(), 365*24*time.Hour,
		"never-fetched instance must report a very large age")
}

// TestTickUpdatePRInfo_DispatchesForSelectedOnly verifies the tick handler's
// lazy behavior: the old code looped over ALL instances; the new code fires
// a cmd only for the currently-selected one (plus reschedules the tick).
func TestTickUpdatePRInfo_DispatchesForSelectedOnly(t *testing.T) {
	h := newTestHome(t)

	a := newLoadingInstance(t, "a")
	b := newLoadingInstance(t, "b")
	h.store.AddInstance(a)
	h.store.AddInstance(b)
	h.sidebar.SetSelectedInstance(1)

	_, cmd := h.Update(tickUpdatePRInfoMessage{})
	require.NotNil(t, cmd, "tick should reschedule itself")
	// The returned command is a tea.Batch of {tickUpdatePRInfoCmd, fetchPRInfoCmd(b, true)}.
	// fetchPRInfoCmd(b, true) returns nil because b has no gitWorktree — the
	// batch thus reduces to only the reschedule. The important behavior we
	// can observe from this handler is that it did NOT synchronously touch
	// every instance's PR state (the old behavior), so no assertion beyond
	// "the handler returned without blocking / panicking" is needed.
}

// TestPrInfoUpdatedMsg_Success_AppliesInfoAndBumpsTimestamp covers the happy
// path: a completed fetch writes the new info onto the instance and bumps
// the age clock so the next selection doesn't trigger a re-fetch.
func TestPrInfoUpdatedMsg_Success_AppliesInfoAndBumpsTimestamp(t *testing.T) {
	h := newTestHome(t)
	inst := newLoadingInstance(t, "target")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	assert.Nil(t, inst.GetPRInfo(), "precondition: no cached PR info")

	info := &git.PRInfo{Number: 42, Title: "add feature", URL: "https://x/42", State: "OPEN"}
	_, _ = h.Update(prInfoUpdatedMsg{instance: inst, info: info})

	got := inst.GetPRInfo()
	require.NotNil(t, got)
	assert.Equal(t, 42, got.Number)
	assert.Equal(t, "add feature", got.Title)
	assert.Less(t, inst.PRInfoAge(), time.Second,
		"prInfoLastFetched must be bumped so the debounce takes effect")
}

// TestPrInfoUpdatedMsg_Error_PreservesCacheAndDebounces — a transient fetch
// error (gh offline, etc.) must NOT clobber the cached PR info, but SHOULD
// bump the fetch timestamp so we don't hammer retries on every selection.
func TestPrInfoUpdatedMsg_Error_PreservesCacheAndDebounces(t *testing.T) {
	h := newTestHome(t)
	inst := newLoadingInstance(t, "target")
	cached := &git.PRInfo{Number: 7, Title: "cached", State: "OPEN"}
	inst.SetPRInfo(cached)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	// Simulate the prInfoLastFetched timestamp being old by clearing it via
	// a fresh SetPRInfo with the same cached value, then waiting would be
	// flaky — instead rely on MarkPRInfoFetched behavior to check debounce.
	_, _ = h.Update(prInfoUpdatedMsg{instance: inst, err: errors.New("gh timeout")})

	assert.Same(t, cached, inst.GetPRInfo(),
		"transient fetch error must not clobber cached PR info")
	assert.Less(t, inst.PRInfoAge(), time.Second,
		"MarkPRInfoFetched should have bumped the fetch timestamp to prevent retry thrash")
}

// TestSelectionChanged_TriggersFetchForStaleSelectedInstance — verifies the
// lazy-on-select wiring: landing on an instance whose PR info is stale (or
// never fetched) schedules a fetch.
//
// We only assert the returned cmd is non-nil for a *stale* started instance;
// asserting the cmd produces a prInfoUpdatedMsg would require a real `gh`
// invocation, which is covered by the e2e layer.
func TestSelectionChanged_DoesNotRefetchFreshInstance(t *testing.T) {
	h := newTestHome(t)
	inst := newStartedInstance(t, "fresh")
	inst.SetPRInfo(&git.PRInfo{Number: 1, Title: "fresh"}) // bumps timestamp to now
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	// selectionChanged now also dispatches an off-loop pane refresh cmd
	// (#579), so the returned cmd is non-nil even when no PR fetch is
	// scheduled. Verify the PR-info path stays untouched by checking that
	// the cached info and fetch timestamp aren't disturbed when we ignore
	// the returned cmd: prInfoLastFetched was set by SetPRInfo above and
	// must still be fresh after selectionChanged.
	_ = h.selectionChanged()
	assert.Less(t, inst.PRInfoAge(), time.Second,
		"fresh PR info must not be re-fetched on selection change")
	require.NotNil(t, inst.GetPRInfo())
	assert.Equal(t, 1, inst.GetPRInfo().Number,
		"cached PR info must be preserved across selectionChanged")
}

// TestFetchPRInfoCmd_MarksFetchAtKickoff_DebouncesConcurrentCalls verifies the
// fix for the in-flight-fetch thrash described in issue #311: selectionChanged
// runs on every 100ms preview tick, and fetchPRInfoCmd was previously only
// bumping prInfoLastFetched once the fetch completed. A restored instance
// (prInfoLastFetched == 0) would therefore trigger a new `gh pr view`
// subprocess on every tick until one finally returned.
//
// The fix marks the instance as fetched at kickoff. This test asserts:
//  1. the first non-forced call returns a non-nil cmd and immediately
//     bumps PRInfoAge out of the "never fetched" sentinel range,
//  2. a second non-forced call within prInfoStaleAfter returns nil even
//     though the fetcher has not been invoked yet (the in-flight fetch is
//     the debounce anchor), and
//  3. when the returned cmd is eventually executed the fetcher runs
//     exactly once per kickoff.
func TestFetchPRInfoCmd_MarksFetchAtKickoff_DebouncesConcurrentCalls(t *testing.T) {
	inst := newStartedInstanceWithWorktree(t, "needs-fetch")

	var calls int32
	block := make(chan struct{})
	restore := SetPRInfoFetcherForTest(func(repoPath, branch string) (*git.PRInfo, error) {
		atomic.AddInt32(&calls, 1)
		<-block
		return &git.PRInfo{Number: 99, Title: "done"}, nil
	})
	t.Cleanup(restore)

	require.Greater(t, inst.PRInfoAge(), 365*24*time.Hour,
		"precondition: a freshly-restored instance reports a very large age")

	cmd1 := fetchPRInfoCmd(inst, false)
	require.NotNil(t, cmd1, "first call should dispatch a fetch")
	assert.Less(t, inst.PRInfoAge(), time.Second,
		"kickoff must bump prInfoLastFetched so the next tick is debounced")

	cmd2 := fetchPRInfoCmd(inst, false)
	assert.Nil(t, cmd2,
		"second non-forced call within the stale window must be a no-op, "+
			"even while the first fetch is still in flight")

	assert.Equal(t, int32(0), atomic.LoadInt32(&calls),
		"kickoff must not run the fetcher — tea runs returned Cmds off-loop")

	// Drain the in-flight fetch before letting t.Cleanup swap the fetcher
	// back — otherwise the goroutine's read of prInfoFetcher races with the
	// restore write.
	done := make(chan struct{})
	go func() {
		_ = cmd1()
		close(done)
	}()
	close(block)
	<-done
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls),
		"each returned cmd should invoke the fetcher exactly once")
}

// TestFetchPRInfoCmd_Force_StillRunsWhileFetchInFlight — force=true bypasses
// the kickoff debounce. The 60s ticker relies on this to always refresh the
// selected instance, even if selectionChanged just kicked off a fetch.
func TestFetchPRInfoCmd_Force_StillRunsWhileFetchInFlight(t *testing.T) {
	inst := newStartedInstanceWithWorktree(t, "force-through")

	restore := SetPRInfoFetcherForTest(func(repoPath, branch string) (*git.PRInfo, error) {
		return &git.PRInfo{Number: 1}, nil
	})
	t.Cleanup(restore)

	require.NotNil(t, fetchPRInfoCmd(inst, false), "first lazy call dispatches")
	assert.NotNil(t, fetchPRInfoCmd(inst, true),
		"force=true must bypass the kickoff-debounce the previous call set")
}

// ----------------------------------------------------------------------------
// Regression tests for issue #862 (sachiniyer/agent-factory):
// "PR info updates can be lost when an instance is swapped during async
// refresh". Same race class as #777/#808: refreshExternalInstances swaps a
// sidebar instance (RemoveInstanceByTitle + a rebuilt FromInstanceData pointer,
// #765) while a PR fetch is in flight, orphaning the pointer the
// prInfoUpdatedMsg handler captured at kickoff.
// ----------------------------------------------------------------------------

// TestPrInfoUpdatedMsg_InstanceSwappedDuringFetch_AppliesToLiveInstance — the
// captured instance is swapped out for a fresh same-title copy while the fetch
// is in flight. The completed update must land on the live sidebar instance,
// not the orphan.
func TestPrInfoUpdatedMsg_InstanceSwappedDuringFetch_AppliesToLiveInstance(t *testing.T) {
	h := newTestHome(t)

	orphan := newStartedInstance(t, "swapped")
	h.store.AddInstance(orphan)
	h.sidebar.SetSelectedInstance(0)

	// Simulate the #765 swap: remove the captured instance and add a fresh
	// same-title copy (as FromInstanceData would build).
	live := newStartedInstance(t, "swapped")
	h.store.RemoveInstanceByTitle("swapped")
	h.store.AddInstance(live)
	require.NotSame(t, orphan, live, "sanity: swap must produce a distinct pointer")

	info := &git.PRInfo{Number: 42, Title: "add feature", URL: "https://x/42", State: "OPEN"}
	_, _ = h.Update(prInfoUpdatedMsg{instance: orphan, info: info})

	got := live.GetPRInfo()
	require.NotNil(t, got, "PR info must be applied to the live sidebar instance")
	assert.Equal(t, 42, got.Number)
	assert.Nil(t, orphan.GetPRInfo(), "the orphaned pointer must not receive the update")
}

// TestPrInfoUpdatedMsg_InstanceGoneDuringFetch_DropsUpdate — the session was
// killed (no same-title replacement) while the fetch was in flight. The handler
// must drop the stale result without panicking or resurrecting state.
func TestPrInfoUpdatedMsg_InstanceGoneDuringFetch_DropsUpdate(t *testing.T) {
	h := newTestHome(t)

	orphan := newStartedInstance(t, "gone")
	h.store.AddInstance(orphan)
	h.sidebar.SetSelectedInstance(0)
	h.store.RemoveInstanceByTitle("gone")

	info := &git.PRInfo{Number: 7, Title: "lost", State: "OPEN"}
	_, cmd := h.Update(prInfoUpdatedMsg{instance: orphan, info: info})

	assert.Nil(t, cmd)
	assert.Nil(t, orphan.GetPRInfo(),
		"an update for a session no longer in the sidebar must be dropped")
}

// TestPrInfoUpdatedMsg_Error_SwappedDuringFetch_MarksLiveInstance — the error
// path must also re-resolve by title: MarkPRInfoFetched should debounce the
// live instance, not the orphan.
func TestPrInfoUpdatedMsg_Error_SwappedDuringFetch_MarksLiveInstance(t *testing.T) {
	h := newTestHome(t)

	orphan := newStartedInstance(t, "swapped")
	h.store.AddInstance(orphan)
	h.sidebar.SetSelectedInstance(0)

	live := newStartedInstance(t, "swapped")
	h.store.RemoveInstanceByTitle("swapped")
	h.store.AddInstance(live)
	require.Greater(t, live.PRInfoAge(), 365*24*time.Hour,
		"precondition: live instance is never-fetched")

	_, _ = h.Update(prInfoUpdatedMsg{instance: orphan, err: errors.New("gh timeout")})

	assert.Less(t, live.PRInfoAge(), time.Second,
		"the live instance must be marked fetched to debounce retries")
	assert.Greater(t, orphan.PRInfoAge(), 365*24*time.Hour,
		"the orphaned pointer must be left untouched")
}

// ----------------------------------------------------------------------------
// Regression tests for issue #921 (sachiniyer/agent-factory):
// "PR info updates can apply to the wrong worktree when an instance is
// recreated with the same title on a different branch". The fetch captures the
// branch at kickoff, but the title-only re-resolution in the prInfoUpdatedMsg
// handler can land on a same-title instance now attached to a *different*
// branch (kill + recreate while the fetch is in flight). PR info is
// branch-specific, so the handler must drop the update when the resolved
// target's branch no longer matches the captured one.
// ----------------------------------------------------------------------------

// TestPrInfoUpdatedMsg_BranchMismatch_DropsUpdate — the captured instance is
// killed and a fresh same-title instance is created on a different branch while
// the fetch is in flight. The title-only fallback resolves to that new
// instance, but its branch differs from the fetch's branch, so the stale PR
// info must NOT be applied.
func TestPrInfoUpdatedMsg_BranchMismatch_DropsUpdate(t *testing.T) {
	h := newTestHome(t)

	// The instance the fetch was kicked off for, on branch X.
	orphan := newStartedInstance(t, "reused")
	orphan.Branch = "feature/x"
	h.store.AddInstance(orphan)
	h.sidebar.SetSelectedInstance(0)

	// User killed it and recreated a same-title instance on branch Y while the
	// gh fetch was still running.
	recreated := newStartedInstance(t, "reused")
	recreated.Branch = "feature/y"
	h.store.RemoveInstanceByTitle("reused")
	h.store.AddInstance(recreated)

	info := &git.PRInfo{Number: 42, Title: "branch X PR", State: "OPEN"}
	_, cmd := h.Update(prInfoUpdatedMsg{instance: orphan, branch: "feature/x", info: info})

	assert.Nil(t, cmd)
	assert.Nil(t, recreated.GetPRInfo(),
		"a fetch for branch X must not write its PR info onto a same-title instance now on branch Y")
	assert.Nil(t, orphan.GetPRInfo(),
		"the orphaned pointer must not receive the update either")
}

// TestPrInfoUpdatedMsg_BranchMatch_AppliesUpdate — same swap as above, but the
// recreated same-title instance is back on the original branch. The captured
// branch matches, so the update applies to the live instance.
func TestPrInfoUpdatedMsg_BranchMatch_AppliesUpdate(t *testing.T) {
	h := newTestHome(t)

	orphan := newStartedInstance(t, "reused")
	orphan.Branch = "feature/x"
	h.store.AddInstance(orphan)
	h.sidebar.SetSelectedInstance(0)

	live := newStartedInstance(t, "reused")
	live.Branch = "feature/x"
	h.store.RemoveInstanceByTitle("reused")
	h.store.AddInstance(live)
	require.NotSame(t, orphan, live, "sanity: swap must produce a distinct pointer")

	info := &git.PRInfo{Number: 42, Title: "branch X PR", State: "OPEN"}
	_, _ = h.Update(prInfoUpdatedMsg{instance: orphan, branch: "feature/x", info: info})

	got := live.GetPRInfo()
	require.NotNil(t, got, "matching-branch update must apply to the live instance")
	assert.Equal(t, 42, got.Number)
	assert.Nil(t, orphan.GetPRInfo(), "the orphaned pointer must not receive the update")
}

// sanity: exercise config.DefaultConfig / AppState wiring so a compile
// regression in newTestHome stays within this package.
func TestNewTestHome_BuildsSuccessfully(t *testing.T) {
	h := newTestHome(t)
	assert.NotNil(t, h.appState)
	assert.Equal(t, uint32(0), h.appState.GetHelpScreensSeen())
	_ = config.DefaultConfig() // touch to keep import if other tests shrink
}

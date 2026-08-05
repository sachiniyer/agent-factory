package daemon

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// TestReserveCreate_DoesNotHoldTheManagerLockAcrossBackendResolution is the
// #2931 regression guard.
//
// reserveCreate resolves the backend through config.RepoFromPath, which shells
// out to `git rev-parse` with no context, no timeout and no WaitDelay. While that
// ran under m.mu, one unreachable repo — a stalled NFS/FUSE mount, a spun-down
// disk — was a daemon-wide outage: the exec never returns, the deferred unlock
// never runs, and everything needing m.mu blocks behind it in every project.
//
// The property is invisible to an ordinary create test: the resolution succeeds
// whether or not the lock is held, and only its LOCK CONTEXT is the defect. So
// this blocks inside the resolver and asks, from another goroutine, whether the
// manager lock is still obtainable.
//
// It is written to FAIL rather than hang on a regression, following
// deadlock_2006_test.go: the observer reports through a channel with a deadline,
// so a re-introduced hold is a red test and not a wedged CI job.
func TestReserveCreate_DoesNotHoldTheManagerLockAcrossBackendResolution(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)

	inResolver := make(chan struct{})
	releaseResolver := make(chan struct{})
	prev := backendKindForCreate
	backendKindForCreate = func(opts session.InstanceOptions, root string) (session.BackendKind, error) {
		close(inResolver)
		<-releaseResolver
		return prev(opts, root)
	}
	t.Cleanup(func() { backendKindForCreate = prev })

	created := make(chan struct{})
	go func() {
		defer close(created)
		_, _, release, _, err := manager.reserveCreate(CreateSessionRequest{
			RepoPath: repoPath, TitleBase: "lockscope", Program: "claude",
		})
		if err == nil && release != nil {
			release()
		}
	}()

	select {
	case <-inResolver:
	case <-time.After(20 * time.Second):
		t.Fatal("the create never reached backend resolution")
	}

	// THE CLAIM: with a create parked mid-resolution, the manager lock is free.
	// Snapshot is the reader that matters — it is what every TUI and web client
	// polls, so a held lock here is a frozen UI for every project.
	lockFree := make(chan struct{})
	go func() {
		manager.Snapshot("")
		close(lockFree)
	}()

	select {
	case <-lockFree:
	case <-time.After(10 * time.Second):
		close(releaseResolver)
		t.Fatal("m.mu was held across backend resolution: Snapshot could not run while a create " +
			"was resolving its runtime, so one stalled repo would wedge every session (#2931)")
	}

	close(releaseResolver)
	select {
	case <-created:
	case <-time.After(20 * time.Second):
		t.Fatal("the create never finished after the resolver was released")
	}
}

// TestReserveCreate_RefusesWhenAProjectDeleteCompletedDuringResolution is the
// other half of moving backend resolution out from under m.mu.
//
// m.projectDeletes answers "is a delete running RIGHT NOW". That was a complete
// answer only while the slow resolution happened under m.mu, because a delete
// then could not reach its own fence-install until the create had already
// reserved or refused. With the resolution hoisted, a delete has room to install
// the fence, run to completion, and REMOVE the fence entirely inside the window
// — after which the fence check reads a clear field and would admit a create
// into a project the user just deleted, re-creating its state behind them.
//
// The delete below is the real DeleteProject, not a hand-installed fence, so the
// test fails if the generation stops being bumped on the production path.
func TestReserveCreate_RefusesWhenAProjectDeleteCompletedDuringResolution(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	inResolver := make(chan struct{})
	releaseResolver := make(chan struct{})
	prev := backendKindForCreate
	backendKindForCreate = func(opts session.InstanceOptions, root string) (session.BackendKind, error) {
		close(inResolver)
		<-releaseResolver
		return prev(opts, root)
	}
	t.Cleanup(func() { backendKindForCreate = prev })

	type createResult struct {
		release func()
		err     error
	}
	results := make(chan createResult, 1)
	go func() {
		_, _, release, _, err := manager.reserveCreate(CreateSessionRequest{
			RepoPath: repoPath, TitleBase: "deleted-under-me", Program: "claude",
		})
		results <- createResult{release: release, err: err}
	}()

	select {
	case <-inResolver:
	case <-time.After(20 * time.Second):
		t.Fatal("the create never reached backend resolution")
	}

	// The whole delete — fence install, durable removals, fence removal — runs
	// and FINISHES while the create is parked. By the time the create takes m.mu
	// there is nothing left in m.projectDeletes for it to see.
	_, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.NoError(t, err, "the delete must complete: the create has reserved nothing to block it")
	manager.mu.Lock()
	_, fenceStillUp := manager.projectDeletes[repoID]
	manager.mu.Unlock()
	require.False(t, fenceStillUp, "precondition: the delete must have removed its own fence before the create resumes")

	close(releaseResolver)
	var got createResult
	select {
	case got = <-results:
	case <-time.After(20 * time.Second):
		t.Fatal("the create never finished after the resolver was released")
	}
	if got.release != nil {
		got.release()
	}
	require.Error(t, got.err, "a create must not be admitted into a project whose delete completed while it resolved")
	require.ErrorContains(t, got.err, "while this session create resolved its backend")
	require.Nil(t, got.release, "a refused create must not hand back a reservation to release")
}

// A task create that is already at its concurrency cap must be refused BEFORE
// backend resolution, for the same reason an already-deleted project's create
// is: the answer is already known, and the resolver is unbounded git.
//
// The cap's contract is that a refusal is cheap and the watch-delivery path
// parks the event to retry when a slot frees. Hanging in the resolver instead
// converts that park into a stall on exactly the repo this change is about.
func TestReserveCreate_RefusesACappedTaskRunBeforeResolvingTheBackend(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	// Fill the single slot with a reservation — an admitted create that has not
	// registered its instance yet, which is what the cap counts.
	manager.mu.Lock()
	manager.reserveTaskRunLocked(repoID, "task-1", 1)
	manager.mu.Unlock()

	var resolverRuns atomic.Int64
	prev := backendKindForCreate
	backendKindForCreate = func(opts session.InstanceOptions, root string) (session.BackendKind, error) {
		resolverRuns.Add(1)
		return prev(opts, root)
	}
	t.Cleanup(func() { backendKindForCreate = prev })

	_, _, release, _, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath:          repoPath,
		TitleBase:         "capped",
		Program:           "claude",
		TaskID:            "task-1",
		MaxConcurrentRuns: 1,
	})
	if release != nil {
		release()
	}
	require.ErrorIs(t, err, errAtConcurrencyLimit,
		"a create over its task cap must be refused with the sentinel the delivery path parks on")
	require.Nil(t, release, "a refused create must not hand back a reservation to release")
	require.Zero(t, resolverRuns.Load(),
		"the cap refusal must come BEFORE backend resolution: resolving first makes a capped delivery wait on unbounded git instead of parking")
}

// TestReserveCreate_RefusesWhenAProjectDeleteWasAlreadyRunningAtTheSample is the
// INVERSE ordering, which the delete generation alone does not catch.
//
// The generation answers "did a delete BEGIN after my sample". A delete already
// running at the sample bumped the counter before it and drops its fence before
// the re-check, so both readings match and the create would be admitted despite
// that delete running across the entire unlocked region.
//
// It asserts two things, and the second is why the refusal sits where it does:
// the create is refused, and it is refused WITHOUT resolving its backend. A
// create that is already known to be impossible must not first wait on the
// unbounded `git rev-parse` this change exists to keep off blocking paths.
//
// The delete is parked inside deregisterRootAgents, which runs after the fence is
// installed, so a create that starts while it is parked necessarily samples with
// the fence up — the state the test needs and cannot otherwise pin down.
func TestReserveCreate_RefusesWhenAProjectDeleteWasAlreadyRunningAtTheSample(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	inDelete := make(chan struct{})
	releaseDelete := make(chan struct{})
	prevDeregister := deregisterRootAgents
	deregisterRootAgents = func(id string) ([]string, error) {
		close(inDelete)
		<-releaseDelete
		return prevDeregister(id)
	}
	t.Cleanup(func() { deregisterRootAgents = prevDeregister })

	deleted := make(chan error, 1)
	go func() {
		_, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
		deleted <- err
	}()
	select {
	case <-inDelete:
	case <-time.After(20 * time.Second):
		t.Fatal("the delete never reached its root-agent deregistration")
	}
	// The fence is up and the generation is already bumped. Anything the create
	// samples from here carries the delete's post-bump value.
	manager.mu.Lock()
	_, fenceUp := manager.projectDeletes[repoID]
	manager.mu.Unlock()
	require.True(t, fenceUp, "precondition: the delete must hold its fence while parked")

	// The resolver must never run. A create refused for an active delete is
	// already known to be impossible, and backend resolution is the unbounded
	// `git rev-parse` this whole change exists to keep off the blocking paths —
	// so making that create wait on a stalled mount before telling it no is the
	// defect, not merely an inefficiency. Before the resolution was hoisted, the
	// fence check ran ahead of it and such a create returned promptly (#2937
	// review); this asserts that property survived the hoist.
	var resolverRuns atomic.Int64
	prevResolver := backendKindForCreate
	backendKindForCreate = func(opts session.InstanceOptions, root string) (session.BackendKind, error) {
		resolverRuns.Add(1)
		return prevResolver(opts, root)
	}
	t.Cleanup(func() { backendKindForCreate = prevResolver })

	type createResult struct {
		release func()
		err     error
	}
	results := make(chan createResult, 1)
	go func() {
		_, _, release, _, err := manager.reserveCreate(CreateSessionRequest{
			RepoPath: repoPath, TitleBase: "sampled-mid-delete", Program: "claude",
		})
		results <- createResult{release: release, err: err}
	}()

	// It must refuse while the delete is STILL PARKED. Waiting on the result here,
	// before releasing the delete, is the assertion: a create that only refused
	// after the delete finished would time out instead.
	var got createResult
	select {
	case got = <-results:
	case <-time.After(20 * time.Second):
		close(releaseDelete)
		<-deleted
		t.Fatal("the create did not refuse while a delete held the fence: it is waiting on something, which is what refusing before backend resolution is meant to prevent")
	}
	if got.release != nil {
		got.release()
	}
	require.Error(t, got.err, "a create sampled during an active delete must be refused")
	require.ErrorContains(t, got.err, "is being deleted")
	require.Nil(t, got.release, "a refused create must not hand back a reservation to release")
	require.Zero(t, resolverRuns.Load(),
		"the refusal must come BEFORE backend resolution: resolving first makes an impossible create wait on unbounded git")

	close(releaseDelete)
	select {
	case err := <-deleted:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("the delete never completed")
	}
}

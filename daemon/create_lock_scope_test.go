package daemon

import (
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

// TestReserveCreate_RefusesWhenAProjectDeleteWasAlreadyRunningAtTheSample is the
// INVERSE ordering, and the delete generation alone does not catch it.
//
// The generation answers "did a delete BEGIN after my sample". A delete that was
// ALREADY running when the create sampled bumped the counter before that sample
// and removes its fence before the re-check — so both readings match, the fence
// reads clear, and the create is admitted even though the delete ran to
// completion across the entire unlocked region. Reported on the #2937 review with
// a working repro; this is that repro.
//
// The delete is parked inside deregisterRootAgents, which runs after the fence is
// installed, so a create that starts while it is parked necessarily samples with
// the fence up — which is the state the test needs and cannot otherwise pin down.
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

	inResolver := make(chan struct{})
	releaseResolver := make(chan struct{})
	prevResolver := backendKindForCreate
	backendKindForCreate = func(opts session.InstanceOptions, root string) (session.BackendKind, error) {
		close(inResolver)
		<-releaseResolver
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
	// Reaching the resolver is what proves the sample already happened, and it
	// happened while the fence above was up.
	select {
	case <-inResolver:
	case <-time.After(20 * time.Second):
		t.Fatal("the create never reached backend resolution")
	}

	// Now let the delete finish and drop its fence, so the create's re-check sees
	// a clear field and an unchanged generation.
	close(releaseDelete)
	select {
	case err := <-deleted:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("the delete never completed")
	}
	manager.mu.Lock()
	_, fenceStillUp := manager.projectDeletes[repoID]
	manager.mu.Unlock()
	require.False(t, fenceStillUp, "precondition: the fence must be down before the create resumes")

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
	require.Error(t, got.err, "a create that sampled DURING an active delete must be refused, not admitted once that delete finishes")
	require.ErrorContains(t, got.err, "while this session create resolved its backend")
	require.Nil(t, got.release, "a refused create must not hand back a reservation to release")
}

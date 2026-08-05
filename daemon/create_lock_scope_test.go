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

package daemon

import (
	"testing"
	"time"

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

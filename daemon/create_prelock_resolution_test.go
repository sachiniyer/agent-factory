package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// TestReserveCreate_RefusesADeleteThatCompletedWhileResolvingTheRepo is the
// #2947 guard: the window BEFORE reserveCreate knows which repo it is talking
// about.
//
// config.RepoFromPath is the first thing reserveCreate does, and it shells out
// to `git rev-parse` with no context and no deadline — the same unbounded call
// #2931 moved the manager lock off, sitting outside that lock on master too. A
// DeleteProject can install its fence, run to completion, and remove the fence
// entirely while it is stalled on an unreachable mount.
//
// #2937's per-repo delete generation cannot cover this one. Sampling it needs
// repo.ID, and repo.ID is precisely what this call returns, so the interval from
// entering reserveCreate to RepoFromPath returning is unfenced BY CONSTRUCTION —
// no amount of re-reading a repo-keyed map closes a gap that precedes the key.
// The fix is to sample something that needs no key: a monotonic counter over
// every fence transition, taken at entry and compared against this repo's most
// recent transition once the identity is known.
//
// The delete below is the real DeleteProject, so this fails if the sequence
// stops being recorded on the production path.
func TestReserveCreate_RefusesADeleteThatCompletedWhileResolvingTheRepo(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	inResolve := make(chan struct{})
	releaseResolve := make(chan struct{})
	prev := repoFromPathForCreate
	repoFromPathForCreate = func(path string) (*config.RepoContext, error) {
		close(inResolve)
		<-releaseResolve
		return prev(path)
	}
	t.Cleanup(func() { repoFromPathForCreate = prev })

	type createResult struct {
		release func()
		err     error
	}
	results := make(chan createResult, 1)
	go func() {
		_, _, release, _, err := manager.reserveCreate(CreateSessionRequest{
			RepoPath: repoPath, TitleBase: "repo-resolved-under-me", Program: "claude",
		})
		results <- createResult{release: release, err: err}
	}()

	select {
	case <-inResolve:
	case <-time.After(20 * time.Second):
		t.Fatal("the create never reached repo resolution")
	}

	// The entire delete runs inside the window where the create does not yet know
	// its own repo id. Nothing repo-keyed can have been sampled yet, which is the
	// whole point.
	_, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.NoError(t, err, "the delete must complete: the create has reserved nothing to block it")
	manager.mu.Lock()
	_, fenceStillUp := manager.projectDeletes[repoID]
	manager.mu.Unlock()
	require.False(t, fenceStillUp, "precondition: the fence must be down before the create resumes, so only the sequence can catch this")

	close(releaseResolve)
	var got createResult
	select {
	case got = <-results:
	case <-time.After(20 * time.Second):
		t.Fatal("the create never finished after repo resolution was released")
	}
	if got.release != nil {
		got.release()
	}
	require.Error(t, got.err, "a create must not be admitted into a project whose delete completed while it was still resolving which project that was")
	require.ErrorContains(t, got.err, "being deleted")
	require.Nil(t, got.release, "a refused create must not hand back a reservation to release")
}

// The same window, but the delete is still RUNNING when repo resolution
// finishes. Its fence never went down, so this one must be caught by the live
// fence — and it must be caught before the backend resolvers, for the same
// reason the already-running case is: the create is already known to be
// impossible and must not wait on more unbounded git to be told so.
func TestReserveCreate_RefusesADeleteStillRunningAfterResolvingTheRepo(t *testing.T) {
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

	got := make(chan error, 1)
	go func() {
		_, _, release, _, err := manager.reserveCreate(CreateSessionRequest{
			RepoPath: repoPath, TitleBase: "resolved-mid-delete", Program: "claude",
		})
		if release != nil {
			release()
		}
		got <- err
	}()

	select {
	case err := <-got:
		require.Error(t, err, "a create must not be admitted while a delete holds this project's fence")
		require.ErrorContains(t, err, "being deleted")
	case <-time.After(20 * time.Second):
		close(releaseDelete)
		<-deleted
		t.Fatal("the create did not refuse while a delete held the fence")
	}

	close(releaseDelete)
	select {
	case err := <-deleted:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("the delete never completed")
	}
}

// A delete that finished BEFORE the create started is not an overlap, and must
// not be refused — otherwise every create in a project that was ever deleted and
// re-created would fail forever, which is a far worse bug than the one being
// fixed. This is the boundary the sequence comparison has to get exactly right:
// strictly-greater, not greater-or-equal.
func TestReserveCreate_AdmitsACreateAfterAFullyCompletedDelete(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	_, err := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.NoError(t, err)

	_, _, release, _, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath, TitleBase: "after-the-delete", Program: "claude",
	})
	if release != nil {
		release()
	}
	require.NoError(t, err,
		"a delete that completed before this create began is ordinary history, not a race: refusing it would strand every project that is ever deleted and re-created")
}

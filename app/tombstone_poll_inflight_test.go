package app

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// TestPreDeletePollInFlightRetainsTombstoneThroughDeleteCleanRefresh is the
// regression for the local-mode clean-clear interleave from #7522's review of
// f221d3d2: a background snapshot poll whose fetch began BEFORE the delete
// completes is still in flight when the delete's own synchronous clean refresh
// runs. Pre-fix that clean refresh called clearGoneProjectRows unconditionally
// and erased the tombstone at exactly the moment it was still needed, so the
// pre-delete poll landing afterward re-added the deleted repo as a session row
// with its live counts. The clear must retain the tombstone while a
// pre-mutation fetch is in flight, and the landing poll's observedAtSeq (which
// predates the tombstone) must keep it retained.
func TestPreDeletePollInFlightRetainsTombstoneThroughDeleteCleanRefresh(t *testing.T) {
	h := armGoneRowMergeHome(t, []overlay.Project{
		{Name: "doomed", Root: "/repos/doomed", RepoID: "repo-doomed",
			RegistryID: "prj_doomed", RegistryRoot: "/repos/doomed",
			RegistryCheckoutID: "chk_doomed", SessionCount: 2},
	})
	// A background snapshot poll is in flight — its fetchSnapshotCmd captured
	// the projectMutationSeq before the delete bumped it.
	preDeleteSeq := h.projectMutationSeq
	h.noteSnapshotFetchStarted(preDeleteSeq)

	// The confirmed delete completes on the loop: noteProjectDeleted records the
	// tombstone (bumping the seq past the in-flight poll's) and the synchronous
	// clean refresh installs the fresh list. Pre-fix the clean refresh cleared
	// the tombstone here; with the in-flight poll still outstanding it must stay.
	h.Update(projectDeletedMsg{root: "/repos/doomed", repoID: "repo-doomed", name: "doomed"})

	require.Equal(t, preDeleteSeq+1, h.projectMutationSeq,
		"the confirmed delete bumped the project-mutation sequence")
	require.NotEmpty(t, h.goneProjectRepoIDs,
		"the tombstone survives the delete's synchronous clean refresh while a pre-delete poll is in flight")

	// The pre-delete poll lands on a HEALTHY registry read, reporting the
	// deleted repo's former live sessions. With the tombstone retained its
	// pre-delete rows are filtered, and the tombstone stays (its observation
	// predates the delete).
	h.noteSnapshotFetchLanded(preDeleteSeq)
	stalePoll := []overlay.Project{
		{Name: "active", Root: "/repos/active", RepoID: "repo-active", SessionCount: 1},
		{Name: "doomed", Root: "/repos/doomed", RepoID: "repo-doomed", SessionCount: 2},
	}
	h.applySidebarProjects(stalePoll, false /* healthy registry */, nil, preDeleteSeq)
	assert.False(t, hasSidebarRow(h, "repo-doomed"),
		"a pre-delete poll's stale session rows must not re-enter the deleted repo")
	assert.True(t, hasSidebarRow(h, "repo-active"),
		"the poll's other rows still land — only the tombstoned repo is filtered")
	require.NotEmpty(t, h.goneProjectRepoIDs,
		"the tombstone is retained against the pre-mutation landing")

	// A later failed-snapshot apply must not preserve a doomed row the stale
	// poll never installed — there is nothing to preserve.
	h.applySidebarProjects(nil, false, errors.New("daemon unavailable"), h.projectMutationSeq)
	assert.False(t, hasSidebarRow(h, "repo-doomed"),
		"no doomed row exists for a later failed snapshot to preserve")
}

// TestPostDeleteCleanPollClearsTombstoneOnceNoFetchInFlight pins the
// complement: once no pre-mutation fetch remains in flight, a clean read
// clears the tombstone, so a genuine re-registration is not filtered as a
// ghost. This is the regression guard against the fix over-retaining the
// tombstone and breaking the original clear's purpose (#7522 review).
func TestPostDeleteCleanPollClearsTombstoneOnceNoFetchInFlight(t *testing.T) {
	h := armGoneRowMergeHome(t, []overlay.Project{
		{Name: "doomed", Root: "/repos/doomed", RepoID: "repo-doomed", SessionCount: 2},
	})
	h.noteProjectDeleted("repo-doomed")
	require.NotEmpty(t, h.goneProjectRepoIDs)
	// No poll in flight: the synchronous clean refresh clears the tombstone.
	h.refreshSidebarProjects()
	assert.Empty(t, h.goneProjectRepoIDs,
		"with no pre-mutation fetch in flight the clean refresh clears the tombstone")

	// A landing poll that reports live work for the deleted repo (a genuine
	// revival) installs the row: the tombstone is gone and the observation is
	// current, so nothing filters it.
	h.applySidebarProjects([]overlay.Project{
		{Name: "doomed", Root: "/repos/doomed", RepoID: "repo-doomed", SessionCount: 1},
	}, false, nil, h.projectMutationSeq)
	assert.True(t, hasSidebarRow(h, "repo-doomed"),
		"a revived row lands once the tombstone has been cleared")
}

// TestClearGoneProjectRowsGatesOnObservationSeqAndInFlightFetches exercises the
// tombstone clear's two gates directly: a landing poll whose observedAtSeq
// predates the tombstone never clears it, and a clean read never clears a
// tombstone while a pre-mutation fetch is still in flight — even one captured
// at a different seq (the rebind conflict snapshot is a second off-loop fetch).
func TestClearGoneProjectRowsGatesOnObservationSeqAndInFlightFetches(t *testing.T) {
	h := newTestHome(t)
	h.noteProjectDeleted("repo-doomed")
	tombSeq := h.projectMutationSeq // 1
	require.Equal(t, uint64(1), tombSeq)

	// A pre-mutation fetch (seq 0) in flight blocks the clean-read clear even
	// though the on-loop refresh's observedAtSeq (the current seq) covers the
	// tombstone.
	h.noteSnapshotFetchStarted(0)
	h.clearGoneProjectRows(h.projectMutationSeq)
	require.NotEmpty(t, h.goneProjectRepoIDs,
		"a pre-mutation fetch in flight retains the tombstone against the clean refresh")

	// The pre-mutation fetch lands but its observation predates the tombstone:
	// it never clears it.
	h.noteSnapshotFetchLanded(0)
	h.clearGoneProjectRows(0)
	require.NotEmpty(t, h.goneProjectRepoIDs,
		"a landing observation that predates the tombstone never clears it")

	// No fetch in flight and the observation covers the tombstone: clear.
	h.clearGoneProjectRows(h.projectMutationSeq)
	assert.Empty(t, h.goneProjectRepoIDs,
		"a post-mutation observation with no fetch in flight clears the tombstone")
}

// TestRemotePreDeletePollDoesNotRePoisonTheCacheAfterDelete is the regression
// for the remote-mode amplification described in the #7522 review of f221d3d2:
// remoteCache.apply applied the project channel before and independently of
// the readSeq != c.seq gate, so a pre-delete poll carrying the deleted record
// re-poisoned the cache after noteProjectRemoved cleaned it, and every later
// degraded poll re-emitted the record from that poisoned cache. The fix bumps
// c.seq in noteProjectRemoved AND gates the project channel under the same
// comparison, so a poll that began before the delete cannot overwrite the
// cleaned cache.
func TestRemotePreDeletePollDoesNotRePoisonTheCacheAfterDelete(t *testing.T) {
	stubRemote(t)
	failOnDial(t)
	h := newTestHome(t)
	h.repoRoot = "/srv/box/alpha"
	h.repoID = "aaaaaaaaaaaa"
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return nil, nil
	}))

	alpha := config.Project{
		ID: "prj_0123456789abcdef0123456789abcdef", Root: "/srv/box/alpha",
		RepoID: "aaaaaaaaaaaa", CheckoutID: "chk_a", PathExists: true,
	}
	beta := config.Project{
		ID: "prj_fedcba9876543210fedcba9876543210", Root: "/srv/box/beta",
		RepoID: "bbbbbbbbbbbb", CheckoutID: "chk_b", PathExists: true,
	}
	h.remote.applyFresh(&remoteReads{projects: []config.Project{alpha, beta}})

	// A remote poll started before the delete, capturing remoteSeq = c.seq.
	pollSeq := h.remote.seq
	staleRead := &remoteReads{projects: []config.Project{alpha, beta}}

	// The confirmed delete completes in remote mode: noteProjectRemoved drops
	// beta from the cache and bumps c.seq so the in-flight poll's older reads
	// cannot re-poison it.
	h.Update(projectDeletedMsg{root: "/srv/box/beta", repoID: "bbbbbbbbbbbb", name: "beta"})
	require.Equal(t, pollSeq+1, h.remote.seq,
		"a confirmed remote delete bumps the remote cache sequence")

	// The pre-delete poll lands: its reads still list beta. apply must refuse to
	// overwrite the cache (readSeq < c.seq) — pre-fix it re-poisoned it.
	h.remote.apply(staleRead, pollSeq)
	for _, p := range h.remote.projects {
		assert.NotEqual(t, beta.ID, p.ID,
			"a pre-delete poll must not re-poison the registry cache the delete cleaned: %v", p)
		assert.NotEqual(t, "bbbbbbbbbbbb", p.RepoID, "the deleted record stays out of the cache: %v", p)
	}

	// The sidebar, refreshed from the clean cache, must not re-emit beta.
	h.refreshSidebarProjectsFromSnapshot(nil, nil, nil, h.projectMutationSeq)
	for _, r := range h.sidebarProjects {
		assert.NotEqual(t, "bbbbbbbbbbbb", r.RepoID,
			"the deleted project must not re-enter the sidebar from the clean cache: %v", r)
		assert.NotEqual(t, beta.ID, r.RegistryID,
			"the deleted record must not re-emit into the merge shadow: %v", r)
	}
	for _, r := range h.projects.Projects() {
		assert.NotEqual(t, "bbbbbbbbbbbb", r.RepoID,
			"the deleted project must not stay a selectable sidebar row: %v", r)
	}
}

// TestRemoteProjectChannelAppliesOnlyForCurrentSeq pins the gate's
// post-mutation behaviour so the cache-poisoning fix does not strand the
// registry channel: a poll that began after the acknowledged mutation
// (readSeq == c.seq) still applies its projects, and a pre-mutation poll
// (readSeq < c.seq) does not — for the project channel exactly as for tasks.
func TestRemoteProjectChannelAppliesOnlyForCurrentSeq(t *testing.T) {
	stubRemote(t)
	c := &remoteCache{}

	// A post-mutation poll applies the project channel.
	c.apply(&remoteReads{projects: []config.Project{{ID: "prj_a", RepoID: "r1"}}}, c.seq)
	require.Len(t, c.projects, 1)
	require.True(t, c.projectsLoaded)
	require.Equal(t, "prj_a", c.projects[0].ID)

	// A task mutation bumps c.seq; a poll that began before it (stale) must not
	// re-poison the project channel either, matching the task channel.
	c.noteTaskStored(taskRowKey{project: "/p", id: "t"}, task.Task{ID: "t"})
	staleSeq := c.seq - 1
	c.apply(&remoteReads{projects: []config.Project{{ID: "prj_b", RepoID: "r2"}}}, staleSeq)
	assert.Len(t, c.projects, 1, "a stale poll does not re-poison the project channel")
	assert.Equal(t, "prj_a", c.projects[0].ID, "the cleaned cache holds the newer read")

	// A poll that began after the acknowledgement applies again.
	c.apply(&remoteReads{projects: []config.Project{{ID: "prj_c", RepoID: "r3"}}}, c.seq)
	assert.Equal(t, "prj_c", c.projects[0].ID, "a current poll applies its projects")
}

// TestSnapshotPollRetiresFromInFlightSetWhenItLands guards the wiring that
// pairs noteSnapshotFetchStarted (in fetchSnapshotCmd) with
// noteSnapshotFetchLanded (in the snapshotFetchedMsg handler): a poll that
// lands must retire from the in-flight set, or the tombstone clear would
// over-retain forever and the re-registration revival would never recover.
func TestSnapshotPollRetiresFromInFlightSetWhenItLands(t *testing.T) {
	h := armGoneRowMergeHome(t, nil)
	h.snapshotFetcher = func(_ context.Context, _ string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	// Drive the real poll cycle: fetchSnapshotCmd registers the in-flight fetch,
	// the message handler retires it.
	msg, ok := h.fetchSnapshotCmd()().(snapshotFetchedMsg)
	require.True(t, ok)
	require.NotEmpty(t, h.pendingSnapshotFetchSeqs,
		"fetchSnapshotCmd records the in-flight fetch")
	_, _ = h.Update(msg)
	assert.Empty(t, h.pendingSnapshotFetchSeqs,
		"the snapshotFetchedMsg handler retires the in-flight fetch")

	// With the in-flight set empty, a subsequent delete's clean refresh clears
	// the tombstone (no stale in-flight entry over-retains it).
	h.noteProjectDeleted("repo-doomed")
	h.refreshSidebarProjects()
	assert.Empty(t, h.goneProjectRepoIDs,
		"no stale in-flight entry keeps the clean refresh from clearing the tombstone")
}

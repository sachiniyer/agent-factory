package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// armGoneRowMergeHome returns a home whose Projects section is seeded with the
// given rows (m.sidebarProjects is the merge's preserve source), with the
// cross-repo session fetcher stubbed to a successful EMPTY fetch — the session
// snapshot lands while the registry read is what degrades. Active scope is a
// repo distinct from the ones the test mutates.
func armGoneRowMergeHome(t *testing.T, rows []overlay.Project) *home {
	t.Helper()
	h := newTestHome(t)
	h.repoRoot = "/repos/active"
	h.repoID = "repo-active"
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return nil, nil
	}))
	h.setSidebarProjects(append([]overlay.Project{
		{Name: "active", Root: "/repos/active", RepoID: "repo-active", SessionCount: 1},
	}, rows...), false)
	return h
}

// degradeProjectRegistry breaks the strict registry enumeration the sync
// refresh performs, with a stray file under the registry dir — the exact
// unrelated-file degradation the Codex threads describe: ListProjects refuses
// the directory while the per-record reads the mutations themselves use
// tolerate it.
func degradeProjectRegistry(t *testing.T) {
	t.Helper()
	dir, err := config.ProjectRegistryDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644))
}

// TestDeleteUnderDegradedRegistryDropsTheDeletedRow is the regression for
// Codex P2 4608287031 on #7522: a delete that SUCCEEDS while the strict
// registry read is degraded reports success while the merge restores the
// deleted project's old registry-backed row from m.sidebarProjects — the
// project stays visible and selectable until the registry is repaired. The
// session itself established this absence by performing the deletion, so the
// merge must not resurrect the row behind a read that cannot prove it.
func TestDeleteUnderDegradedRegistryDropsTheDeletedRow(t *testing.T) {
	h := armGoneRowMergeHome(t, []overlay.Project{
		{Name: "doomed", Root: "/repos/doomed", RepoID: "repo-doomed",
			RegistryID: "prj_doomed", RegistryRoot: "/repos/doomed",
			RegistryCheckoutID: "chk_doomed", SessionCount: 2},
	})
	degradeProjectRegistry(t)

	// The confirmed delete completes; its sync refresh hits the degraded read.
	h.Update(projectDeletedMsg{root: "/repos/doomed", repoID: "repo-doomed", name: "doomed"})

	for _, r := range h.sidebarProjects {
		assert.NotEqual(t, "repo-doomed", r.RepoID,
			"a confirmed delete must not leave the project's row in the merge shadow: %v", r)
		assert.NotEqual(t, "prj_doomed", r.RegistryID,
			"a confirmed delete must not leave the project's record in the merge shadow: %v", r)
	}
	for _, r := range h.projects.Projects() {
		assert.NotEqual(t, "repo-doomed", r.RepoID,
			"the deleted project must not stay a selectable sidebar row: %v", r)
		assert.NotEqual(t, "/repos/doomed", r.Root,
			"the deleted project's root must not stay a selectable sidebar row: %v", r)
	}
}

// TestRebindUnderDegradedRegistryDropsTheStaleBinding is the regression for
// Codex P2 4608287034 on #7522: a rebind that SUCCEEDS while the strict
// registry read is degraded reports success while the merge restores the old
// registry row with its previous root and checkout id — the section keeps a
// stale selectable binding, and renderedRebindFollow cannot correct it because
// the registry view carries the read's error. The session itself established
// the record's move by performing the rebind, so the merge must not resurrect
// the old identity behind a read that cannot prove it.
func TestRebindUnderDegradedRegistryDropsTheStaleBinding(t *testing.T) {
	h := armGoneRowMergeHome(t, []overlay.Project{
		{Name: "moved", Root: "/repos/moved-old", RepoID: "repo-moved",
			RegistryID: "prj_moved", RegistryRoot: "/repos/moved-old",
			RegistryCheckoutID: "chk_old", SessionCount: 0},
	})
	degradeProjectRegistry(t)

	// The confirmed rebind completes; its sync refresh hits the degraded read.
	h.Update(projectReboundMsg{
		token:           1,
		projectID:       "prj_moved",
		oldRepoID:       "repo-moved",
		oldRoot:         "/repos/moved-old",
		oldRegistryRoot: "/repos/moved-old",
		name:            "moved",
		root:            "/repos/moved-new",
	})

	for _, r := range h.sidebarProjects {
		assert.NotEqual(t, "prj_moved", r.RegistryID,
			"a confirmed rebind must not leave the record's stale identity in the merge shadow: %v", r)
		assert.NotEqual(t, "/repos/moved-old", r.RegistryRoot,
			"the rebound record's old registry root must not survive: %v", r)
		assert.NotEqual(t, "chk_old", r.RegistryCheckoutID,
			"the rebound record's old checkout id must not survive: %v", r)
	}
	for _, r := range h.projects.Projects() {
		assert.NotEqual(t, "repo-moved", r.RepoID,
			"a sessionless row whose record moved has nothing left to describe: %v", r)
		assert.NotEqual(t, "chk_old", r.CheckoutID,
			"the pane row must not keep the stale checkout id: %v", r)
	}
}

// hasSidebarRow reports whether the merge shadow currently holds a row for the
// given repo id — the merge's preserve source between snapshots.
func hasSidebarRow(h *home, repoID string) bool {
	for _, r := range h.sidebarProjects {
		if r.RepoID == repoID {
			return true
		}
	}
	return false
}

// TestRebindUnderHealthyRegistryReplacesTheStaleBindingRoot is the regression for
// Codex P2 4608778532 on #7522: a rebind that SUCCEEDS, then a sync refresh whose
// session fetch FAILS while the registry read is HEALTHY. The healthy read
// carries the record's new binding, but the pre-fix rebind tombstone stripped
// the old row's registry identity before the merge, so MergePreservingSessionRows
// treated it as a session-only row and coalesceReboundRows grafted the fresh
// registration onto it without updating its navigable Root — the sidebar kept
// selecting the prior, potentially-removed worktree until a session snapshot
// succeeded. The tombstone must apply only while the registry cannot establish
// the binding; a healthy read replaces the row through the merge instead.
func TestRebindUnderHealthyRegistryReplacesTheStaleBindingRoot(t *testing.T) {
	h := armGoneRowMergeHome(t, []overlay.Project{
		{Name: "moved", Root: "/repos/moved-old", RepoID: "repo-moved",
			RegistryID: "prj_moved", RegistryRoot: "/repos/moved-old",
			RegistryCheckoutID: "chk_old", SessionCount: 3, InPlaceCount: 1},
	})
	// The confirmed rebind tombstoned the record; the registry read is HEALTHY
	// and carries the new binding, but the session snapshot failed.
	h.noteProjectRebound("prj_moved", "repo-moved")
	fresh := []overlay.Project{
		{Name: "moved", Root: "/repos/moved-new", RepoID: "repo-moved",
			RegistryID: "prj_moved", RegistryRoot: "/repos/moved-new",
			RegistryCheckoutID: "chk_new", SessionCount: 0},
	}
	h.applySidebarProjects(fresh, false, errors.New("session fetch failed"), h.projectMutationSeq)

	var row *overlay.Project
	for i := range h.sidebarProjects {
		if h.sidebarProjects[i].RepoID == "repo-moved" {
			row = &h.sidebarProjects[i]
		}
	}
	require.NotNil(t, row, "the rebound repo's row survives the failed session fetch")
	assert.Equal(t, "/repos/moved-new", row.Root,
		"a healthy registry read replaces the stale binding's root — the pre-fix tombstone stripped provenance and left the old worktree selectable")
	assert.Equal(t, "prj_moved", row.RegistryID,
		"the healthy merge keeps the registration's durable identity")
	assert.Equal(t, "chk_new", row.RegistryCheckoutID,
		"the healthy merge replaces the stale checkout id")
	assert.Equal(t, 3, row.SessionCount,
		"the failed snapshot's last-known counts carry onto the fresh row")
}

// TestDeleteTombstoneRetiresWhenLiveSessionsReturn is the regression for Codex
// P2 4608778541 on #7522: a delete tombstone, otherwise cleared only by a
// healthy registry read, would survive a STILL-degraded registry and filter a
// revived repo's row on the next failed snapshot. Restoring an archived session
// or creating a new session for the deleted repo legitimately reintroduces its
// row under a degraded registry; a landed snapshot that reports live work must
// retire the tombstone so the revived row survives a later snapshot hiccup.
func TestDeleteTombstoneRetiresWhenLiveSessionsReturn(t *testing.T) {
	h := armGoneRowMergeHome(t, nil)
	h.noteProjectDeleted("repo-doomed")

	// A landed session snapshot under the STILL-degraded registry reports live
	// work in the deleted repo — the session data establishes the project is
	// live again, and retires the deletion tombstone.
	revived := []overlay.Project{
		{Name: "doomed", Root: "/repos/doomed", RepoID: "repo-doomed", SessionCount: 1},
	}
	h.applySidebarProjects(revived, true, nil, h.projectMutationSeq)
	require.True(t, hasSidebarRow(h, "repo-doomed"),
		"the revived repo's row appears on the landed snapshot")

	// A subsequent FAILED snapshot must not drop the revived row — the
	// tombstone was retired by the live session observation, so the preserving
	// merge keeps the last-known row.
	h.applySidebarProjects(nil, true, errors.New("daemon unavailable"), h.projectMutationSeq)
	assert.True(t, hasSidebarRow(h, "repo-doomed"),
		"a retired tombstone must not filter the revived row on a later failed snapshot")
}

// TestRemoteDegradedRegistryDoesNotReemitDeletedProject is the regression for
// Codex P2 4608848747 on #7522: in remote-target mode the registry rows come
// from remoteCache, which a failed poll does not advance — so after a
// confirmed delete the cache keeps the last-good record and buildRemoteProjectList
// re-emits it in the INCOMING projects slice (degraded). Filtering only the
// retained `old` set cannot stop the stale record landing from the incoming
// one; the confirmed deletion must remove the record from the cache itself,
// exactly like noteTaskRemoved advances the task cache.
func TestRemoteDegradedRegistryDoesNotReemitDeletedProject(t *testing.T) {
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

	// The confirmed delete completes in remote mode: the registry record the
	// cache still holds is the source every degraded poll re-emits from.
	h.Update(projectDeletedMsg{root: "/srv/box/beta", repoID: "bbbbbbbbbbbb", name: "beta"})

	// The next registry poll fails — the cache keeps its last-good set, which
	// must no longer include the deleted project, so the degraded incoming
	// rows cannot re-emit it either.
	h.remote.applyFresh(&remoteReads{projectsErr: errors.New("rpc failed")})
	h.refreshSidebarProjectsFromSnapshot(nil, nil, nil, h.projectMutationSeq)

	for _, p := range h.remote.projects {
		assert.NotEqual(t, beta.ID, p.ID,
			"a confirmed delete must drop the record the cache re-emits on every degraded poll: %v", p)
	}
	for _, r := range h.sidebarProjects {
		assert.NotEqual(t, "bbbbbbbbbbbb", r.RepoID,
			"the deleted project must not stay a merged row in remote mode: %v", r)
		assert.NotEqual(t, beta.ID, r.RegistryID,
			"the deleted project's record must not re-emit into the merge shadow: %v", r)
	}
	for _, r := range h.projects.Projects() {
		assert.NotEqual(t, "bbbbbbbbbbbb", r.RepoID,
			"the deleted project must not stay a selectable sidebar row: %v", r)
	}
}

// TestDeleteTombstoneRejectsPreDeletePollRevival is the regression for Codex
// P2 4608848751 on #7522: an in-flight poll whose fetchSnapshotCmd began BEFORE
// the delete completed can land after it carrying the repo's former live
// sessions — a pre-delete observation, not a revival. Retirement must honor
// only a poll known to have started after the deletion (its captured mutation
// sequence covers the tombstone's), or the stale row comes back and a later
// failed poll then preserves it.
func TestDeleteTombstoneRejectsPreDeletePollRevival(t *testing.T) {
	h := armGoneRowMergeHome(t, []overlay.Project{
		{Name: "doomed", Root: "/repos/doomed", RepoID: "repo-doomed",
			RegistryID: "prj_doomed", RegistryRoot: "/repos/doomed",
			RegistryCheckoutID: "chk_doomed", SessionCount: 2},
	})
	// The poll was created before the delete completed: the sequence it
	// captured is the one the tombstone then bumps past.
	preDeleteSeq := h.projectMutationSeq
	h.noteProjectDeleted("repo-doomed")

	// The stale poll lands under a degraded registry, reporting the deleted
	// repo's former live sessions. It must NOT retire the tombstone, and its
	// pre-delete session row must not land either.
	stalePoll := []overlay.Project{
		{Name: "active", Root: "/repos/active", RepoID: "repo-active", SessionCount: 1},
		{Name: "doomed", Root: "/repos/doomed", RepoID: "repo-doomed", SessionCount: 2},
	}
	h.applySidebarProjects(stalePoll, true, nil, preDeleteSeq)
	assert.False(t, hasSidebarRow(h, "repo-doomed"),
		"a poll that began before the delete carries pre-delete rows — none may land for the deleted repo")
	assert.True(t, hasSidebarRow(h, "repo-active"),
		"the poll's other rows still land — only the tombstoned repo is filtered")

	// A genuinely post-delete poll reporting live work DOES retire the
	// tombstone: the revived row lands and then survives a failed snapshot.
	revived := []overlay.Project{
		{Name: "active", Root: "/repos/active", RepoID: "repo-active", SessionCount: 1},
		{Name: "doomed", Root: "/repos/doomed", RepoID: "repo-doomed", SessionCount: 1},
	}
	h.applySidebarProjects(revived, true, nil, h.projectMutationSeq)
	assert.True(t, hasSidebarRow(h, "repo-doomed"),
		"a poll that began after the delete establishes the revival")
	h.applySidebarProjects(nil, true, errors.New("daemon unavailable"), h.projectMutationSeq)
	assert.True(t, hasSidebarRow(h, "repo-doomed"),
		"the retired tombstone leaves the revived row preserved through a hiccup")
}

// TestRebindDegradedThenHealthyReplacesStrippedRowRoot is the regression for
// Codex P2 4608848761 on #7522: a rebind tombstone strips the old row's
// registry identity while the registry stays degraded, leaving a session-only
// row at the OLD root. When the registry then recovers during a session-fetch
// failure, that stripped row is indistinguishable from an ordinary session
// row — the coalesce grafts the recovered registration onto it WITHOUT
// replacing its navigable Root, so the sidebar keeps opening the old worktree.
// The tombstone retains the record's provenance precisely so a clean read can
// re-attach it and let the merge replace the row wholesale.
func TestRebindDegradedThenHealthyReplacesStrippedRowRoot(t *testing.T) {
	h := armGoneRowMergeHome(t, []overlay.Project{
		{Name: "moved", Root: "/repos/moved-old", RepoID: "repo-moved",
			RegistryID: "prj_moved", RegistryRoot: "/repos/moved-old",
			RegistryCheckoutID: "chk_old", SessionCount: 2, InPlaceCount: 1},
	})
	h.noteProjectRebound("prj_moved", "repo-moved")

	// The completion's refresh hits the degraded registry + a failed snapshot:
	// the stale binding is stripped and the repo keeps a session-only row at
	// the OLD root.
	h.applySidebarProjects(nil, true, errors.New("session fetch failed"), h.projectMutationSeq)
	require.True(t, hasSidebarRow(h, "repo-moved"),
		"live sessions keep a session-only row while the read cannot prove the record")

	// The registry then recovers while the snapshot stays failed: the stripped
	// row must be replaced by the record's CURRENT binding — navigable Root
	// included — rather than graft the registration onto the stale row.
	h.applySidebarProjects([]overlay.Project{
		{Name: "moved", Root: "/repos/moved-new", RepoID: "repo-moved",
			RegistryID: "prj_moved", RegistryRoot: "/repos/moved-new",
			RegistryCheckoutID: "chk_new", SessionCount: 0},
	}, false, errors.New("session fetch failed"), h.projectMutationSeq)

	var row *overlay.Project
	for i := range h.sidebarProjects {
		if h.sidebarProjects[i].RepoID == "repo-moved" {
			row = &h.sidebarProjects[i]
		}
	}
	require.NotNil(t, row, "the rebound repo's row survives on the record's current binding")
	assert.Equal(t, "/repos/moved-new", row.Root,
		"the recovered binding's root replaces the stripped row's stale one")
	assert.Equal(t, "prj_moved", row.RegistryID)
	assert.Equal(t, "chk_new", row.RegistryCheckoutID)
	assert.Equal(t, 2, row.SessionCount,
		"the failed snapshot's last-known counts graft onto the recovered row")
	assert.Equal(t, 1, row.InPlaceCount)
}

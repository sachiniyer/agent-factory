package app

import (
	"context"
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

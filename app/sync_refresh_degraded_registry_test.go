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
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// TestSyncRefreshDegradedRegistryKeepsRegistryRows pins the synchronous refresh
// funnel (refreshSidebarProjectsResolving — launch, project switch, and
// add/delete/rebind completion) against the mirror-image degraded state the poll
// path (TestProjectsSection_DegradedRegistryKeepsRegistryRows) and the
// rebind-conflict path (TestRebindConflictWithADegradedRegistryKeepsRegistryRows)
// already merge through: the session snapshot LANDED but the registry read
// degraded. The fresh list then carries live session rows and no registry
// records, so installing it wholesale would drop every registered project that
// has no live sessions behind a read that cannot prove absence — and overwrite
// the merge shadow a later failed-snapshot poll reads from. The funnel must
// route that state through applySidebarProjects so the registry-backed rows
// survive, the landed snapshot still replaces the session half at once, and the
// shadow the next poll merges from keeps the durable identity.
func TestSyncRefreshDegradedRegistryKeepsRegistryRows(t *testing.T) {
	h := newTestHome(t)
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}
	// repo B is a genuinely registered checkout with NO live sessions — the row
	// only the durable registry supplies, and the one a degraded read cannot
	// prove absent.
	repoB := initTestGitRepo(t)
	rec, err := config.RegisterProject(repoB)
	require.NoError(t, err)
	// The active project is a different, UNREGISTERED checkout, so its row is
	// session-derived (it always appears via buildProjectListFromView seeding
	// m.repoRoot) and does not muddy the registry-backed row's preservation.
	h.repoRoot = initTestGitRepo(t)
	require.NotEqual(t, repoB, h.repoRoot)
	h.repoID = config.RepoIDFromRoot(h.repoRoot)
	// The session snapshot answers cleanly and reports NO live sessions, so the
	// snapshot half lands (snapshotErr == nil) on every refresh below and repo B
	// is registry-only — exactly the asymmetric failure the bug report names.
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return nil, nil
	}))

	rowsByRoot := func() map[string]ui.SidebarProject {
		out := make(map[string]ui.SidebarProject, len(h.projects.Projects()))
		for _, r := range h.projects.Projects() {
			out[r.Root] = r
		}
		return out
	}
	shadowByRoot := func() map[string]overlay.Project {
		out := make(map[string]overlay.Project, len(h.sidebarProjects))
		for _, r := range h.sidebarProjects {
			out[r.Root] = r
		}
		return out
	}

	// Stage 1: a healthy synchronous refresh lists repo B from the registry and
	// records its identity in the merge shadow.
	h.refreshSidebarProjects()
	rows := rowsByRoot()
	require.Contains(t, rows, repoB,
		"a healthy sync refresh lists the registered sessionless project")
	assert.False(t, h.projects.Degraded(), "a healthy registry read is not degraded")
	require.Contains(t, shadowByRoot(), repoB,
		"the merge shadow records the registered sessionless row")
	assert.Equal(t, rec.ID, shadowByRoot()[repoB].RegistryID,
		"the registry identity is recorded in the shadow")

	// Stage 2: corrupt the registry exactly as TestBuildProjectListReportsDegradedRegistry
	// does — the snapshot still succeeds, so snapshotErr stays nil and the
	// pre-fix funnel took the wholesale-install branch.
	dir, err := config.ProjectRegistryDir()
	require.NoError(t, err)
	stray := filepath.Join(dir, "stray")
	require.NoError(t, os.WriteFile(stray, []byte("not a record"), 0o644))
	_, err = config.ListProjects()
	require.Error(t, err, "fixture: the strict registry read must fail")

	h.refreshSidebarProjects()

	// The registered sessionless row survives in BOTH the visible rows and the
	// merge shadow — the pre-fix wholesale install wiped both.
	rows = rowsByRoot()
	require.Contains(t, rows, repoB,
		"a degraded registry read cannot prove absence — the registered sessionless row stays in the visible list")
	assert.Equal(t, 0, rows[repoB].SessionCount,
		"the landed session snapshot is the count authority — an uncovered repo's tally clears (#4888 review)")
	assert.True(t, h.projects.Degraded(),
		"the degraded badge still renders over the preserved rows")
	shadow := shadowByRoot()
	require.Contains(t, shadow, repoB,
		"the merge shadow keeps the preserved row so a later failed-snapshot poll can restore it")
	assert.Equal(t, rec.ID, shadow[repoB].RegistryID,
		"the preserved row keeps its registration identity for the rebind affordance")

	// Stage 3: a subsequent poll whose SNAPSHOT half fails (the real poll path
	// refreshSidebarProjectsFromSnapshot) cannot resurrect a row the wholesale
	// install would have wiped — the merge reads the shadow stage 2 just
	// preserved. Without the fix stage 2 overwrote that shadow, so stage 3
	// lost the row for as long as the registry stayed unreadable.
	h.refreshSidebarProjectsFromSnapshot(nil, nil, errors.New("daemon unavailable"))
	rows = rowsByRoot()
	require.Contains(t, rows, repoB,
		"the failed-snapshot poll merges from a shadow that still carries the registered row")
	assert.True(t, h.projects.Degraded(),
		"the registry is still unreadable — the degraded badge stays")
	require.Contains(t, shadowByRoot(), repoB,
		"the shadow still carries the registered row through the failed-snapshot poll")

	// Stage 4: healing the registry read restores the wholesale install (no
	// merge) with the registered sessionless row and clears the badge — the fix
	// did not strand stale rows on recovery.
	require.NoError(t, os.Remove(stray))
	h.refreshSidebarProjects()
	rows = rowsByRoot()
	require.Contains(t, rows, repoB,
		"registry recovery restores the registered sessionless row via the non-merge path")
	assert.False(t, h.projects.Degraded(),
		"the degraded badge clears once the registry reads again")
	assert.Equal(t, rec.ID, shadowByRoot()[repoB].RegistryID,
		"the recovered row carries its registration identity")
}

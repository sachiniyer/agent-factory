package app

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// TestRemoteDegradedRegistryPreservesLiveSessionCounts pins the remote-mode bug
// #5833 introduced over #4888's merge: the TUI sidebar's Projects section
// refreshes on the background poll from two independent daemon reads — the
// cross-repo session snapshot (live counts) and the project registry
// (registration identity). When the registry read degrades but the snapshot
// lands, the merge zeroes the preserved rows' counts on the premise that a
// fresh session row reclaims them and gets the live counts grafted back. In
// LOCAL mode the fresh row carries no RegistryID (a failed config.ListProjects
// yields no identity), so it takes the RegistryID-less graft whose guard
// honours the live counts. In REMOTE mode buildRemoteProjectList stamps a
// RegistryID on the fresh row from the stale cached registry even while
// degraded, so the fresh row takes the RegistryID-match branch — which (before
// the fix) unconditionally copied the just-zeroed preserved counts over the
// fresh row's live counts, making the sidebar read (0) for every registered
// project for the life of the outage and persisting the zeroed set into
// m.sidebarProjects so every poll repeated it.
//
// Driven through refreshSidebarProjectsFromSnapshot — the real production path
// whose applySidebarProjects runs registryBackedRows + MergePreservingSessionRows
// and writes the merged set back into m.sidebarProjects — so the test exercises
// both the visible pane rows and the persisted shadow the next tick merges from.
func TestRemoteDegradedRegistryPreservesLiveSessionCounts(t *testing.T) {
	stubRemote(t)
	failOnDial(t)
	h := newTestHome(t)
	// The active project is the registered one with live sessions, the realistic
	// shape: buildRemoteProjectList ensures (m.repoID, m.repoRoot) matches the
	// registry's own alpha row so a single row describes it.
	h.repoID = "aaaaaaaaaaaa"
	h.repoRoot = "/srv/box/alpha"

	registryAlpha := config.Project{
		ID: "prj_0123456789abcdef0123456789abcdef", Root: "/srv/box/alpha",
		RepoID: "aaaaaaaaaaaa", CheckoutID: "chk_a", PathExists: true,
	}
	registryBeta := config.Project{
		ID: "prj_fedcba9876543210fedcba9876543210", Root: "/srv/box/beta",
		RepoID: "bbbbbbbbbbbb", CheckoutID: "chk_b", PathExists: true,
	}

	// The healthy cross-repo snapshot: two live sessions at /srv/box/alpha (one
	// an external/in-place worktree), none at /srv/box/beta. This same snapshot
	// is re-fed on the degraded tick — exactly the asymmetric failure (snapshot
	// lands, registry RPC fails) the bug report describes.
	snapshot := []session.InstanceData{
		{Worktree: session.GitWorktreeData{RepoPath: "/srv/box/alpha"}},
		{Worktree: session.GitWorktreeData{RepoPath: "/srv/box/alpha", ExternalWorktree: true}},
	}

	visibleByRepoID := func() map[string]ui.SidebarProject {
		out := map[string]ui.SidebarProject{}
		for _, r := range h.projects.Projects() {
			out[r.RepoID] = r
		}
		return out
	}
	shadowByRepoID := func() map[string]overlay.Project {
		out := map[string]overlay.Project{}
		for _, r := range h.sidebarProjects {
			out[r.RepoID] = r
		}
		return out
	}

	// Healthy tick: the registry read lands, so the section takes the fresh rows
	// wholesale (no merge). alpha reports its two live sessions; beta is present
	// with none.
	h.remote.applyFresh(&remoteReads{projects: []config.Project{registryAlpha, registryBeta}})
	require.True(t, h.refreshSidebarProjectsFromSnapshot(snapshot, nil),
		"the healthy tick changes the visible rows")
	rows := visibleByRepoID()
	require.Contains(t, rows, "aaaaaaaaaaaa")
	require.Equal(t, 2, rows["aaaaaaaaaaaa"].SessionCount, "the healthy snapshot's live count for alpha")
	require.Equal(t, 1, rows["aaaaaaaaaaaa"].InPlaceCount, "the in-place split is preserved")
	require.Equal(t, "prj_0123456789abcdef0123456789abcdef", shadowByRepoID()["aaaaaaaaaaaa"].RegistryID,
		"the registration identity is recorded in the shadow")
	require.Contains(t, rows, "bbbbbbbbbbbb")
	require.Zero(t, rows["bbbbbbbbbbbb"].SessionCount, "beta has no live sessions")

	// Degraded tick: the registry read fails (the cache keeps the last-good rows,
	// the realistic remoteCache.apply behaviour) while the SAME healthy snapshot
	// is re-fed. Before the fix, the merge zeroed alpha's count on the way back
	// into m.sidebarProjects and the sidebar read (0); it must now keep the live
	// count the snapshot reported.
	h.remote.applyFresh(&remoteReads{projectsErr: errors.New("rpc failed")})
	require.NotNil(t, h.remote.projectsErr, "the cache records the degraded registry read")
	require.True(t, h.refreshSidebarProjectsFromSnapshot(snapshot, nil),
		"the degraded tick still refreshes from the live snapshot")
	rows = visibleByRepoID()
	require.Equal(t, 2, rows["aaaaaaaaaaaa"].SessionCount,
		"the degraded registry read must not zero the live count the healthy snapshot reported")
	require.Equal(t, 1, rows["aaaaaaaaaaaa"].InPlaceCount,
		"the in-place count is preserved too")
	// The persisted shadow carries the live counts, so the next tick merges from
	// correct data rather than re-zeroing every poll — the bug's compounding half.
	shadow := shadowByRepoID()
	require.Equal(t, 2, shadow["aaaaaaaaaaaa"].SessionCount,
		"the live counts are persisted into the shadow the next tick merges from")
	require.Equal(t, "prj_0123456789abcdef0123456789abcdef", shadow["aaaaaaaaaaaa"].RegistryID,
		"the registration identity survives the degraded merge")

	// beta was uncovered by the snapshot: the landed snapshot is the count
	// authority, so its tally clears — the fix must not resurrect last-known
	// counts for a repo the fresh snapshot proved has none. The registry row
	// itself stays (absence unproven under a degraded read).
	require.Contains(t, rows, "bbbbbbbbbbbb",
		"absence unproven under a degraded read — the registry row stays")
	require.Zero(t, rows["bbbbbbbbbbbb"].SessionCount,
		"an uncovered repo's tally clears — the snapshot is the count authority")

	// A second degraded tick with the same snapshot must still show the live
	// counts: the bug compounded because the zeroed shadow became the next
	// tick's old. After the fix the shadow carries the truth, so a sustained
	// outage does not decay the counts over repeated polls. The refresh's own
	// change-flag is a repaint hint (steady-state rows did not change), not the
	// contract — the row contents are.
	h.refreshSidebarProjectsFromSnapshot(snapshot, nil)
	rows = visibleByRepoID()
	require.Equal(t, 2, rows["aaaaaaaaaaaa"].SessionCount,
		"a sustained degraded registry does not decay the live count over repeated polls")
	require.Equal(t, 1, rows["aaaaaaaaaaaa"].InPlaceCount)

	// Recovery: the registry reads again and the section returns to the wholesale
	// install (no merge). Confirms the fix did not break the healthy path or strand
	// stale rows on recovery.
	h.remote.applyFresh(&remoteReads{projects: []config.Project{registryAlpha, registryBeta}})
	h.refreshSidebarProjectsFromSnapshot(snapshot, nil)
	rows = visibleByRepoID()
	require.Equal(t, 2, rows["aaaaaaaaaaaa"].SessionCount,
		"registry recovery restores the non-merge path with the live counts intact")
	require.Equal(t, "prj_0123456789abcdef0123456789abcdef", shadowByRepoID()["aaaaaaaaaaaa"].RegistryID)
}

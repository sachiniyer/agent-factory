package app

import (
	"context"
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// #7180: the sidebar Projects section's `D` opens the same delete-project
// confirmation as the ctrl+p picker, and that confirmation is built from the
// counts on the section's rows. The picker refuses `D` when the snapshot behind
// its counts failed (#1973/#1985); the section did not, so a failed or stale
// snapshot let the dialog claim counts nobody observed — down to "It has no
// live sessions — it just leaves the projects list" while the daemon archived
// and tore down every live session. The property pinned here: the delete
// confirmation never opens on counts the latest snapshot did not observe.

// sidebarDeleteDecoyRoot is a second project, so the Projects section holds
// two rows — the grid hides it at one row, which would leave it unfocusable.
const sidebarDeleteDecoyRoot = "/tmp/af-delete-project-decoy/zzz-decoy"

// sidebarDeleteSnapshot is the healthy cross-repo snapshot: the active project
// runs one worktree session and one in-place session, beside a decoy project.
func sidebarDeleteSnapshot() []session.InstanceData {
	decoy := session.InstanceData{
		Title: "decoy",
		Worktree: session.GitWorktreeData{
			RepoPath: sidebarDeleteDecoyRoot, WorktreePath: sidebarDeleteDecoyRoot, SessionName: "decoy",
		},
	}
	return []session.InstanceData{
		deleteProjectSession("alpha", false),
		deleteProjectSession("beta", true),
		decoy,
	}
}

// newSidebarDeleteHome stages a home scoped to deleteProjectTestRoot whose
// Projects section was filled by a HEALTHY fetch, exactly as launch does.
func newSidebarDeleteHome(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	h.repoRoot = deleteProjectTestRoot
	h.repoID = config.RepoIDFromRoot(deleteProjectTestRoot)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return sidebarDeleteSnapshot(), nil
	}))
	resizeHome(h, 120, 45)
	h.refreshSidebarProjects()
	h.relayout()
	require.Len(t, h.projects.Projects(), 2, "precondition: the active project and the decoy")
	row := sidebarDeleteActiveRow(t, h)
	require.Equal(t, 2, row.SessionCount, "precondition: a healthy fetch counts both live sessions")
	require.Equal(t, 1, row.InPlaceCount, "precondition: a healthy fetch counts the in-place session")
	return h
}

// sidebarDeleteActiveRow returns the Projects section's row for the active
// project — the row whose counts the confirmation would read.
func sidebarDeleteActiveRow(t *testing.T, h *home) ui.SidebarProject {
	t.Helper()
	for _, r := range h.projects.Projects() {
		if r.RepoID == h.repoID {
			return r
		}
	}
	t.Fatalf("the Projects section lost the active project's row: %+v", h.projects.Projects())
	return ui.SidebarProject{}
}

// pressSidebarDelete focuses the Projects section, rests the cursor on the
// active project's row and presses the real delete-project key there.
func pressSidebarDelete(t *testing.T, h *home) *home {
	t.Helper()
	h.relayout()
	h.focusRegion(layout.RegionProjects)
	require.Equal(t, layout.RegionProjects, h.ring.Active(), "precondition: the Projects section holds focus")
	proj, ok := h.projects.SelectedProject()
	require.True(t, ok)
	require.Equal(t, h.repoID, proj.RepoID, "precondition: the cursor rests on the active project")
	model, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	require.True(t, consumed, "`D` on the focused Projects section routes to the delete verb")
	return model.(*home)
}

// failAllReposFetch makes every later cross-repo snapshot fetch fail.
func failAllReposFetch(t *testing.T) {
	t.Helper()
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return nil, errors.New("daemon unavailable")
	}))
}

// assertSidebarDeleteRefused asserts that `D` opened no consent dialog and
// told the user why: the live sessions could not be checked.
func assertSidebarDeleteRefused(t *testing.T, h *home) {
	t.Helper()
	require.NotEqual(t, stateConfirm, h.state,
		"D must not open the delete confirmation on counts the latest snapshot did not observe")
	require.Nil(t, h.confirmationOverlay,
		"no consent dialog may be built from unobserved counts")
	notice := h.errBox.FullError()
	require.NotEmpty(t, notice, "the refusal must reach the user, not be a silent no-op")
	assert.Contains(t, notice, "cannot delete project", "the notice names the refused verb")
	assert.Contains(t, notice, "could not check live sessions",
		"the notice says the live sessions were not checked, rather than stating a count")
	assert.NotContains(t, notice, "no live sessions",
		"the refusal must never itself claim the project is idle")
}

// TestSidebarDeleteRefusesAfterFailedProjectRefresh is the report's repro: the
// synchronous refresh (launch, project switch, add/delete/rebind completion)
// fails its snapshot fetch. It must neither fabricate zero counts over the
// ones the section last observed, nor let `D` consent on them.
func TestSidebarDeleteRefusesAfterFailedProjectRefresh(t *testing.T) {
	h := newSidebarDeleteHome(t)
	failAllReposFetch(t)
	h.refreshSidebarProjects()

	row := sidebarDeleteActiveRow(t, h)
	assert.Equal(t, 2, row.SessionCount,
		"a failed fetch must keep the last-known count, not wipe it to zero")
	assert.Equal(t, 1, row.InPlaceCount,
		"a failed fetch must keep the last-known in-place count, not wipe it to zero")

	assertSidebarDeleteRefused(t, pressSidebarDelete(t, h))
}

// TestSidebarDeleteRefusesWhileProjectsPollFails covers the stale half: the
// background poll keeps the last-known rows when its fetch fails, so the
// counts on screen are an older snapshot's — sessions may have started or
// ended since. A confirmation built from them states a count nobody observed.
func TestSidebarDeleteRefusesWhileProjectsPollFails(t *testing.T) {
	h := newSidebarDeleteHome(t)
	h.refreshSidebarProjectsFromSnapshot(nil, nil, errors.New("daemon unavailable"))

	assertSidebarDeleteRefused(t, pressSidebarDelete(t, h))
}

// TestSidebarDeleteRefusesAfterFailedRebindConflictRefresh covers the third
// writer of the section's rows: a rebind conflict refreshes them from a
// snapshot of its own, and a failed one leaves the counts unconfirmed.
func TestSidebarDeleteRefusesAfterFailedRebindConflictRefresh(t *testing.T) {
	h := newSidebarDeleteHome(t)
	h.handleRebindConflictSnapshot(rebindConflictSnapshotMsg{
		refusal:  errors.New("project was rebound elsewhere"),
		fetchErr: errors.New("daemon unavailable"),
	})

	assertSidebarDeleteRefused(t, pressSidebarDelete(t, h))
}

// TestSidebarDeleteProceedsOnceThePollRecovers is the control: the gate reads
// the LATEST snapshot, so a healthy poll after a failed one re-opens the real
// confirmation with the honest split — the refusal never outlives the outage.
func TestSidebarDeleteProceedsOnceThePollRecovers(t *testing.T) {
	h := newSidebarDeleteHome(t)
	h.refreshSidebarProjectsFromSnapshot(nil, nil, errors.New("daemon unavailable"))
	h.refreshSidebarProjectsFromSnapshot(sidebarDeleteSnapshot(), nil, nil)

	h = pressSidebarDelete(t, h)
	require.Equal(t, stateConfirm, h.state,
		"a healthy snapshot must re-open the delete confirmation — the gate fails closed only while counts are unobserved")
	require.NotNil(t, h.confirmationOverlay)
	dialog := dialogText(h.confirmationOverlay.Render())
	assert.Contains(t, dialog, "1 in-place session torn down — not restorable.",
		"the confirmation states the observed in-place count")
	assert.Contains(t, dialog, "1 session archived — restorable.",
		"the confirmation states the observed archived count")
	assert.NotContains(t, dialog, "no live sessions")
}

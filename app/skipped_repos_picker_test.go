package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// TestUpdateOpenPickerSkipState_DeferWhileFormActive pins the #4742 review
// finding (P1) on the open-picker refresh: RefreshRows is a no-op while the
// add/rebind path-input form is active (those modes consume every key so D is
// unreachable), so a healthy poll that arrives while a form is open must not
// clear SnapshotDegraded or replace the skip IDs — the rows behind them are
// still stale, and clearing the guard would re-enable the D path against
// unrefreshed counts once the form closes. The whole update defers to the next
// poll after the form closes.
func TestUpdateOpenPickerSkipState_DeferWhileFormActive(t *testing.T) {
	h := newTestHome(t)
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}

	alphaRoot := initTestGitRepo(t)
	rec, err := config.RegisterProject(alphaRoot)
	require.NoError(t, err)
	h.repoRoot = alphaRoot
	h.repoID = config.RepoIDFromRoot(alphaRoot)

	healthy := []session.InstanceData{{Worktree: session.GitWorktreeData{RepoPath: alphaRoot}}}
	projects, degraded := h.buildProjectListFrom(healthy)
	require.False(t, degraded, "precondition: the registry read is not degraded")
	// Make the row registry-backed so `b` enters rebind mode on it.
	for i, p := range projects {
		if p.RepoID == h.repoID {
			projects[i].RegistryID = rec.ID
			projects[i].RegistryRoot = rec.Root
			projects[i].RegistryCheckoutID = rec.CheckoutID
		}
	}

	h.state = stateSwitchProject
	h.projectPickerOverlay = overlay.NewProjectPickerOverlay(projects, h.repoRoot)
	// The picker opened degraded with alpha already marked skipped (the state
	// a Ctrl+P opened while the daemon was skipping alpha).
	h.projectPickerOverlay.SetSnapshotDegraded(true)
	h.projectPickerOverlay.SetSkippedRepoIDs([]string{h.repoID})
	h.projectPickerOverlay.SetSkippedRepoHints(map[string]string{h.repoID: "repair alpha"})
	require.True(t, h.projectPickerOverlay.SnapshotDegraded(), "precondition: degraded guard is set")
	require.True(t, h.projectPickerOverlay.IsRepoSkipped(h.repoID), "precondition: alpha is marked skipped")

	// Enter rebind mode on the registry-backed row: RefreshRows now no-ops.
	handled := h.projectPickerOverlay.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	require.False(t, handled)
	require.True(t, h.projectPickerOverlay.FormActive(), "precondition: the rebind form is active")

	// A HEALTHY poll arrives while the form is open. Without the deferral it
	// would clear SnapshotDegraded and the skip markers; the rows behind them
	// are stale (RefreshRows no-oped), so the update must defer entirely.
	h.updateOpenPickerSkipState(healthy, nil, nil)
	assert.True(t, h.projectPickerOverlay.SnapshotDegraded(),
		"a healthy poll while the form is active must not clear the degraded guard")
	assert.True(t, h.projectPickerOverlay.IsRepoSkipped(h.repoID),
		"a healthy poll while the form is active must not clear a previously skipped repo's marker")
	assert.Equal(t, "repair alpha", h.projectPickerOverlay.SkippedRepoHint(h.repoID),
		"a healthy poll while the form is active must not clear a previously skipped repo's repair hint")
}

// TestShowProjectPickerOverlay_SynthesizesIDLevelRowForSkippedOnlyRepo pins the
// #4742 review finding (P2) on the fresh-picker open path: a skipped repo that
// exists ONLY through its live sessions (no registry/root_agents/active row)
// and that has no cached sidebar row to graft (a fresh start before the first
// poll, or a session-only repo first skipped this poll) would otherwise be
// absent from the picker despite its ID being in SkippedRepos. The picker must
// synthesize an ID-level degraded row so the operator can see the repo is
// skipped, not absent.
func TestShowProjectPickerOverlay_SynthesizesIDLevelRowForSkippedOnlyRepo(t *testing.T) {
	h := newTestHome(t)
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}

	alphaRoot := initTestGitRepo(t)
	_, err := config.RegisterProject(alphaRoot)
	require.NoError(t, err)
	h.repoRoot = alphaRoot
	h.repoID = config.RepoIDFromRoot(alphaRoot)

	// beta is a repo the daemon skips that has NO registry/root_agents/active
	// row and whose instances are all omitted, so buildProjectListFrom cannot
	// reconstruct it and there is no cached sidebar row to graft. On a fresh
	// start m.sidebarProjects is empty.
	const betaID = "beta-skipped-id"
	require.Empty(t, h.sidebarProjects, "precondition: fresh start, no cached sidebar rows")

	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		return []session.InstanceData{{Worktree: session.GitWorktreeData{RepoPath: alphaRoot}}},
			[]daemon.SkippedRepo{{RepoID: betaID, Reason: daemon.SkippedRepoReasonCorruptedInstancesJSON}},
			nil
	}))

	h.showProjectPickerOverlay()
	require.NotNil(t, h.projectPickerOverlay)
	byID := rowByID(t, h.projectPickerOverlay.Projects())
	require.Contains(t, byID, betaID,
		"a skipped repo with no cached row must appear as a synthesized ID-level row instead of being absent")
	assert.Equal(t, betaID, byID[betaID].Name,
		"the synthesized row's name is the repo ID (an ID-level degraded entry)")
	assert.True(t, h.projectPickerOverlay.IsRepoSkipped(betaID),
		"the synthesized row is marked skipped so it renders hidden and D is gated")
}

// TestShowProjectPickerOverlay_DegradedOpenReusesRetainedSkipSet pins the #4742
// review finding (P2) on the degraded-open path: when the sidebar already knows
// a repo is skipped but the synchronous fetch performed while opening Ctrl+P
// fails, buildProjectListWithSnapshotStatus returns skipped == nil. The picker
// must reuse the retained sidebar skip set, hints, and rows so the repo still
// renders hidden and D stays gated, while SnapshotDegraded stays set.
func TestShowProjectPickerOverlay_DegradedOpenReusesRetainedSkipSet(t *testing.T) {
	h := newTestHome(t)
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}

	alphaRoot := initTestGitRepo(t)
	_, err := config.RegisterProject(alphaRoot)
	require.NoError(t, err)
	h.repoRoot = alphaRoot
	h.repoID = config.RepoIDFromRoot(alphaRoot)
	const betaID = "beta-skipped-id"

	// The sidebar already knows beta is skipped from an earlier poll, and
	// retains beta's row and reason-specific hint.
	h.setSidebarProjects([]overlay.Project{
		{RepoID: h.repoID, Name: filepath.Base(alphaRoot), Root: alphaRoot, RegistryID: "prj-alpha"},
		{RepoID: betaID, Name: "beta", Root: "/repos/beta", SessionCount: 2},
	}, false, map[string]bool{betaID: true}, map[string]string{betaID: "repair beta"})
	require.True(t, h.sidebarProjectsSkipped[betaID], "precondition: the sidebar retains beta's skip set")
	require.Equal(t, "repair beta", h.sidebarProjectsSkippedHints[betaID], "precondition: the sidebar retains beta's hint")

	// The synchronous fetch performed while opening Ctrl+P FAILS, so
	// buildProjectListWithSnapshotStatus returns skipped == nil. The picker
	// must reuse the retained sidebar skip set, row, and hint while keeping
	// SnapshotDegraded set.
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, []daemon.SkippedRepo, error) {
		return nil, nil, errors.New("rpc boom")
	}))

	h.showProjectPickerOverlay()
	require.NotNil(t, h.projectPickerOverlay)
	assert.True(t, h.projectPickerOverlay.SnapshotDegraded(),
		"a failed synchronous fetch must leave SnapshotDegraded set")
	byID := rowByID(t, h.projectPickerOverlay.Projects())
	require.Contains(t, byID, betaID,
		"the degraded open must graft beta's retained sidebar row instead of dropping it")
	assert.True(t, h.projectPickerOverlay.IsRepoSkipped(betaID),
		"the degraded open must reuse the retained skip set so beta renders hidden and D is gated")
	assert.Equal(t, "repair beta", h.projectPickerOverlay.SkippedRepoHint(betaID),
		"the degraded open must reuse the retained repair hint, not a default derived from a lost reason")
}

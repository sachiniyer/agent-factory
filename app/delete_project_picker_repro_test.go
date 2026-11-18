package app

import (
	"context"
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

// deletePickerTestRepo builds a real on-disk git repo and returns the resolved
// root + repo id, so the row's identity and the on-disk checkout both resolve
// the way the production switch path (localRepoContextForSelection ->
// config.RepoFromPath) requires. Delete-project deliberately leaves the git repo
// on disk, which is exactly what lets a stale picker row re-commit a switch
// into the just-deleted project in local mode — so resolving against a real
// repo is what makes these tests reproduce the bug rather than short-circuit it.
func deletePickerTestRepo(t *testing.T) (root, repoID string) {
	t.Helper()
	root = initTestGitRepo(t)
	repo, err := config.RepoFromPath(root)
	require.NoError(t, err)
	return root, repo.ID
}

// armDeletePickerRepoHome returns a home scoped to a real git repo (the active
// project), sized and with the Projects section freshly derived, the way a live
// TUI is before the user opens the picker. The all-repos snapshot fetcher is
// stubbed to a SUCCESSFUL empty fetch so the picker opens non-degraded (the
// destructive D gate refuses on a degraded snapshot — see #1973/#1985); callers
// that need a live session row install their own fetcher over this.
func armDeletePickerRepoHome(t *testing.T) (h *home, root, repoID string) {
	t.Helper()
	h = newTestHome(t)
	root, repoID = deletePickerTestRepo(t)
	h.repoRoot = root
	h.repoID = repoID
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return nil, nil
	}))
	resizeHome(h, 120, 45)
	h.refreshSidebarProjects()
	h.relayout()
	return h, root, repoID
}

// openPickerOnActive opens the project picker the way Ctrl+P does, asserting it
// lands in stateSwitchProject with the active project's row pre-selected — the
// exact state a stale-row Enter re-commits the switch from.
func openPickerOnActive(t *testing.T, h *home, root, repoID string) *overlay.ProjectPickerOverlay {
	t.Helper()
	h.showProjectPickerOverlay()
	require.NotNil(t, h.projectPickerOverlay, "the picker must be open")
	require.Equal(t, stateSwitchProject, h.state)
	row, ok := h.projectPickerOverlay.HighlightedProject()
	require.True(t, ok, "the active project's row must be present in the picker")
	assert.Equal(t, repoID, row.RepoID, "the active project's row must be pre-selected")
	return h.projectPickerOverlay
}

// stubDeleteSuccess stubs the daemon delete seam to succeed with archived=n.
func stubDeleteSuccess(t *testing.T, archived int) {
	t.Helper()
	prev := deleteProjectThroughDaemon
	deleteProjectThroughDaemon = func(_, _ string) (daemon.DeleteProjectResponse, error) {
		return daemon.DeleteProjectResponse{OK: true, ArchivedCount: archived}, nil
	}
	t.Cleanup(func() { deleteProjectThroughDaemon = prev })
}

// runDeleteCompletion drives the async delete the way the program does: it runs
// the off-loop deleteProjectCmd closure (the tea.Cmd the confirmation forwards
// as startDeleteProjectMsg -> deleteProjectCmd), then routes the resulting
// projectDeletedMsg through the real m.Update message router, so the test
// exercises the production completion path rather than calling handleProjectDeleted
// directly.
func runDeleteCompletion(t *testing.T, h *home, startMsg startDeleteProjectMsg) (tea.Model, tea.Cmd) {
	t.Helper()
	done, ok := h.deleteProjectCmd(startMsg)().(projectDeletedMsg)
	require.True(t, ok, "deleteProjectCmd must emit projectDeletedMsg")
	require.Equal(t, startMsg.repoID, done.repoID)
	return h.Update(done)
}

// TestDeleteActiveProjectWhilePickerOpenClosesStalePicker is the regression for
// the picker-stale-on-delete bug. The user sequence the report describes — start
// a delete from the picker (or the sidebar), confirm, then reopen the picker
// (Ctrl+P) before projectDeletedMsg lands — leaves the just-deleted project as a
// selectable row, because the picker builds its list once at open time and nothing
// on the delete-completion path rebuilt it. Pressing Enter re-committed a switch
// into the deleted project, reviving the zombie active scope (#4741) the rescope
// had just cleared. handleProjectDeleted now closes the picker on every
// completion that may have landed, mirroring the rebind flow; a subsequent Ctrl+P
// rebuilds rows from the refreshed list at open time.
func TestDeleteActiveProjectWhilePickerOpenClosesStalePicker(t *testing.T) {
	h, root, repoID := armDeletePickerRepoHome(t)

	// Open the picker and press D — the destructive verb routes through the real
	// gate (skippedReposGuardRefusal, SnapshotDegraded) on the highlighted row.
	openPickerOnActive(t, h, root, repoID)
	model, _ := h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	h = model.(*home)
	require.Equal(t, stateConfirm, h.state, "D must open the delete confirmation")
	require.NotNil(t, h.confirmationOverlay)

	stubDeleteSuccess(t, 1)

	// Confirm: the overlay forwards startDeleteProjectMsg and drops to stateDefault.
	// The picker overlay is left dangling (invisible, since state is no longer
	// stateSwitchProject) — the pre-existing minor leak — which is why the bug
	// only revives if the user reopens the picker before the completion lands.
	model, confirmCmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	h = model.(*home)
	require.Equal(t, stateDefault, h.state, "confirm must close the dialog")
	startMsg, ok := confirmCmd().(startDeleteProjectMsg)
	require.True(t, ok, "confirm must forward startDeleteProjectMsg")
	assert.Equal(t, repoID, startMsg.repoID)

	// Reopen the picker (Ctrl+P) during the daemon round-trip, before
	// projectDeletedMsg lands. This is the live trigger route the report names:
	// the fresh picker rebuilds its rows from the still-current registry, which
	// still lists the about-to-be-deleted project, so the row is present and
	// pre-selected — exactly the stale-on-arrival row Enter re-enters.
	openPickerOnActive(t, h, root, repoID)

	// The daemon delete completes while the reopened picker is open.
	runDeleteCompletion(t, h, startMsg)

	// THE FIX: the picker must be torn down so the stale row dies with it. Before
	// the fix this asserted state was stateSwitchProject with the deleted
	// project's row still highlighted.
	assert.Nil(t, h.projectPickerOverlay,
		"a delete completing while the picker is open must close the picker so its stale rows cannot be selected")
	assert.Equal(t, stateDefault, h.state,
		"the model must drop out of stateSwitchProject when the delete closes the picker")
	assert.Equal(t, "", h.repoID, "the rescope must still clear the active scope (#4741)")
	assert.Equal(t, "", h.repoRoot)

	// The stale row can no longer be selected: no project switch is pending, so
	// the zombie active scope the bug recreated stays cleared.
	assert.Nil(t, h.projectSwitch, "Enter on the closed picker cannot have started a switch back into the deleted project")

	// Recovery: a subsequent Ctrl+P reopens the picker from the refreshed list,
	// which no longer contains the deleted project (snapshot empty, active root
	// cleared, no registry record). The user sees what happened, the way the
	// rebind flow's "the refreshed list is where the user sees what happened"
	// convention prescribes.
	h.showProjectPickerOverlay()
	require.NotNil(t, h.projectPickerOverlay)
	for _, p := range h.projectPickerOverlay.Projects() {
		assert.NotEqual(t, repoID, p.RepoID,
			"the reopened picker must not list the deleted project: %v", p)
		assert.NotEqual(t, root, p.Root, "the reopened picker must not list the deleted project's root: %v", p)
	}
}

// TestDeleteProjectUnknownOutcomeClosesStalePicker covers the unknown-outcome
// path: the delete's reply was lost, so the daemon may have archived the
// project's sessions and dropped its registration with only the reply missing.
// handleProjectDeleted does not re-scope on a guess (#4824) — the active scope
// stays — but it must still tear the picker down: an open picker's rows are
// stale the moment the delete may have landed, and Enter on a possibly-deleted
// row re-enters a project the daemon may have removed. This mirrors the rebind
// flow, which closes the picker on an unknown outcome and tells the user to
// check the project list.
func TestDeleteProjectUnknownOutcomeClosesStalePicker(t *testing.T) {
	h, root, repoID := armDeletePickerRepoHome(t)
	openPickerOnActive(t, h, root, repoID)

	prev := deleteProjectThroughDaemon
	deleteProjectThroughDaemon = func(_, _ string) (daemon.DeleteProjectResponse, error) {
		return daemon.DeleteProjectResponse{}, replyLost()
	}
	t.Cleanup(func() { deleteProjectThroughDaemon = prev })

	startMsg := startDeleteProjectMsg{root: root, repoID: repoID, name: filepath.Base(root)}
	runDeleteCompletion(t, h, startMsg)

	assert.Nil(t, h.projectPickerOverlay,
		"an unknown-outcome delete may have landed; the picker must close so its rows cannot be re-entered")
	assert.Equal(t, stateDefault, h.state)
	// The unknown path deliberately does NOT re-scope on a guess, so the active
	// scope stays. The close is independent of the rescope: it protects against
	// a stale row, not a stale scope.
	assert.Equal(t, repoID, h.repoID, "the unknown path must not re-scope on a guess (#4824)")
	assert.Equal(t, root, h.repoRoot)
	assert.Contains(t, h.errBox.FullError(), "could not be confirmed",
		"the unknown outcome must still surface to the user")
}

// TestDeleteProjectDefiniteFailureKeepsPickerOpen is the fail-closed complement
// on the definite-failure path: a delete the daemon provably never acted on
// (e.g. a transport failure before any byte left the process) leaves the project
// registered and its picker rows accurate, so the picker stays open for the user
// to retry — mirroring the rebind flow's definitive-refusal re-arm. Closing the
// picker here would dismiss a correct list and force a pointless Ctrl+P; the fix
// does not touch this branch.
func TestDeleteProjectDefiniteFailureKeepsPickerOpen(t *testing.T) {
	h, root, repoID := armDeletePickerRepoHome(t)
	picker := openPickerOnActive(t, h, root, repoID)

	prev := deleteProjectThroughDaemon
	deleteProjectThroughDaemon = func(_, _ string) (daemon.DeleteProjectResponse, error) {
		return daemon.DeleteProjectResponse{}, neverSent()
	}
	t.Cleanup(func() { deleteProjectThroughDaemon = prev })

	startMsg := startDeleteProjectMsg{root: root, repoID: repoID, name: filepath.Base(root)}
	runDeleteCompletion(t, h, startMsg)

	require.Same(t, picker, h.projectPickerOverlay,
		"a definite failure changed nothing; the picker stays open so the user can retry")
	assert.Equal(t, stateSwitchProject, h.state, "the picker keeps its state on a definitive failure")
	// The row is still accurate: the project the daemon never deleted is still
	// selectable, which is correct.
	row, ok := picker.HighlightedProject()
	require.True(t, ok)
	assert.Equal(t, repoID, row.RepoID, "the row keeps its identity on a definite failure")
	assert.Contains(t, h.errBox.FullError(), "failed to delete project",
		"the definite failure must surface to the user")
}

// TestDeleteNonActiveProjectWhilePickerOpenClosesPicker covers the non-active
// case: deleting a project the TUI is NOT scoped to while the picker is open also
// leaves a stale row (the deleted project's), and Enter on it would switch the
// TUI INTO the just-deleted project — a fresh zombie scope, not the revival of
// an existing one. The rescope does not fire (the deleted project is not the
// active one), but the stale-row hazard is identical, so the close must fire
// regardless of whether the deleted project was the active one.
func TestDeleteNonActiveProjectWhilePickerOpenClosesPicker(t *testing.T) {
	h, activeRoot, activeID := armDeletePickerRepoHome(t)
	deletedRoot, deletedID := deletePickerTestRepo(t)

	// Seed a SECOND distinguishable row so the picker lists both the active
	// project and the to-delete project. The to-delete project's row comes from a
	// registry record the union reads, so register it (the sandboxed
	// AGENT_FACTORY_HOME keeps this hermetic).
	_, err := config.RegisterProject(deletedRoot)
	require.NoError(t, err)

	// The active root's row comes from the active pre-seed; the deleted root's
	// from the registry. Both appear in the freshly built picker.
	h.showProjectPickerOverlay()
	require.NotNil(t, h.projectPickerOverlay)
	require.Equal(t, stateSwitchProject, h.state)
	pickerRows := h.projectPickerOverlay.Projects()
	sawActive, sawDeleted := false, false
	for _, p := range pickerRows {
		if p.RepoID == activeID {
			sawActive = true
		}
		if p.RepoID == deletedID {
			sawDeleted = true
		}
	}
	require.True(t, sawActive, "precondition: the active project is listed in the picker")
	require.True(t, sawDeleted, "precondition: the to-delete project is listed in the picker")

	// Park the highlight on the to-delete row so the bug's Enter re-enters it.
	for i, p := range pickerRows {
		if p.RepoID == deletedID {
			h.projectPickerOverlay.SetSelectedIndex(i)
			break
		}
	}
	row, ok := h.projectPickerOverlay.HighlightedProject()
	require.True(t, ok && row.RepoID == deletedID, "the cursor must rest on the to-delete row")

	stubDeleteSuccess(t, 0)
	startMsg := startDeleteProjectMsg{root: deletedRoot, repoID: deletedID, name: filepath.Base(deletedRoot)}
	runDeleteCompletion(t, h, startMsg)

	assert.Nil(t, h.projectPickerOverlay,
		"a delete of a non-active project must ALSO close the picker — its stale row could switch the TUI into the deleted project")
	assert.Equal(t, stateDefault, h.state)
	// The active scope is untouched: the deleted project was not the active one.
	assert.Equal(t, activeID, h.repoID, "the active scope must be untouched by a non-active delete")
	assert.Equal(t, activeRoot, h.repoRoot)
}

// TestDeleteCompletionInvalidatesPickerRetainedBehindConfirmation is the
// regression for #7525's Codex finding (review thread 4608285856): the
// completion's close only fired in stateSwitchProject, so a delete landing
// while a SECOND picker-launched confirmation sat in stateConfirm left the
// picker — rows captured before the delete — retained underneath it.
// Cancelling that confirmation ran the OnCancel that restores
// stateSwitchProject, re-showing the stale rows; Enter on the deleted project
// then re-created the zombie scope. The completion now invalidates the
// retained overlay whatever the model's state, and the confirmation's cancel
// returns to a freshly built picker — never the captured one.
func TestDeleteCompletionInvalidatesPickerRetainedBehindConfirmation(t *testing.T) {
	h, deletedRoot, deletedID := armDeletePickerRepoHome(t)

	// A second project so the reopened picker has "another row" to press D on:
	// the second confirmation asks about a project that is NOT the one whose
	// delete is in flight, which is exactly what makes its picker stale — the
	// rows were captured before the first delete completed.
	otherRoot, otherID := deletePickerTestRepo(t)
	_, err := config.RegisterProject(otherRoot)
	require.NoError(t, err)

	// Open the picker and start a delete of the ACTIVE project: D → y.
	openPickerOnActive(t, h, deletedRoot, deletedID)
	model, _ := h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	h = model.(*home)
	require.Equal(t, stateConfirm, h.state, "D on the active project's row must open the delete confirmation")

	stubDeleteSuccess(t, 1)
	model, confirmCmd := h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	h = model.(*home)
	startMsg, ok := confirmCmd().(startDeleteProjectMsg)
	require.True(t, ok, "confirm must forward startDeleteProjectMsg")
	require.Equal(t, deletedID, startMsg.repoID)
	require.Equal(t, stateDefault, h.state)

	// While the daemon round-trip is in flight, reopen the picker (Ctrl+P) and
	// press D on ANOTHER row. The picker keeps its just-built rows underneath
	// the confirmation — rows captured BEFORE the first delete completed.
	h.showProjectPickerOverlay()
	require.Equal(t, stateSwitchProject, h.state)
	retained := h.projectPickerOverlay
	rows := retained.Projects()
	otherIdx, sawDeleted := -1, false
	for i, p := range rows {
		if p.RepoID == otherID {
			otherIdx = i
		}
		if p.RepoID == deletedID {
			sawDeleted = true
		}
	}
	require.True(t, sawDeleted, "precondition: the reopened picker still lists the in-flight-deleted project")
	require.NotEqual(t, -1, otherIdx, "precondition: the other project is a picker row")
	retained.SetSelectedIndex(otherIdx)
	model, _ = h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	h = model.(*home)
	require.Equal(t, stateConfirm, h.state, "D on the other row must open a second confirmation")
	require.NotNil(t, h.confirmationOverlay)

	// The FIRST delete completes while that confirmation is open. Its captured
	// picker must be invalidated whatever state the model is in — stateConfirm
	// included — while the confirmation itself is left alone.
	runDeleteCompletion(t, h, startMsg)
	require.Equal(t, stateConfirm, h.state, "the completion leaves the open confirmation alone")
	assert.Nil(t, h.projectPickerOverlay,
		"a completed delete invalidates the retained picker, not only a visibly open one")
	assert.Equal(t, "", h.repoID, "the rescope still cleared the active scope (#4741)")

	// Cancel the confirmation. It must NOT return to the captured picker: the
	// user lands on a freshly built picker whose rows postdate the delete.
	model, _ = h.handleStateConfirm(tea.KeyMsg{Type: tea.KeyEsc})
	h = model.(*home)
	require.Equal(t, stateSwitchProject, h.state, "cancel returns to a freshly built picker")
	require.NotNil(t, h.projectPickerOverlay)
	assert.NotSame(t, retained, h.projectPickerOverlay,
		"the pre-delete picker is invalidated, not restored")
	deletedRowIdx := -1
	for i, p := range h.projectPickerOverlay.Projects() {
		assert.NotEqual(t, deletedID, p.RepoID, "the deleted project must not be a row: %v", p)
		assert.NotEqual(t, deletedRoot, p.Root, "the deleted project's root must not be a row: %v", p)
		if p.RepoID == deletedID {
			deletedRowIdx = i
		}
	}

	// Enter — even pointed at the stale row where the deleted project sat —
	// can no longer commit a switch into it.
	if deletedRowIdx >= 0 {
		h.projectPickerOverlay.SetSelectedIndex(deletedRowIdx)
	}
	model, _ = h.handleStateSwitchProject(tea.KeyMsg{Type: tea.KeyEnter})
	h = model.(*home)
	if h.projectSwitch != nil {
		assert.NotEqual(t, deletedID, h.projectSwitch.repo.ID,
			"Enter must not start a switch into the deleted project")
	}
	assert.NotEqual(t, deletedID, h.repoID, "the zombie active scope stays cleared")
}

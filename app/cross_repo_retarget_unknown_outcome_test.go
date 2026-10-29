package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the regression guard for the intersection the two features that
// introduced the bug never had a test for: a ProjectPath retarget (#1836's
// cross-repo notice machinery) whose save could not be confirmed (#4824's
// mutationOutcomeUnknown branch). The unknown branch used to HoldUnconfirmedEdit
// and continue without registering a retarget candidate, so a retarget whose
// reply was lost but whose patch landed cross-repo was never classified by the
// post-reload (or reload-failure) notice logic. The row vanished from the
// active-scope pane with only a generic "could not be confirmed", the held draft
// never settled (its ID is absent from every active-scope reload), and no
// binding notice ever fired. The fix records the retarget as an unconfirmed
// candidate in that branch too, so the existing classification surfaces it.
//
// Each test drives the edit form to retarget a seeded task, then points the
// task-update seam at a stub that commits via the real task.UpdateTask and
// returns replyLost() (an unknown outcome), so the save takes the
// mutationOutcomeUnknown branch exactly the way a lost-after-send reply does in
// production.

// TestSaveContentPaneState_RetargetUnknownOutcomeLandedSurfacesCrossRepoNotice
// is the core regression: a retarget A->B whose patch committed but whose reply
// was lost lands cross-repo and must surface the binding notice the way the
// committed-success and committed-with-warning branches do. Pre-fix the unknown
// branch skipped recordRetargetIfMoved, so no candidate was ever classified and
// the task vanished silently.
func TestSaveContentPaneState_RetargetUnknownOutcomeLandedSurfacesCrossRepoNotice(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	repoA := initTestGitRepo(t)
	repoB := initTestGitRepo(t)
	require.NotEqual(t, config.RepoIDFromRoot(repoA), config.RepoIDFromRoot(repoB))

	t.Chdir(repoA)
	h.repoRoot = repoA
	h.repoID = config.RepoIDFromRoot(repoA)

	seed := task.Task{
		ID: "retarget-unknown", Name: "Retarget me", Prompt: "p",
		CronExpr: "* * * * *", ProjectPath: repoA, Program: "claude",
		Enabled: true, CreatedAt: time.Now(),
	}
	require.NoError(t, task.AddTask(seed))

	tp := h.automations.TaskPane()
	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	tp.SetTasks(loaded)
	h.store.SetTasks(loaded)

	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	require.True(t, tp.IsEditing(),
		"showTasksOverlay drops into the seeded task's edit form")
	press := func(k tea.KeyMsg) { _, _ = h.handleStateTasks(k) }
	tabToTaskField(t, h, "path")
	require.True(t, tp.IsTextFieldFocused(),
		"the focus must be on a text input (Path) after the tab walk from Name")
	for range repoA {
		press(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(repoB)})
	press(tea.KeyMsg{Type: tea.KeyEnter}) // submit (validates B as a git repo, marks dirty, exits editing)
	require.False(t, tp.IsEditing())
	require.True(t, tp.IsDirty(), "the retargeted task must be queued for save")

	// The update commits the patch to disk (B's root as ProjectPath) and then
	// returns an unknown outcome — the reply was lost after the mutation landed
	// (#4824) — so the save takes the mutationOutcomeUnknown branch. The
	// retarget must now be recorded as an unconfirmed candidate too.
	t.Cleanup(SetTaskUpdaterForTest(func(id string, update task.TaskUpdate, expect task.ProjectExpectation) error {
		_, err := task.UpdateTask(id, update, expect)
		require.NoError(t, err)
		return replyLost()
	}))

	require.ErrorContains(t, h.saveContentPaneState(), "could not be confirmed",
		"the unknown outcome still surfaces as a could-not-be-confirmed failure")

	// The retarget landed on disk: the task is at B, with RepoID = B.
	disk, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, disk, 1)
	assert.Equal(t, repoB, disk[0].ProjectPath, "the patch committed with B's root as the ProjectPath")
	assert.Equal(t, config.RepoIDFromRoot(repoB), disk[0].RepoID,
		"the daemon re-derived the RepoID from the new ProjectPath at write time")

	// The active-scope (A) reload drops the retargeted row from the rail/store.
	assert.Empty(t, h.store.GetTasks(),
		"a task retargeted out of the active project must not appear in the active-scope store")

	// The cross-repo retarget notice is queued — the #1836 silent drop this fix
	// exists to surface. Pre-fix, no notice fired and the row vanished with
	// only a generic "could not be confirmed".
	notice := tp.TakeCrossRepoRetargetNotice()
	require.NotEmpty(t, notice,
		"a landed cross-repo retarget whose reply was lost must queue a binding notice")
	assert.Contains(t, notice, "Retarget me", "the notice names the retargeted task")
	assert.Contains(t, notice, repoB, "the notice echoes the binding that landed")
	assert.Contains(t, notice, "no longer appears in this project's list",
		"the notice explains the missing row")
	assert.Empty(t, tp.TakeCrossRepoRetargetNotice(), "the cross-repo retarget notice is one-shot")
}

// TestSaveContentPaneState_RetargetUnknownOutcomeInScopeSettlesDraft is the
// control sibling: an unknown-outcome retarget that stays in the active project
// (the patch landed within the active repo) must NOT raise a cross-repo notice,
// and the held draft must settle via settleConfirmedDrafts once the
// active-scope reload carries the edited path. This pins that recording the
// candidate in the unknown branch did not change the #4824 not-landed/settle
// behavior — a retarget whose ID is still in the reloaded list falls through
// to the settle-or-stay-held path exactly as before.
func TestSaveContentPaneState_RetargetUnknownOutcomeInScopeSettlesDraft(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	repoA := initTestGitRepo(t)

	t.Chdir(repoA)
	h.repoRoot = repoA
	h.repoID = config.RepoIDFromRoot(repoA)

	// A subdirectory of the active repo still resolves to the active repo
	// identity, so a retarget onto it stays in scope and exercises the settle
	// path rather than the cross-repo notice path.
	sub := filepath.Join(repoA, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))

	seed := task.Task{
		ID: "retarget-in-scope", Name: "Retarget me", Prompt: "p",
		CronExpr: "* * * * *", ProjectPath: repoA, Program: "claude",
		Enabled: true, CreatedAt: time.Now(),
	}
	require.NoError(t, task.AddTask(seed))

	tp := h.automations.TaskPane()
	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	tp.SetTasks(loaded)
	h.store.SetTasks(loaded)

	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	require.True(t, tp.IsEditing(),
		"showTasksOverlay drops into the seeded task's edit form")
	press := func(k tea.KeyMsg) { _, _ = h.handleStateTasks(k) }
	tabToTaskField(t, h, "path")
	require.True(t, tp.IsTextFieldFocused(),
		"the focus must be on a text input (Path) after the tab walk from Name")
	for range repoA {
		press(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(sub)})
	press(tea.KeyMsg{Type: tea.KeyEnter}) // submit (sub is inside a git repo, so it validates)
	require.False(t, tp.IsEditing())
	require.True(t, tp.IsDirty(), "the retargeted task must be queued for save")

	t.Cleanup(SetTaskUpdaterForTest(func(id string, update task.TaskUpdate, expect task.ProjectExpectation) error {
		_, err := task.UpdateTask(id, update, expect)
		require.NoError(t, err)
		return replyLost()
	}))

	// The unknown outcome still surfaces as a could-not-be-confirmed — the save
	// could not be verified at commit time — but the edit landed and the reload
	// settles it, so the pane ends clean.
	require.ErrorContains(t, h.saveContentPaneState(), "could not be confirmed")

	// The retarget landed in scope: disk carries the subdirectory path, still
	// bound to the active repo.
	disk, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, disk, 1)
	assert.Equal(t, sub, disk[0].ProjectPath)
	assert.Equal(t, config.RepoIDFromRoot(repoA), disk[0].RepoID,
		"the subdirectory still resolves to the active repo identity")

	// The active-scope reload settled the held draft.
	require.Len(t, tp.GetTasks(), 1, "the in-scope retarget stays in the pane")
	assert.Equal(t, sub, tp.GetTasks()[0].ProjectPath)
	assert.False(t, tp.IsDirty(), "the held draft settled once the reload carried the edit")
	notice := tp.TakeSettledDraftNotice()
	require.NotEmpty(t, notice, "the settled draft must be announced")
	assert.Contains(t, notice, "Retarget me", "the settlement names the task")
	assert.Empty(t, tp.TakeCrossRepoRetargetNotice(),
		"an in-scope retarget must NOT raise a cross-repo notice")
	assert.Empty(t, tp.TakeUnconfirmedQuitNotice(),
		"a settled edit is no longer warned about as unconfirmed")
}

// TestSaveContentPaneState_RetargetUnknownOutcomeQueuesNoticeWhenReloadFails is
// the reload-failure sibling: the unknown-outcome retarget commits the
// cross-repo binding, but the post-save active-scope reload fails transiently.
// With no in-scope list to classify the candidate, the reload-failure branch
// falls back to the committed destination (sameScopeAfterSave) the way it does
// for the committed-success and committed-with-warning branches, so the
// cross-repo notice still fires rather than leaving the task to vanish on the
// next snapshot poll with no explanation.
func TestSaveContentPaneState_RetargetUnknownOutcomeQueuesNoticeWhenReloadFails(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	repoA := initTestGitRepo(t)
	repoB := initTestGitRepo(t)
	require.NotEqual(t, config.RepoIDFromRoot(repoA), config.RepoIDFromRoot(repoB))

	t.Chdir(repoA)
	h.repoRoot = repoA
	h.repoID = config.RepoIDFromRoot(repoA)

	seed := task.Task{
		ID: "retarget-unknown-reloadfail", Name: "Retarget me", Prompt: "p",
		CronExpr: "* * * * *", ProjectPath: repoA, Program: "claude",
		Enabled: true, CreatedAt: time.Now(),
	}
	require.NoError(t, task.AddTask(seed))

	tp := h.automations.TaskPane()
	loaded, err := task.LoadTasksForCurrentRepo()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	tp.SetTasks(loaded)
	h.store.SetTasks(loaded)

	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	require.True(t, tp.IsEditing(),
		"showTasksOverlay drops into the seeded task's edit form")
	press := func(k tea.KeyMsg) { _, _ = h.handleStateTasks(k) }
	tabToTaskField(t, h, "path")
	require.True(t, tp.IsTextFieldFocused(),
		"the focus must be on a text input (Path) after the tab walk from Name")
	for range repoA {
		press(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(repoB)})
	press(tea.KeyMsg{Type: tea.KeyEnter})
	require.False(t, tp.IsEditing())
	require.True(t, tp.IsDirty(), "the retargeted task must be queued for save")

	// The update commits the retarget to disk (B's root as ProjectPath) and
	// returns an unknown outcome; the reload then reads through getTasksPathFn,
	// which is swapped to a synthetic failure for the reload only.
	var restoreReloadPath func()
	t.Cleanup(SetTaskUpdaterForTest(func(id string, update task.TaskUpdate, expect task.ProjectExpectation) error {
		_, err := task.UpdateTask(id, update, expect)
		require.NoError(t, err)
		restoreReloadPath = task.SetTasksPathFnForTest(func() (string, error) {
			return "", fmt.Errorf("simulated tasks.json read failure")
		})
		return replyLost()
	}))

	err = h.saveContentPaneState()
	require.ErrorContains(t, err, "could not be confirmed",
		"the unknown outcome still surfaces")
	require.ErrorContains(t, err, "failed to reload tasks after save",
		"the reload failure surfaces alongside the retarget notice")
	// Restore the real path so the disk assertions below can read tasks.json.
	if restoreReloadPath != nil {
		restoreReloadPath()
	}

	// The retarget committed: disk carries the task at B, with RepoID = B.
	disk, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, disk, 1)
	assert.Equal(t, repoB, disk[0].ProjectPath, "the patch committed with B's root as the ProjectPath")
	assert.Equal(t, config.RepoIDFromRoot(repoB), disk[0].RepoID,
		"the daemon re-derived the RepoID from the new ProjectPath at write time")

	// The reload failed, so the not-known destination is the only signal; the
	// notice is queued from the reload-failure branch using that destination.
	notice := tp.TakeCrossRepoRetargetNotice()
	require.NotEmpty(t, notice,
		"a cross-repo retarget whose reload failed must still queue a binding notice")
	assert.Contains(t, notice, "Retarget me", "the notice names the retargeted task")
	assert.Contains(t, notice, repoB, "the notice echoes the binding that landed")
	assert.Empty(t, tp.TakeCrossRepoRetargetNotice(), "the cross-repo retarget notice is one-shot")
}

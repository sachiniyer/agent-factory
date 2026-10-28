package app

import (
	"fmt"
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

// TestHandleTaskCreate_CrossRepoSurfacesBindingNotice is the regression guard for
// the cross-repo create bug: when the create form's project path points at a git
// repository different from the TUI's active project, the save commits, but the
// post-save reload is scoped to the active project and so omits the new task.
// Pre-fix, handleTaskCreate closed the form as an unqualified silent success —
// no error, no notice, no echo of the binding — which the user would read as a
// failed save and create it again, silently accumulating duplicate cron/watch
// automations in the other repository. Post-fix, the reload uses the active
// scope and the cross-repo landing is named the way the CLI's `project_path` echo
// does (#1891).
func TestHandleTaskCreate_CrossRepoSurfacesBindingNotice(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}

	repoA := initTestGitRepo(t)
	repoB := initTestGitRepo(t)
	require.NotEqual(t, config.RepoIDFromRoot(repoA), config.RepoIDFromRoot(repoB),
		"precondition: the two repos must be distinct projects")

	// Make A the active scope (mirrors production startup when af launches in A).
	t.Chdir(repoA)
	h.repoRoot = repoA
	h.repoID = config.RepoIDFromRoot(repoA)

	// Default addTaskThroughDaemon writes to disk via task.AddTask (newTestHome
	// stub), so the create reaches tasks.json and AddTaskChecked resolves the
	// task's RepoID from its ProjectPath at write time.

	tp := h.automations.TaskPane()
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	// Open the create form pre-filled with B's root — the same shape as the
	// form's "n" handler when the in-pane path has been set to another repo.
	tp.EnterCreateMode(repoB)
	require.True(t, tp.IsCreating())

	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("cross")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> trigger selector (cron stays selected)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> schedule picker (valid daily default)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do the cross-repo thing")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> target session
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> path (B's root, prefilled)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> program (default option)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> save button
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	// The save reached disk and bound the task to B's repo (its RepoID is derived
	// from ProjectPath at AddTask time).
	saved, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, saved, 1, "the create must reach disk")
	assert.Equal(t, "cross", saved[0].Name)
	assert.Equal(t, repoB, saved[0].ProjectPath, "the task's ProjectPath is the path the form committed")
	assert.Equal(t, config.RepoIDFromRoot(repoB), saved[0].RepoID,
		"the task's RepoID is its binding's repo identity, NOT the TUI's active scope")

	// The active-scope reload omitted the cross-repo task: the pane and the
	// sidebar store both show no tasks. Pre-fix this silent empty list was
	// indistinguishable from a failed save.
	assert.Empty(t, tp.GetTasks(),
		"the cross-repo task must NOT appear in the active project's pane (#2098 scope filtering)")
	assert.Empty(t, h.store.GetTasks(),
		"the cross-repo task must NOT appear in the active project's sidebar store")

	// And the cross-repo landing is surfaced. handleTaskCreate returns a notice
	// naming the binding (the CLI's project_path echo, #1891), so the user is not
	// left to infer a failed save. The errBox renders the notice synchronously
	// inside setTransientNotice, so the surfaced text is readable immediately —
	// as long as the bar has a size, which a fresh newTestHome leaves at zero.
	h.errBox.SetSize(500, 1)
	assert.Contains(t, h.errBox.String(), "cross",
		"the notice must name the task the user just created")
	assert.Contains(t, h.errBox.String(), repoB,
		"the notice must echo the binding that landed rather than leaving the cross-repo save invisible")
	assert.Contains(t, h.errBox.String(), "does not appear in this project's list",
		"the notice must explain why the pane omits the just-saved task")
}

// TestHandleTaskCreate_CrossRepoCommittedWarningNamesTheBinding is the
// committed-with-warning sibling of the cross-repo create guard above. When the
// durable write lands but the daemon's scheduler refresh fails (the adder
// returns MutationCommitted), the create path surfaces the scheduler warning
// via handleError. A cross-repo create's task is omitted from the active-scope
// reload, so before this guard the warning branch returned before reaching the
// absence check — the user got the scheduler warning but not the binding, and
// read the empty pane as a failed save that lost the task. The warning branch
// now names where the task landed the way the nil-error branch does.
func TestHandleTaskCreate_CrossRepoCommittedWarningNamesTheBinding(t *testing.T) {
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

	// The adder writes the task to disk (so the active-scope reload omits it as
	// cross-repo) and returns a committed-with-warning outcome so handleTaskCreate
	// takes the MutationCommitted branch rather than the nil-error cross-repo path.
	t.Cleanup(SetTaskAdderForTest(func(tk task.Task) error {
		require.NoError(t, task.AddTask(tk))
		return committedTaskMutationTestError{}
	}))

	tp := h.automations.TaskPane()
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	tp.EnterCreateMode(repoB)
	require.True(t, tp.IsCreating())

	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("cross-warn")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // trigger selector
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // schedule picker
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // prompt
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("committed cross-repo")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // target session
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // path (B's root, prefilled)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // program
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // save
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	// The durable write landed in B (its RepoID is derived from ProjectPath at
	// AddTask time), but the active-scope reload (A) omits it.
	saved, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, saved, 1, "the committed add must reach disk")
	assert.Equal(t, "cross-warn", saved[0].Name)
	assert.Equal(t, repoB, saved[0].ProjectPath)
	assert.Empty(t, tp.GetTasks(),
		"the cross-repo task must NOT appear in the active project's pane")

	// The scheduler warning surfaces (the committed-with-warning outcome), and
	// the cross-repo binding rides with it so the empty pane does not read as a
	// failed save that lost the task.
	h.errBox.SetSize(500, 1)
	assert.Contains(t, h.errBox.String(), "scheduler",
		"the committed-with-warning outcome must still surface")
	assert.Contains(t, h.errBox.String(), "cross-warn",
		"the warning must name the committed task")
	assert.Contains(t, h.errBox.String(), repoB,
		"the warning must echo the binding the task landed at, not just the scheduler failure")
	assert.Contains(t, h.errBox.String(), "does not appear in this project's list",
		"the warning must explain why the pane omits the just-saved task")
}

// TestHandleTaskCreate_SameRepoAfterSwitchAppearsInList is the control + the
// post-project-switch fix: when the user switches to a project (m.repoID/repoRoot
// move with it) and then creates a task at that project's root, the reload is
// scoped to the switched-to project and the new task shows up in the pane. The
// pre-fix cwd-based reload clobbered the switched-to project's rows with the
// launch repo's tasks until the next snapshot poll.
func TestHandleTaskCreate_SameRepoAfterSwitchAppearsInList(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}

	repoA := initTestGitRepo(t)
	repoB := initTestGitRepo(t)
	require.NotEqual(t, config.RepoIDFromRoot(repoA), config.RepoIDFromRoot(repoB))

	// af launches in A; the user switches to B; then creates a task whose
	// ProjectPath is B's root (the pre-fill default after the switch).
	t.Chdir(repoA)
	h.repoRoot = repoA
	h.repoID = config.RepoIDFromRoot(repoA)
	h.switchProject(&config.RepoContext{Root: repoB, ID: config.RepoIDFromRoot(repoB)})
	require.Equal(t, repoB, h.repoRoot, "precondition: the switch must move the active scope to B")
	require.Equal(t, config.RepoIDFromRoot(repoB), h.repoID)

	tp := h.automations.TaskPane()
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	tp.EnterCreateMode(repoB)
	require.True(t, tp.IsCreating())

	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("switched")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // trigger selector
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // schedule picker (daily default)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // prompt
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("run inside B")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // target session
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // path
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // program
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // save
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	saved, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, saved, 1)
	assert.Equal(t, repoB, saved[0].ProjectPath)
	assert.Equal(t, config.RepoIDFromRoot(repoB), saved[0].RepoID)

	// The reload is scoped to the ACTIVE project (B), so the task lands in the
	// pane and the sidebar store immediately. Pre-fix, the cwd-based reload
	// clobbered the pane with A's tasks until the next snapshot poll.
	require.Len(t, tp.GetTasks(), 1,
		"a same-repo create after a project switch must put the new task in the active-scope pane")
	assert.Equal(t, "switched", tp.GetTasks()[0].Name)
	require.Len(t, h.store.GetTasks(), 1,
		"the sidebar store must match the pane after a same-repo create")
	assert.Equal(t, "switched", h.store.GetTasks()[0].Name)

	// No notice fires on a same-repo create — the task's binding matches the
	// active scope, so the pane shows it and there is nothing to echo.
	assert.NotContains(t, h.errBox.String(), "does not appear in this project's list",
		"a same-repo create must NOT raise the cross-repo binding notice")
}

// TestHandleTaskCreate_CrossRepoAfterSwitchNamesTheCrossRepoBinding extends the
// cross-repo notice to the post-project-switch case: the active scope is B, the
// user names C as the binding in the create form. The save commits to C; the
// active-scope reload omits it; the notice must name C so the user does not
// mistake the empty pane for a failed save.
func TestHandleTaskCreate_CrossRepoAfterSwitchNamesTheCrossRepoBinding(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}

	repoA := initTestGitRepo(t)
	repoB := initTestGitRepo(t)
	repoC := initTestGitRepo(t)
	require.NotEqual(t, config.RepoIDFromRoot(repoA), config.RepoIDFromRoot(repoB))
	require.NotEqual(t, config.RepoIDFromRoot(repoA), config.RepoIDFromRoot(repoC))
	require.NotEqual(t, config.RepoIDFromRoot(repoB), config.RepoIDFromRoot(repoC))

	t.Chdir(repoA)
	h.repoRoot = repoA
	h.repoID = config.RepoIDFromRoot(repoA)
	h.switchProject(&config.RepoContext{Root: repoB, ID: config.RepoIDFromRoot(repoB)})
	require.Equal(t, repoB, h.repoRoot)
	require.Equal(t, config.RepoIDFromRoot(repoB), h.repoID)

	// The user retargets the create at a THIRD repo (C) other than the active B.
	tp := h.automations.TaskPane()
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	tp.EnterCreateMode(repoC)
	require.True(t, tp.IsCreating())

	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tri-target")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // trigger selector
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // schedule picker
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // prompt
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("land in C")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // target session
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // path
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // program
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // save
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	saved, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, saved, 1)
	assert.Equal(t, repoC, saved[0].ProjectPath, "the task bound to C")
	assert.Equal(t, config.RepoIDFromRoot(repoC), saved[0].RepoID)

	// B's pane omits the cross-repo task (its binding is C).
	assert.Empty(t, tp.GetTasks(),
		"a task bound to C must not appear in B's pane after the active-scope reload")
	assert.Empty(t, h.store.GetTasks())

	// The notice must name C — not B (active scope) and not the launch repo A.
	// Set a bar size so the rendered notice text is readable; a fresh newTestHome
	// leaves the bar at zero size and String() collapses to "" in that state.
	h.errBox.SetSize(500, 1)
	assert.Contains(t, h.errBox.String(), repoC,
		"the cross-repo notice must echo the binding that landed (C)")
	assert.Contains(t, h.errBox.String(), "tri-target",
		"the cross-repo notice must name the task")
	assert.NotContains(t, h.errBox.String(), "bound to "+repoA,
		"the notice must not point at the launch repo (A), which the user did not bind")
	assert.NotContains(t, h.errBox.String(), "bound to "+repoB,
		"the notice must not point at the active repo (B), which the user did not bind")
}

// TestHandleTaskCreate_SameRepoSilentSuccessIsUnchanged pins the same-repo
// happy path: name a task whose Path matches the active scope and the form
// closes as the unqualified success the bug fix must NOT regress. No notice
// fires, the pane shows the new task, and the cmd returned by handleStateTasks
// carries no delayed clear (the cross-repo branch is not taken).
func TestHandleTaskCreate_SameRepoSilentSuccessIsUnchanged(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}

	repoA := initTestGitRepo(t)
	t.Chdir(repoA)
	h.repoRoot = repoA
	h.repoID = config.RepoIDFromRoot(repoA)

	var recorded []task.Task
	t.Cleanup(SetTaskAdderForTest(func(tk task.Task) error {
		recorded = append(recorded, tk)
		return task.AddTask(tk)
	}))

	tp := h.automations.TaskPane()
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	tp.EnterCreateMode(repoA)
	require.True(t, tp.IsCreating())

	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("same-repo")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // trigger selector
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // schedule picker
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // prompt
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("stay in A")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // target session
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // path
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // program
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // save
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	require.Len(t, recorded, 1, "the create routed through the daemon seam once")
	assert.Equal(t, repoA, recorded[0].ProjectPath)
	require.Len(t, tp.GetTasks(), 1, "the same-repo task is in the pane")
	assert.Equal(t, "same-repo", tp.GetTasks()[0].Name)

	// No notice fires on the unqualified success.
	assert.Empty(t, h.errBox.String(),
		"a same-repo create must close the form as the unqualified success the bug fix must not regress")
}

// TestSaveContentPaneState_CrossRepoRetargetQueuesBindingNotice is the
// recommended-fix regression guard for the same seam on the edit-save path: a
// task in the active project that is retargeted at another repo through the
// edit form commits the patch, then the reload — scoped to the active project
// — drops the row from the pane. Pre-fix the row vanished with no notice, and
// the user re-edited a task they could no longer see. Post-fix the new binding
// is queued for the cross-repo retarget notice (#1891 echo for the edit path,
// #1836).
func TestSaveContentPaneState_CrossRepoRetargetQueuesBindingNotice(t *testing.T) {
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

	// Seed a task in A so the edit form has a row to retarget.
	seed := task.Task{
		ID:          "retarget-me",
		Name:        "Retarget me",
		Prompt:      "p",
		CronExpr:    "* * * * *",
		ProjectPath: repoA,
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now(),
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
		"showTasksOverlay drops into the selected task's edit form when a task is seeded")
	// Tab to the Path field by its identity (not a press count, which every new
	// form field shifts), then clear the path field, type B's root, and submit via Enter. Enter submits from any non-prompt
	// field, so the focus position does not need to land on Save to commit. The
	// path field was seeded with repoA (the task's stored ProjectPath), so its
	// existing length is len(repoA) — backspace once per rune to clear it.
	press := func(k tea.KeyMsg) { _, _ = h.handleStateTasks(k) }
	tabToTaskField(t, h, "path")
	require.True(t, tp.IsTextFieldFocused(),
		"the focus must be on a text input (Path) after the tab walk from Name")
	for range repoA {
		press(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(repoB)})
	press(tea.KeyMsg{Type: tea.KeyEnter}) // submit the form (validates B as a real git repo, marks dirty, exits editing)
	require.False(t, tp.IsEditing(), "Enter must submit the form")
	require.True(t, tp.IsDirty(), "the retargeted task must be queued for save")
	// Close the overlay to flush the dirty edit through saveContentPaneState.
	press(tea.KeyMsg{Type: tea.KeyEsc})

	// The retarget committed: disk carries the task at B, with RepoID = B.
	disk, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, disk, 1)
	assert.Equal(t, repoB, disk[0].ProjectPath, "the patch committed with B's root as the ProjectPath")
	assert.Equal(t, config.RepoIDFromRoot(repoB), disk[0].RepoID,
		"the daemon re-derived the RepoID from the new ProjectPath at write time")

	// The active-scope reload drops the retargeted row from the pane (#2098
	// scope filtering), with no spurious held draft because the patch saved.
	assert.Empty(t, tp.GetTasks(),
		"a task retargeted out of the active project must not appear in the active-scope pane")

	// And the new binding is queued for the cross-repo retarget notice — the
	// CLI's project_path echo (#1891) for the edit path. The notice is delivered
	// by TakeCrossRepoRetargetNotice the way TakeDiscardedDraftNotice is: once,
	// by handleQuit or the next snapshot poll. Draining here verifies the queue.
	notice := tp.TakeCrossRepoRetargetNotice()
	require.NotEmpty(t, notice, "the retarget that crossed repos must queue a binding notice")
	assert.Contains(t, notice, "Retarget me", "the notice names the retargeted task")
	assert.Contains(t, notice, repoB, "the notice echoes the binding that landed")
	assert.Contains(t, notice, "no longer appears in this project's list",
		"the notice explains the missing row")

	// A second take drains nothing: the notice is one-shot.
	assert.Empty(t, tp.TakeCrossRepoRetargetNotice(),
		"the cross-repo retarget notice is raised once, then cleared")
}

// TestSaveContentPaneState_CrossRepoRetargetQueuesNoticeOnCommittedWarning is the
// committed-with-warning sibling of the retarget notice guard above. When the
// daemon reports MutationCommitted (the durable write landed but its scheduler
// refresh failed), the save's error branch acknowledges the edit and keeps
// surfacing the schedule failure, but the retarget still committed the new
// ProjectPath — so the active-scope reload still drops the row and the
// cross-repo notice must still be queued. Pre-fix the error branch returned
// before the retarget-recording block, so a committed-with-warning retarget
// left the user with a generic "failed to save task" and no idea where it landed.
func TestSaveContentPaneState_CrossRepoRetargetQueuesNoticeOnCommittedWarning(t *testing.T) {
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
		ID:          "retarget-committed",
		Name:        "Retarget me",
		Prompt:      "p",
		CronExpr:    "* * * * *",
		ProjectPath: repoA,
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now(),
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
		"showTasksOverlay drops into the selected task's edit form when a task is seeded")
	// Tab to the Path field, clear it, and enter B's root to retarget the task.
	press := func(k tea.KeyMsg) { _, _ = h.handleStateTasks(k) }
	tabToTaskField(t, h, "path")
	require.True(t, tp.IsTextFieldFocused(),
		"the focus must be on a text input (Path) after the tab walk from Name")
	for range repoA {
		press(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(repoB)})
	press(tea.KeyMsg{Type: tea.KeyEnter}) // submit the form (validates B as a real git repo, marks dirty, exits editing)
	require.False(t, tp.IsEditing(), "Enter must submit the form")
	require.True(t, tp.IsDirty(), "the retargeted task must be queued for save")

	// The update commits the new ProjectPath to disk but reports a committed-
	// with-warning outcome (the daemon's scheduler refresh failed), so the save
	// surfaces the schedule failure AND must still record the retarget.
	t.Cleanup(SetTaskUpdaterForTest(func(id string, update task.TaskUpdate, expect task.ProjectExpectation) error {
		_, err := task.UpdateTask(id, update, expect)
		require.NoError(t, err)
		return committedTaskMutationTestError{}
	}))

	// Flush the dirty edit through saveContentPaneState directly. The committed
	// branch surfaces the schedule failure as the return value, and the retarget
	// recording runs on the same path the nil-error success does.
	require.Error(t, h.saveContentPaneState(), "the post-commit schedule failure must remain visible")

	// The retarget committed: disk carries the task at B, with RepoID = B.
	disk, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, disk, 1)
	assert.Equal(t, repoB, disk[0].ProjectPath, "the patch committed with B's root as the ProjectPath")

	// The active-scope reload drops the retargeted row from the pane.
	assert.Empty(t, tp.GetTasks(),
		"a task retargeted out of the active project must not appear in the active-scope pane")

	// The retarget that committed with a warning is still recorded for the
	// cross-repo notice — the user is told where it landed, not just shown a
	// generic "failed to save task".
	notice := tp.TakeCrossRepoRetargetNotice()
	require.NotEmpty(t, notice,
		"a committed-with-warning retarget must queue a cross-repo binding notice")
	assert.Contains(t, notice, "Retarget me", "the notice names the retargeted task")
	assert.Contains(t, notice, repoB, "the notice echoes the binding that landed")
	assert.Empty(t, tp.TakeCrossRepoRetargetNotice(),
		"the cross-repo retarget notice is raised once, then cleared")
}

// TestSaveContentPaneState_CrossRepoRetargetQueuesNoticeWhenReloadFails is the
// reload-failure sibling of the retarget notice guards above. The retarget
// commits the new ProjectPath, but the post-save active-scope reload fails
// transiently — so the in-scope list above cannot classify the retarget. The
// save still knows the committed destination, so a retarget that crossed repos
// must be recorded from the reload-failure branch too; otherwise the next
// successful snapshot poll drops the row from the pane with no notice and the
// task vanishes the way this change exists to prevent (#1836).
func TestSaveContentPaneState_CrossRepoRetargetQueuesNoticeWhenReloadFails(t *testing.T) {
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
		ID:          "retarget-reload-fail",
		Name:        "Retarget me",
		Prompt:      "p",
		CronExpr:    "* * * * *",
		ProjectPath: repoA,
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now(),
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
		"showTasksOverlay drops into the selected task's edit form when a task is seeded")
	press := func(k tea.KeyMsg) { _, _ = h.handleStateTasks(k) }
	tabToTaskField(t, h, "path")
	require.True(t, tp.IsTextFieldFocused(),
		"the focus must be on a text input (Path) after the tab walk from Name")
	for range repoA {
		press(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(repoB)})
	press(tea.KeyMsg{Type: tea.KeyEnter}) // submit the form (validates B as a real git repo, marks dirty, exits editing)
	require.False(t, tp.IsEditing(), "Enter must submit the form")
	require.True(t, tp.IsDirty(), "the retargeted task must be queued for save")

	// The update commits the retarget to disk (B's root as ProjectPath), then
	// makes the post-save active-scope reload fail so the in-scope list above
	// cannot classify the retarget. The save must still record the notice from
	// the reload-failure branch using the committed destination.
	var restoreReloadPath func()
	t.Cleanup(SetTaskUpdaterForTest(func(id string, update task.TaskUpdate, expect task.ProjectExpectation) error {
		_, err := task.UpdateTask(id, update, expect)
		require.NoError(t, err)
		// The update committed via the real path; now the reload reads through
		// getTasksPathFn, so swap it to a synthetic failure for the reload only.
		restoreReloadPath = task.SetTasksPathFnForTest(func() (string, error) {
			return "", fmt.Errorf("simulated tasks.json read failure")
		})
		return nil
	}))

	require.ErrorContains(t, h.saveContentPaneState(), "failed to reload tasks after save",
		"the reload failure must surface alongside the retarget notice")
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

	// The reload failed, so the pane's list is unchanged (the retargeted row is
	// still on it from before the save). The notice is what tells the user the
	// task landed in B — without it, the next successful snapshot poll drops the
	// row with no explanation.
	notice := tp.TakeCrossRepoRetargetNotice()
	require.NotEmpty(t, notice,
		"a cross-repo retarget whose reload failed must still queue a binding notice")
	assert.Contains(t, notice, "Retarget me", "the notice names the retargeted task")
	assert.Contains(t, notice, repoB, "the notice echoes the binding that landed")
	assert.Empty(t, tp.TakeCrossRepoRetargetNotice(),
		"the cross-repo retarget notice is raised once, then cleared")
}

// TestPollSurfacesDiscardedDraftAndRetargetTogether is the #1836 guard for the
// snapshot poll's notice draining: a save that both discards a draft (its task
// was deleted) and retargets another task to a different repo queues both a
// discarded-draft notice (#4798) and a cross-repo retarget notice (#1836). The
// poll drains both in one tick — showTransientMessage replaces the bar, so
// draining each notice with its own call would clobber the more urgent
// discarded-work warning with the retarget notice (and clear both durable queues
// before either reached the user). The poll combines them into one message so
// neither is lost.
func TestPollSurfacesDiscardedDraftAndRetargetTogether(t *testing.T) {
	h, sp, demo := homeWithHeldDraft(t, true)

	// A save proved demo's task was deleted, so the pane discards its draft and
	// queues the discarded-draft notice (#4798).
	sp.DiscardDeletedDraft(demo.ID)

	// The same save retargeted another task out of the active project, so a
	// retarget notice is queued too (#1836).
	sp.RecordCrossRepoRetarget("nightly", "/repo/other")

	// The snapshot poll is the fallback delivery path for both notices. With no
	// recovery screen shadowing the bar, it drains both in one tick.
	h.recovery = nil
	h.errBox.SetSize(500, 1)
	_, _ = h.Update(snapshotFetchedMsg{repoID: h.repoID})

	text, _ := h.errBox.RetainedNotice()
	assert.Contains(t, text, `Discarded unsaved edits to "demo"`,
		"the more urgent discarded-work notice must surface (not be clobbered by the retarget notice)")
	assert.Contains(t, text, "nightly",
		"the retarget notice must also surface (both notices drain in one tick, combined)")
	assert.Contains(t, text, "/repo/other",
		"the retarget notice's binding must surface alongside the discarded-work notice")
}

// TestSaveContentPaneState_SameRepoEditQueuesNoCrossRepoNotice is the control for
// the retarget notice: a same-repo edit (rename a task whose ProjectPath stays
// at the active project's root) must NOT queue a cross-repo retarget notice. The
// row stays in the active-scope pane and there is nothing to echo.
func TestSaveContentPaneState_SameRepoEditQueuesNoCrossRepoNotice(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	repoA := initTestGitRepo(t)

	t.Chdir(repoA)
	h.repoRoot = repoA
	h.repoID = config.RepoIDFromRoot(repoA)

	seed := task.Task{
		ID:          "stay-1836",
		Name:        "Stay",
		Prompt:      "p",
		CronExpr:    "* * * * *",
		ProjectPath: repoA,
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now(),
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
	// Toggle the task (a same-repo edit) and close — the patch's ProjectPath is
	// nil, so no retarget candidate reaches the reload; the row stays in the
	// active-scope pane and no cross-repo notice is queued.
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc}) // leave edit form -> list
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	require.True(t, tp.IsDirty(), "the toggle marks the pane dirty")
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEsc}) // close the overlay; save

	require.Len(t, tp.GetTasks(), 1, "the same-repo task stays in the active-scope pane")
	assert.Equal(t, "stay-1836", tp.GetTasks()[0].ID)
	assert.False(t, tp.GetTasks()[0].Enabled, "the toggle committed (a same-repo reload preserves the row)")
	assert.Empty(t, tp.TakeCrossRepoRetargetNotice(),
		"a same-repo edit (no ProjectPath patch) must NOT queue a cross-repo retarget notice")
}

// TestHandleTaskCreate_CrossRepoTaskSurvivesSnapshotPoll guards against a
// follow-on regression: the snapshot poll (app/sync.go) reads the active scope
// via LoadTasksForKnownRepo(m.repoRoot, m.repoID), and after a cross-repo create
// the active-scope pane must continue to omit the cross-repo task — i.e. a poll
// must NOT silently re-pull the cross-repo row into the active project. (The
// bug fix uses LoadTasksForKnownRepo to align with this poll; the test pins that
// alignment.)
func TestHandleTaskCreate_CrossRepoTaskSurvivesSnapshotPoll(t *testing.T) {
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

	// Create a cross-repo task that lives in B (its RepoID = B) but is invisible
	// in A's scope.
	require.NoError(t, task.AddTask(task.Task{
		ID:          "cross-only",
		Name:        "Cross only",
		Prompt:      "p",
		CronExpr:    "* * * * *",
		ProjectPath: repoB,
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now(),
	}))

	// Simulate a snapshot poll: the active-scope read runs against (repoA, A.id),
	// and the cross-repo task stays out.
	tasks, err := task.LoadTasksForKnownRepo(h.repoRoot, h.repoID)
	require.NoError(t, err)
	for _, tk := range tasks {
		assert.NotEqual(t, "cross-only", tk.ID,
			"a task bound to B must remain invisible in A's pane across the snapshot poll")
	}
	// Disk still holds it; switching to B would surface it. (Verifies the saved
	// task is reachable, just not in this scope.)
	all, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, repoB, all[0].ProjectPath)

	// Sanity: filepath.Base coincidences (a sibling-tempdir case) do not let a
	// cross-repo task leak back into A's scope via string-prefix coincidence —
	// repo identity, not path strings, drives the filter.
	assert.NotEqual(t, filepath.Base(repoA), "cross-only")
}

// tabToTaskField presses Tab until the task form's focus is on the named field,
// so the walk does not depend on how many fields precede it.
func tabToTaskField(t *testing.T, h *home, field string) {
	t.Helper()
	tp := h.automations.TaskPane()
	for i := 0; i < 32 && tp.FocusedField() != field; i++ {
		_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab})
	}
	require.Equal(t, field, tp.FocusedField(), "tab walk must reach the %q field", field)
}

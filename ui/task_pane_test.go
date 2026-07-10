package ui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newGitRepo creates a throwaway git repository and returns its absolute path.
// validateForm now requires a task's project path to be a real git repo (#924),
// so the create/edit success-path tests need a genuine repo rather than a
// literal placeholder like "/tmp/repo".
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

// TestTaskPaneSetTasksEmptyResetsSelectedIdx verifies that calling SetTasks
// with an empty slice leaves selectedIdx at a valid value (0) rather than -1.
// Regression test for #251.
func TestTaskPaneSetTasksEmptyResetsSelectedIdx(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "a"},
		{ID: "b"},
	})

	// Move selection off index 0 so the clamp logic applies.
	tp.selectedIdx = 1

	// External modification empties the list.
	tp.SetTasks([]task.Task{})
	assert.Equal(t, 0, tp.selectedIdx, "selectedIdx should reset to 0 for an empty list")
}

// TestTaskPaneSetTasksClampsSelectedIdx verifies the existing clamp behavior
// when shrinking a non-empty list.
func TestTaskPaneSetTasksClampsSelectedIdx(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "a"},
		{ID: "b"},
		{ID: "c"},
	})
	tp.selectedIdx = 2

	tp.SetTasks([]task.Task{{ID: "a"}})
	assert.Equal(t, 0, tp.selectedIdx)
}

// TestTaskPaneConsumeDirtyTracksOnlyEditedTasks is the pane-level regression
// guard for #1213: only tasks the user actually edited (toggle or field edit)
// are returned by ConsumeDirty, so the save path never rewrites unmodified
// tasks. A no-edit pane returns nothing; toggling one task in a two-task pane
// returns only that task.
func TestTaskPaneConsumeDirtyTracksOnlyEditedTasks(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "a", Name: "A", Enabled: true},
		{ID: "b", Name: "B", Enabled: true},
	})
	tp.SetFocus(true)

	// Nothing edited yet.
	assert.Empty(t, tp.ConsumeDirty(), "an unedited pane must have no dirty tasks")
	assert.False(t, tp.IsDirty())

	// Toggle only task A (selectedIdx defaults to 0).
	assert.True(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	assert.True(t, tp.IsDirty(), "toggling a task marks the pane dirty")

	dirty := tp.ConsumeDirty()
	require.Len(t, dirty, 1, "only the toggled task must be dirty")
	assert.Equal(t, "a", dirty[0].ID)
	assert.False(t, dirty[0].Enabled, "the dirty task carries the toggled value")

	// ConsumeDirty clears the set: a second call returns nothing.
	assert.Empty(t, tp.ConsumeDirty(), "ConsumeDirty must clear the dirty set")
}

// TestTaskPaneConsumeDirtyExcludesDeletedTask verifies that a task edited and
// then deleted is not returned by ConsumeDirty — deletion is handled by
// ConsumeDeleted, and updating a just-removed task would log a spurious
// not-found error (#1213 / #763).
func TestTaskPaneConsumeDirtyExcludesDeletedTask(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "a", Name: "A", Enabled: true},
		{ID: "b", Name: "B", Enabled: true},
	})
	tp.SetFocus(true)

	// Toggle A (dirty), then delete A.
	assert.True(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}))
	assert.True(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")}))

	assert.Empty(t, tp.ConsumeDirty(), "a deleted task must not appear in the dirty update set")
	deleted := tp.ConsumeDeleted()
	require.Len(t, deleted, 1)
	assert.Equal(t, "a", deleted[0].ID)
}

// TestTaskPaneConsumePendingTriggerEmpty verifies that ConsumePendingTrigger
// returns nil (instead of panicking) when the task list is empty, even if
// selectedIdx is negative. Regression test for #251.
func TestTaskPaneConsumePendingTriggerEmpty(t *testing.T) {
	tp := NewTaskPane()
	// Simulate the legacy broken state where selectedIdx was set to -1.
	tp.selectedIdx = -1
	tp.pendingTrigger = true

	assert.NotPanics(t, func() {
		got := tp.ConsumePendingTrigger()
		assert.Nil(t, got)
	})
	assert.False(t, tp.pendingTrigger, "pendingTrigger should be cleared")
}

// TestTaskPaneConsumePendingTriggerReturnsSelected verifies that
// ConsumePendingTrigger still returns the selected task when valid.
func TestTaskPaneConsumePendingTriggerReturnsSelected(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "a"},
		{ID: "b"},
	})
	tp.selectedIdx = 1
	tp.runSelectedTask()

	got := tp.ConsumePendingTrigger()
	if assert.NotNil(t, got) {
		assert.Equal(t, "b", got.ID)
	}
	assert.False(t, tp.pendingTrigger)
}

// TestTaskPaneConsumePendingTriggerResolvesByIDAfterReload verifies that a
// pending run-now intent survives a task reload by task ID, not by stale index.
func TestTaskPaneConsumePendingTriggerResolvesByIDAfterReload(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "a"},
		{ID: "b"},
	})
	tp.SelectTask(1)
	tp.runSelectedTask()

	tp.SetTasks([]task.Task{
		{ID: "b"},
		{ID: "a"},
	})

	got := tp.ConsumePendingTrigger()
	if assert.NotNil(t, got) {
		assert.Equal(t, "b", got.ID)
	}
	assert.False(t, tp.pendingTrigger)
	assert.Empty(t, tp.pendingTriggerID)
}

func TestTaskPaneNormalModeAllowsQuitKeysToPropagate(t *testing.T) {
	tp := NewTaskPane()
	tp.SetFocus(true)

	assert.False(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}))
	assert.False(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlC}))
}

func TestTaskPaneNormalModeAllowsReboundQuitKeyToPropagate(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(map[string][]string{"quit": {"Q"}}))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	tp := NewTaskPane()
	tp.SetFocus(true)

	assert.False(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Q")}))
	assert.True(t, tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}),
		"the old default key must not keep propagating after quit is rebound")
}

// tabTo advances the edit-form focus from its current position by pressing
// Tab the given number of times.
func tabTo(tp *TaskPane, presses int) {
	for i := 0; i < presses; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	}
}

// fillCreateForm types a name, cron expression, and prompt into the create
// form (the trigger selector defaults to cron) so submitting via the Save
// button doesn't trip validation. Leaves focus back on index 0 (Name) so
// callers can walk to whichever field they want to drive next.
func fillCreateForm(t *testing.T, tp *TaskPane, name string) {
	t.Helper()
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)})
	tabTo(tp, 2) // name -> trigger selector (cron stays selected) -> cron value
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("* * * * *")})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do something")})
	// Walk back to name (index 0) so callers can navigate forward consistently.
	for i := 0; i < taskFocusPrompt; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab})
	}
}

// TestTaskPaneCreateModeRejectsEmptyPrompt is the regression guard for #517:
// submitting the create form for a cron task with no prompt (or
// whitespace-only) must surface an inline validation error instead of marking
// a pending create with a blank prompt that no-ops when the scheduler fires.
func TestTaskPaneCreateModeRejectsEmptyPrompt(t *testing.T) {
	for _, prompt := range []string{"", "   "} {
		t.Run("prompt="+prompt, func(t *testing.T) {
			tp := NewTaskPane()
			tp.EnterCreateMode("/tmp/repo")

			// Fill name and cron, leave prompt empty/whitespace.
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("daily")})
			tabTo(tp, 2) // -> trigger -> cron value
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("* * * * *")})
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
			if prompt != "" {
				tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(prompt)})
			}

			// Walk to Save and submit.
			tabTo(tp, taskFocusSave-taskFocusPrompt)
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

			assert.False(t, tp.HasPendingCreate(), "empty prompt must not produce a pending create")
			assert.True(t, tp.IsCreating(), "form must stay open so user can fix the error")
			assert.Equal(t, "prompt must be non-empty", tp.editError,
				"inline validation error must surface to the user")
			assert.Equal(t, taskFocusPrompt, tp.editErrorField,
				"the error must render under the Prompt field")
		})
	}
}

// TestTaskPaneCreateModeRejectsWhitespaceName guards the adjacent-call-site
// audit for #870: like the watch/cron/prompt fields, a whitespace-only task
// name must be rejected rather than persisted as a blank name.
func TestTaskPaneCreateModeRejectsWhitespaceName(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode("/tmp/repo")

	// Whitespace-only name, otherwise-valid cron + prompt.
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("   ")})
	tabTo(tp, 2) // -> trigger -> cron value
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("* * * * *")})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do work")})

	tabTo(tp, taskFocusSave-taskFocusPrompt)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.HasPendingCreate(), "whitespace-only name must not produce a pending create")
	assert.True(t, tp.IsCreating(), "form must stay open so the user can fix the error")
	assert.Equal(t, "name is required", tp.editError,
		"inline validation error must surface to the user")
	assert.Equal(t, taskFocusName, tp.editErrorField,
		"the error must render under the Name field")
}

// TestTaskPaneCreateModeSelectorDefaultsToConfigDefault verifies that creating
// a new task without touching the Program selector persists "" so the daemon
// uses the configured default_program. Regression test for #492.
func TestTaskPaneCreateModeSelectorDefaultsToConfigDefault(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode(newGitRepo(t))
	fillCreateForm(t, tp, "daily")

	// Walk to the Save button without touching the Program selector.
	tabTo(tp, taskFocusCount-1)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.HasPendingCreate(), "submit should mark a pending create")
	_, _, _, _, _, _, program := tp.ConsumePendingCreate()
	assert.Equal(t, "", program, "default selector option must persist an empty Program")
}

// TestTaskPaneCreateModeSelectorPicksCanonicalAgent verifies that advancing
// the Program selector to a SupportedPrograms entry persists the canonical
// bare name (no path, no flags). Regression test for #492.
func TestTaskPaneCreateModeSelectorPicksCanonicalAgent(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode(newGitRepo(t))
	fillCreateForm(t, tp, "daily")

	// Walk to the Program field and step the selector to "claude"
	// (option index 1: 0 is the default sentinel, 1 is the first supported).
	tabTo(tp, taskFocusProgram)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	// Advance to the Save button and submit.
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.HasPendingCreate(), "submit should mark a pending create")
	_, _, _, _, _, _, program := tp.ConsumePendingCreate()
	assert.Equal(t, "claude", program, "selector must persist the canonical agent name")
}

// TestTaskPaneEditModePresetFromExistingProgram verifies that opening edit
// mode on a task whose Program already matches a SupportedPrograms entry
// pre-selects that option so saving without changes is a no-op. Regression
// test for #492.
func TestTaskPaneEditModePresetFromExistingProgram(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "* * * * *",
		ProjectPath: newGitRepo(t),
		Program:     "amp",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	assert.True(t, tp.IsEditing())

	// Tab to Save and submit without touching the selector.
	tabTo(tp, taskFocusCount-1)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.IsEditing(), "save should exit edit mode")
	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, "amp", tasks[0].Program,
			"pre-selected canonical option must round-trip the original value")
	}
}

// TestTaskPaneEditModeCollapsesLegacyProgramToDefault verifies that opening
// edit mode on a task whose Program is a legacy free-text command collapses
// the selector to the "(use config default)" sentinel. Saving rewrites the
// Program to the empty string — the on-disk legacy value is not preserved
// because per-task Program is now enum-only (#658) and save-side enum
// validation would reject any free-text value. Users must explicitly choose
// a SupportedPrograms entry (or accept the config default) on edit.
func TestTaskPaneEditModeCollapsesLegacyProgramToDefault(t *testing.T) {
	tp := NewTaskPane()
	const legacy = "/usr/local/bin/aider --model gpt-4"
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "* * * * *",
		ProjectPath: newGitRepo(t),
		Program:     legacy,
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	assert.True(t, tp.IsEditing())

	// Tab to Save and submit without touching the selector.
	tabTo(tp, taskFocusCount-1)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.IsEditing(), "save should exit edit mode")
	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, "", tasks[0].Program,
			"legacy free-text program must collapse to the default sentinel")
	}
}

// TestTaskPaneEnterEditSelectedEntersEditWhenTaskExists is the #1249 unit
// guard: EnterEditSelected drops straight into the edit form for the selected
// task, so the overlay can open a task into its editable config in a single
// action rather than list-then-enter.
func TestTaskPaneEnterEditSelectedEntersEditWhenTaskExists(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "* * * * *",
		ProjectPath: newGitRepo(t),
		Program:     "claude",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.EnterEditSelected()
	assert.True(t, tp.IsEditing(), "a single action must land directly in edit mode")
}

// TestTaskPaneEnterEditSelectedNoopsOnEmptyList verifies the empty-list guard:
// with no tasks there is nothing to edit, so EnterEditSelected stays in list
// mode where `n` can create the first task (and never indexes an empty slice).
func TestTaskPaneEnterEditSelectedNoopsOnEmptyList(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{})
	tp.SetFocus(true)
	tp.EnterEditSelected()
	assert.False(t, tp.IsEditing(), "empty list must stay in list mode so `n` works")
}

// TestTaskPaneEnterEditSelectedThenEscLeavesNoDirty is the #1213 guarantee for
// the #1249 auto-open: merely opening a task into its edit form (then backing
// out with Esc) must not mark the task dirty, so save-on-exit won't rewrite an
// otherwise-untouched task over a concurrent CLI/daemon change.
func TestTaskPaneEnterEditSelectedThenEscLeavesNoDirty(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "* * * * *",
		ProjectPath: newGitRepo(t),
		Program:     "claude",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.EnterEditSelected()
	assert.True(t, tp.IsEditing())

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEsc}) // back out to list mode
	assert.False(t, tp.IsEditing(), "Esc must return to the list")
	assert.True(t, tp.HasFocus(), "Esc in edit mode keeps the overlay open")
	assert.False(t, tp.IsDirty(), "auto-opening then cancelling must not mark the task dirty")
	assert.Empty(t, tp.ConsumeDirty(), "no task should be persisted after a no-op open")
}

// TestTaskPaneEditModeKeepsListActionsReachable is the #1288 guard: the
// #1249 one-step edit flow must not hide the documented list verbs. Opening a
// selected task directly into edit mode still lets the user run, toggle, or
// delete that selected task without first knowing to press Esc.
func TestTaskPaneEditModeKeepsListActionsReachable(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "* * * * *",
		ProjectPath: newGitRepo(t),
		Program:     "claude",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.EnterEditSelected()
	require.True(t, tp.IsEditing())

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	require.True(t, tp.HasPendingTrigger(), "r must remain reachable from edit mode")
	pending := tp.ConsumePendingTrigger()
	require.NotNil(t, pending)
	assert.Equal(t, "abc", pending.ID)
	assert.True(t, tp.IsEditing(), "run-now leaves the edit form in place until the app consumes it")

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	require.True(t, tp.IsEditing(), "toggle should not kick the user out of edit")
	require.True(t, tp.IsDirty(), "toggle from edit mode marks the task dirty")
	assert.False(t, tp.GetTasks()[0].Enabled)
	dirty := tp.ConsumeDirty()
	require.Len(t, dirty, 1)
	assert.Equal(t, "abc", dirty[0].ID)

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})
	assert.False(t, tp.IsEditing(), "deleting the task being edited returns to list mode")
	assert.Empty(t, tp.GetTasks())
	deleted := tp.ConsumeDeleted()
	require.Len(t, deleted, 1)
	assert.Equal(t, "abc", deleted[0].ID)
}

// TestTaskPaneEditModeCtrlCCancels is the regression guard for #526: Ctrl+C
// inside the edit form must cancel the edit (matching Esc) instead of being
// silently swallowed. Dirty buffer changes must not be written back.
func TestTaskPaneEditModeCtrlCCancels(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 0 * * *",
		ProjectPath: "/tmp/repo",
		Program:     "claude",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	assert.True(t, tp.IsEditing())

	// Dirty the Name buffer so we can prove the cancel doesn't persist edits.
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("XXX")})

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlC})

	assert.False(t, tp.IsEditing(), "Ctrl+C should exit edit mode")
	assert.Equal(t, "", tp.editError, "Ctrl+C should clear any inline error")
	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, "nightly", tasks[0].Name,
			"Ctrl+C must not write the dirty Name buffer back to the task")
	}
}

// TestTaskPaneCreateModeCtrlCCancels mirrors the edit-mode regression guard
// for the create form: Ctrl+C must exit create mode without producing a
// pending create. Regression test for #526.
func TestTaskPaneCreateModeCtrlCCancels(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode("/tmp/repo")
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("draft")})

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlC})

	assert.False(t, tp.IsCreating(), "Ctrl+C should exit create mode")
	assert.False(t, tp.HasPendingCreate(), "Ctrl+C must not produce a pending create")
}

// editPathTo opens edit mode on the first task and overwrites the Path field
// with newPath, then saves. Used by the ProjectPath-normalization regression
// tests to drive only the Path input without disturbing the other fields.
func editPathTo(t *testing.T, tp *TaskPane, newPath string) {
	t.Helper()
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	if !tp.IsEditing() {
		t.Fatalf("expected edit mode")
	}
	// Tab Name -> Trigger -> Trigger value -> Prompt -> Target -> Path
	tabTo(tp, taskFocusPath)
	// Clear current value then type the new one.
	for range tp.editPath.Value() {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	if newPath != "" {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(newPath)})
	}
	// Tab Path -> Program -> Save, then submit.
	tabTo(tp, taskFocusSave-taskFocusPath)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
}

// TestTaskPaneEditModeRejectsEmptyPath supersedes the #641 empty→CWD behavior:
// since #924 the form validates the path on save, so clearing ProjectPath now
// surfaces an inline error under the Path field instead of silently defaulting
// to the CWD. The rejected edit must not be written back.
func TestTaskPaneEditModeRejectsEmptyPath(t *testing.T) {
	repo := newGitRepo(t)
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 0 * * *",
		ProjectPath: repo,
		Program:     "claude",
		Enabled:     true,
	}})

	editPathTo(t, tp, "")

	assert.True(t, tp.IsEditing(), "form must stay open so the user can fix the error")
	assert.Equal(t, "project path is required", tp.editError)
	assert.Equal(t, taskFocusPath, tp.editErrorField)
	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, repo, tasks[0].ProjectPath,
			"rejected edit must not overwrite the stored path")
	}
}

// TestTaskPaneEditModeNormalizesRelativePath verifies that editing
// ProjectPath to a relative value resolves it via filepath.Abs at save
// time, mirroring the create path (#641). Since #924 the resolved path must
// also be a real git repo, so the relative input points into a temp repo.
func TestTaskPaneEditModeNormalizesRelativePath(t *testing.T) {
	repo := newGitRepo(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	rel, err := filepath.Rel(cwd, repo)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}

	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 0 * * *",
		ProjectPath: repo,
		Program:     "claude",
		Enabled:     true,
	}})

	editPathTo(t, tp, rel)

	assert.False(t, tp.IsEditing(), "a relative path into a git repo must save")
	assert.Equal(t, "", tp.editError)
	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, repo, tasks[0].ProjectPath,
			"relative ProjectPath must be resolved via filepath.Abs on save")
	}
}

// TestTaskPaneEditModeKeepsAbsolutePath verifies that an already-absolute
// ProjectPath pointing at a git repo is preserved verbatim across edit/save
// (#641, tightened by #924's git-repo validation).
func TestTaskPaneEditModeKeepsAbsolutePath(t *testing.T) {
	oldRepo := newGitRepo(t)
	newRepo := newGitRepo(t)
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 0 * * *",
		ProjectPath: oldRepo,
		Program:     "claude",
		Enabled:     true,
	}})

	editPathTo(t, tp, newRepo)

	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, newRepo, tasks[0].ProjectPath,
			"absolute ProjectPath must be preserved verbatim across edit/save")
	}
}

// TestTaskPaneListShowsAgentName confirms the selected row's detail line
// renders the per-task agent enum name (#658 collapsed Program to the enum,
// so the rendering is now a straight lookup).
func TestTaskPaneListShowsAgentName(t *testing.T) {
	tp := NewTaskPane()
	tp.SetSize(80, 24)
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 0 * * *",
		ProjectPath: "/tmp/repo",
		Program:     "aider",
		Enabled:     true,
	}})

	out := tp.String()
	assert.Contains(t, out, "aider", "selected row detail should render the agent name")
}

// TestTaskPaneListShowsConfigDefaultWhenProgramEmpty confirms a task with no
// per-task Program shows the "(use config default)" label rather than an
// empty agent name.
func TestTaskPaneListShowsConfigDefaultWhenProgramEmpty(t *testing.T) {
	tp := NewTaskPane()
	tp.SetSize(80, 24)
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 0 * * *",
		ProjectPath: "/tmp/repo",
		Program:     "",
		Enabled:     true,
	}})

	out := tp.String()
	assert.Contains(t, out, programDefaultLabel,
		"selected row detail should render the config-default sentinel for empty Program")
}

// TestTaskPaneConsumeDeletedClearsState verifies that deleting a task and then
// consuming the deletion clears both the pending-deletion slice and the dirty
// flag, so a second save pass finds nothing to reprocess. Regression test for
// #763: saveContentPaneState previously read GetDeleted() without clearing it,
// so pressing ESC (save) twice re-ran the deletion loop on an already-removed
// task, tripping the rollback path that re-installs an orphaned scheduler.
func TestTaskPaneConsumeDeletedClearsState(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{
		{ID: "a", Name: "alpha"},
		{ID: "b", Name: "beta"},
	})

	// Select and delete the first task (the "D" key in normal mode). The pane
	// only handles keys while focused.
	tp.SetFocus(true)
	tp.selectedIdx = 0
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})

	assert.True(t, tp.IsDirty(), "deleting a task must mark the pane dirty")

	// First save consumes the deletion: the deleted task is returned exactly
	// once, and the pane state is cleared.
	first := tp.ConsumeDeleted()
	if assert.Len(t, first, 1, "first save should surface the deleted task once") {
		assert.Equal(t, "a", first[0].ID)
	}
	assert.False(t, tp.IsDirty(),
		"consuming the deletion must clear the dirty flag so updates aren't re-run")

	// Second save (e.g. ESC pressed again) must find nothing to process, so the
	// deletion loop in saveContentPaneState never re-runs RemoveTask on
	// records that no longer exist.
	second := tp.ConsumeDeleted()
	assert.Empty(t, second,
		"second save must not reprocess an already-deleted task (#763)")
}

// TestTaskPaneCreateModeInactiveTriggerNotSaved pins the structural
// exactly-one trigger contract (#782): the trigger-type selector decides
// which buffer is saved, so a stale value left in the inactive trigger's
// buffer can never leak into the created task — the old "mutually exclusive"
// validation error is unrepresentable.
func TestTaskPaneCreateModeInactiveTriggerNotSaved(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode(newGitRepo(t))
	fillCreateForm(t, tp, "both") // name, cron, prompt — cron type selected

	// Flip the trigger type to watch and fill the watch command. The cron
	// buffer still holds "* * * * *".
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> trigger selector
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> watch value
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tail -F log")})
	tabTo(tp, taskFocusSave-taskFocusTriggerValue)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.HasPendingCreate(), "a valid watch task must submit")
	_, _, cron, watchCmd, _, _, _ := tp.ConsumePendingCreate()
	assert.Equal(t, "", cron, "the inactive cron buffer must not be saved")
	assert.Equal(t, "tail -F log", watchCmd)
}

// TestTaskPaneCreateModeRejectsEmptyCron covers the cron half of the trigger
// contract: submitting a cron-type task with no expression must surface an
// inline error under the trigger field instead of creating an unfireable task.
func TestTaskPaneCreateModeRejectsEmptyCron(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode("/tmp/repo")

	// Name and prompt only — no cron expression.
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("untriggered")})
	tabTo(tp, taskFocusPrompt)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do something")})
	tabTo(tp, taskFocusSave-taskFocusPrompt)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.HasPendingCreate(), "no trigger must not produce a pending create")
	assert.True(t, tp.IsCreating(), "form must stay open so user can fix the error")
	assert.Equal(t, "cron expression is required", tp.editError)
	assert.Equal(t, taskFocusTriggerValue, tp.editErrorField)
}

// TestTaskPaneCreateModeRejectsEmptyWatch covers the watch half: a watch-type
// task with no command must surface the inline error under the trigger field.
func TestTaskPaneCreateModeRejectsEmptyWatch(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode("/tmp/repo")

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("untriggered")})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> trigger selector
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	tabTo(tp, taskFocusSave-taskFocusTrigger)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.HasPendingCreate(), "no trigger must not produce a pending create")
	assert.True(t, tp.IsCreating(), "form must stay open so user can fix the error")
	assert.Equal(t, "watch command is required", tp.editError)
	assert.Equal(t, taskFocusTriggerValue, tp.editErrorField)
}

// TestTaskPaneCreateModeWatchTask drives a full watch-task create: trigger
// type switched to watch, watch cmd and target session filled, prompt left
// empty (a watch task may omit it — each event defaults to the raw emitted
// line). The pending create must carry the new fields and no cron.
func TestTaskPaneCreateModeWatchTask(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode(newGitRepo(t))

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("gh-issues")})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> trigger selector
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> watch value
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("gh-issue-watch.sh")})
	tabTo(tp, taskFocusTarget-taskFocusTriggerValue)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("captain")})
	tabTo(tp, taskFocusSave-taskFocusTarget)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.HasPendingCreate(), "a watch task with an empty prompt is valid")
	assert.Equal(t, "", tp.editError)
	_, prompt, cron, watchCmd, targetSession, _, _ := tp.ConsumePendingCreate()
	assert.Equal(t, "", prompt)
	assert.Equal(t, "", cron)
	assert.Equal(t, "gh-issue-watch.sh", watchCmd)
	assert.Equal(t, "captain", targetSession)
}

// TestValidateFormPathValidation pins the #924 path contract: validateForm
// rejects an empty, non-git, or unresolved-tilde project path for BOTH cron
// and watch tasks, and accepts a real git repo — storing the expanded absolute
// form so what is validated is exactly what gets persisted.
func TestValidateFormPathValidation(t *testing.T) {
	repo := newGitRepo(t)
	nonRepo := t.TempDir() // a real directory that is not a git repo

	newForm := func(trigger, path string) *TaskPane {
		tp := NewTaskPane()
		tsk := task.Task{Name: "t", ProjectPath: path}
		if trigger == "watch" {
			tsk.WatchCmd = "watch.sh"
		} else {
			tsk.CronExpr = "0 0 * * *"
			tsk.Prompt = "do it"
		}
		tp.initForm(&tsk, "")
		return tp
	}

	for _, trigger := range []string{"cron", "watch"} {
		t.Run(trigger, func(t *testing.T) {
			// Empty path rejected with focus on the path field.
			msg, field := newForm(trigger, "").validateForm()
			assert.Equal(t, "project path is required", msg)
			assert.Equal(t, taskFocusPath, field)

			// A tilde path that expands to a non-repo (home dir) is rejected —
			// proving the value is expanded (not treated literally) and then
			// validated.
			msg, field = newForm(trigger, "~/definitely-not-a-repo-924").validateForm()
			assert.NotEqual(t, "", msg, "a non-git tilde path must be rejected")
			assert.Equal(t, taskFocusPath, field)

			// A non-git absolute path is rejected.
			msg, field = newForm(trigger, nonRepo).validateForm()
			assert.NotEqual(t, "", msg, "a non-git path must be rejected")
			assert.Equal(t, taskFocusPath, field)

			// A valid git repo is accepted and stored normalized.
			tp := newForm(trigger, repo)
			msg, field = tp.validateForm()
			assert.Equal(t, "", msg, "a valid git repo path must be accepted")
			assert.Equal(t, -1, field)
			assert.Equal(t, repo, tp.editPath.Value(),
				"the validated path must be persisted in its normalized form")

			// A "~" path expanding to a git repo is accepted, proving tilde
			// expansion flows through validateForm and the stored value is the
			// expanded absolute path.
			t.Setenv("HOME", repo)
			tp = newForm(trigger, "~")
			msg, field = tp.validateForm()
			assert.Equal(t, "", msg, "a tilde path resolving to a git repo must be accepted")
			assert.Equal(t, -1, field)
			assert.Equal(t, repo, tp.editPath.Value(),
				"a leading ~ must be expanded to the home dir before storing")
		})
	}
}

// TestTaskPaneEditModeSwitchCronToWatch verifies the edit form can retrigger
// an existing cron task as a watch task: flipping the trigger selector to
// watch and filling the command must save WatchCmd and clear CronExpr — the
// stale cron buffer must not leak into the save (phase 3 of #782 replaced the
// phase-2 stopgap that froze watch-task triggers in the TUI).
func TestTaskPaneEditModeSwitchCronToWatch(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 0 * * *",
		ProjectPath: newGitRepo(t),
		Program:     "claude",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	assert.True(t, tp.IsEditing())

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> trigger selector
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> watch value
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tail -F app.log")})
	tabTo(tp, taskFocusSave-taskFocusTriggerValue)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.IsEditing(), "save should exit edit mode")
	assert.Equal(t, "", tp.editError)
	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, "", tasks[0].CronExpr)
		assert.Equal(t, "tail -F app.log", tasks[0].WatchCmd)
	}
}

// TestTaskPaneEditModeSelectorPresetsFromWatchTask verifies that opening edit
// mode on a watch task pre-selects the watch trigger type, so saving without
// touching the selector round-trips the trigger unchanged.
func TestTaskPaneEditModeSelectorPresetsFromWatchTask(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "gh-issues",
		WatchCmd:    "gh-issue-watch.sh",
		ProjectPath: newGitRepo(t),
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, tp.IsEditing())
	assert.True(t, tp.editTriggerIsWatch, "watch task must preset the watch trigger type")

	tabTo(tp, taskFocusCount-1)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.IsEditing(), "save should exit edit mode")
	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, "", tasks[0].CronExpr)
		assert.Equal(t, "gh-issue-watch.sh", tasks[0].WatchCmd)
	}
}

// TestTaskPaneEditModeRejectsEmptyWatch verifies the edit form surfaces an
// inline error when the trigger type is flipped to watch without a command,
// and the rejected edit is not written back.
func TestTaskPaneEditModeRejectsEmptyWatch(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 0 * * *",
		ProjectPath: "/tmp/repo",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, tp.IsEditing())

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab}) // -> trigger selector
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	tabTo(tp, taskFocusSave-taskFocusTrigger)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.IsEditing(), "form must stay open so user can fix the error")
	assert.Equal(t, "watch command is required", tp.editError)
	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, "0 0 * * *", tasks[0].CronExpr, "rejected edit must not be written back")
		assert.Equal(t, "", tasks[0].WatchCmd, "rejected edit must not be written back")
	}
}

// TestTaskPaneListRendersWatchStatus pins the list-row surface for watch
// tasks (#782 phase 3, tightened by #801): the trigger column shows the watch
// command and the status derived from the daemon-persisted fields — watching
// (enabled, no terminal status), errored (crash loop), or stopped (clean exit
// or disabled). The #797 failure summary gets its own detail line only when
// errored; other statuses appear in short form on the selected row's detail.
func TestTaskPaneListRendersWatchStatus(t *testing.T) {
	cases := []struct {
		name          string
		enabled       bool
		lastRunStatus string
		want          string
		wantDetail    string // short status on the selected row's detail line
	}{
		{"enabled fresh", true, "", "[watching]", ""},
		{"enabled delivering", true, "sent", "[watching]", "(sent)"},
		{"crash loop", true, "errored", "[errored]", "(errored)"},
		{"crash loop with summary", true, "errored: exit status 1: WARN lock held", "[errored]", "(errored)"},
		{"clean exit", true, "stopped", "[stopped]", "(stopped)"},
		{"disabled", false, "sent", "[stopped]", "(sent)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tp := NewTaskPane()
			tp.SetSize(120, 24)
			tp.SetTasks([]task.Task{{
				ID:            "abc",
				Name:          "gh-issues",
				WatchCmd:      "gh-issue-watch.sh",
				TargetSession: "captain",
				ProjectPath:   "/tmp/repo",
				Enabled:       c.enabled,
				LastRunStatus: c.lastRunStatus,
			}})

			out := tp.String()
			assert.Contains(t, out, "watch: gh-issue-watch.sh",
				"watch rows must show the trigger kind and command")
			assert.Contains(t, out, c.want)
			assert.Contains(t, out, "→ captain",
				"rows must surface the target session delivery")
			if c.wantDetail != "" {
				assert.Contains(t, out, c.wantDetail,
					"the selected row's detail must show the short status")
			}
		})
	}
}

// TestTaskPaneListShowsErroredSummaryLine pins the #797 surface: a
// crash-looped watcher's full failure summary renders on its own detail line.
func TestTaskPaneListShowsErroredSummaryLine(t *testing.T) {
	tp := NewTaskPane()
	tp.SetSize(120, 24)
	tp.SetTasks([]task.Task{{
		ID:            "abc",
		Name:          "gh-issues",
		WatchCmd:      "gh-issue-watch.sh",
		ProjectPath:   "/tmp/repo",
		Enabled:       true,
		LastRunStatus: "errored: exit status 1: WARN lock held",
	}})

	out := tp.String()
	assert.Contains(t, out, "errored: exit status 1: WARN lock held",
		"the errored detail line must render the #797 failure summary in full")
}

// TestTaskPaneListCronRowHasNoWatchStatus confirms cron rows render the cron
// expression, the create-per-run delivery label, and no watch-status bracket.
func TestTaskPaneListCronRowHasNoWatchStatus(t *testing.T) {
	tp := NewTaskPane()
	tp.SetSize(120, 24)
	tp.SetTasks([]task.Task{{
		ID:       "abc",
		Name:     "nightly",
		Prompt:   "do it",
		CronExpr: "0 0 * * *",
		Enabled:  true,
	}})

	out := tp.String()
	assert.Contains(t, out, "0 0 * * *")
	assert.Contains(t, out, "new session")
	assert.NotContains(t, out, "[watching]")
	assert.NotContains(t, out, "→ ")
}

// TestTaskPaneListPromptOnlyOnSelectedRow pins the #801 cleanup: list rows
// are one line each; the prompt renders only as the selected row's detail
// snippet, not for every task.
func TestTaskPaneListPromptOnlyOnSelectedRow(t *testing.T) {
	tp := NewTaskPane()
	tp.SetSize(120, 24)
	tp.SetTasks([]task.Task{
		{ID: "a", Name: "first", Prompt: "selected prompt body", CronExpr: "0 0 * * *", Enabled: true},
		{ID: "b", Name: "second", Prompt: "unselected prompt body", CronExpr: "0 1 * * *", Enabled: true},
	})
	tp.selectedIdx = 0

	out := tp.String()
	assert.Contains(t, out, "selected prompt body",
		"the selected row must show its prompt snippet")
	assert.NotContains(t, out, "unselected prompt body",
		"unselected rows must stay one line — no prompt body")
}

// TestTaskPaneCreateModeEnterSubmitsFromAnyField pins #1098 finding 3: the
// footer promises a blanket "enter save", but Enter used to be a dead key on
// every field except the Create/Save button. Submitting from the Name field
// must work.
func TestTaskPaneCreateModeEnterSubmitsFromAnyField(t *testing.T) {
	repo := newGitRepo(t)
	tp := NewTaskPane()
	tp.SetSize(80, 24)
	tp.EnterCreateMode(repo)
	fillCreateForm(t, tp, "enter-on-name")

	// fillCreateForm leaves focus on Name (index 0).
	assert.Equal(t, taskFocusName, tp.focusIndex)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, tp.pendingCreate, "enter on the Name field must submit a valid form")
	assert.False(t, tp.creating)
}

// TestTaskPaneCreateModeEnterMovesFocusToInvalidField: submitting an invalid
// form from any field surfaces the inline error AND moves focus to the
// offending field, so the error is in view even when the clamped form has it
// scrolled off-screen (#1098).
func TestTaskPaneCreateModeEnterMovesFocusToInvalidField(t *testing.T) {
	repo := newGitRepo(t)
	tp := NewTaskPane()
	tp.SetSize(80, 24)
	tp.EnterCreateMode(repo)
	// Name typed, cron left empty.
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("half-filled")})

	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, tp.creating, "invalid form must not submit")
	assert.False(t, tp.pendingCreate)
	assert.Equal(t, "cron expression is required", tp.editError)
	assert.Equal(t, taskFocusTriggerValue, tp.editErrorField)
	assert.Equal(t, taskFocusTriggerValue, tp.focusIndex,
		"focus must land on the offending field")
}

// TestTaskPaneCreateModeEnterInPromptInsertsNewline: the prompt textarea keeps
// Enter for newlines — the any-field submit (#1098) must not swallow it.
func TestTaskPaneCreateModeEnterInPromptInsertsNewline(t *testing.T) {
	repo := newGitRepo(t)
	tp := NewTaskPane()
	tp.SetSize(80, 24)
	tp.EnterCreateMode(repo)
	fillCreateForm(t, tp, "prompt-newline")

	tabTo(tp, taskFocusPrompt)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("line2")})
	assert.True(t, tp.creating, "enter in the prompt must not submit")
	assert.Contains(t, tp.editPrompt.Value(), "\n",
		"enter in the prompt must insert a newline")
}

func TestTaskPaneCreateModeCompactFooterShowsQuitKey(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(nil))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	tp := NewTaskPane()
	tp.SetSize(40, 10)
	tp.EnterCreateMode(newGitRepo(t))

	lines := strings.Split(tp.String(), "\n")
	assert.Contains(t, lines[len(lines)-1], "esc cancel", "cancel remains visible")
	assert.Contains(t, lines[len(lines)-1], "q quit", "configured quit key stays visible in the compact form footer")
}

func TestTaskPaneCreateModeFooterUsesReboundQuitKey(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(map[string][]string{"quit": {"Q"}}))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	tp := NewTaskPane()
	tp.SetSize(40, 10)
	tp.EnterCreateMode(newGitRepo(t))

	lines := strings.Split(tp.String(), "\n")
	assert.Contains(t, lines[len(lines)-1], "Q quit", "form footer must derive the quit hint from the keymap")
	assert.NotContains(t, lines[len(lines)-1], "q quit", "the old default quit key must not be advertised after rebinding")
}

func TestTaskPaneEditModeCompactActionFooterShowsQuitKey(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(nil))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	tp := NewTaskPane()
	tp.SetSize(40, 10)
	tp.SetTasks([]task.Task{{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 0 * * *",
		ProjectPath: newGitRepo(t),
		Program:     "claude",
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.EnterEditSelected()

	lines := strings.Split(tp.String(), "\n")
	assert.Contains(t, lines[len(lines)-1], "esc", "list escape remains visible")
	assert.Contains(t, lines[len(lines)-1], "q quit", "quit key stays visible on the pinned edit action footer")
}

// TestTaskPaneEditFormClampsToHeightWithFocusInView pins #1098 finding 1: at
// a 60x15 terminal the edit form is taller than the tasks overlay and used to
// clip off the TOP — Name/Trigger invisible while Name silently held focus.
// The form must window to the pane height, keep the focused field in view,
// pin the key-hint footer, and flag hidden fields.
func TestTaskPaneEditFormClampsToHeightWithFocusInView(t *testing.T) {
	repo := newGitRepo(t)
	tp := NewTaskPane()
	// The task pane's share of a 60x15 terminal (app sizing: 52 wide, 0.6*15 tall).
	tp.SetSize(52, 9)
	tp.EnterCreateMode(repo)

	out := tp.String()
	lines := strings.Split(out, "\n")
	assert.LessOrEqual(t, len(lines), 9, "form must clamp to the pane height")
	assert.Contains(t, out, "Name:", "the focused first field must be visible")
	assert.Contains(t, lines[len(lines)-1], "esc cancel", "key hints stay pinned")
	assert.Contains(t, out, "↓ more", "hidden fields below are flagged")

	// Walk to the Create button: it must scroll into view, pushing Name out.
	tabTo(tp, taskFocusSave)
	out = tp.String()
	lines = strings.Split(out, "\n")
	assert.LessOrEqual(t, len(lines), 9, "clamp holds while scrolled")
	assert.Contains(t, out, "Create", "the focused Save button scrolled into view")
	assert.NotContains(t, out, "Name:", "top fields scrolled out of the window")
	assert.Contains(t, out, "↑ more", "hidden fields above are flagged")
	assert.Contains(t, lines[len(lines)-1], "esc cancel", "key hints stay pinned")

	// Walking back re-scrolls the top fields into view.
	for i := 0; i < taskFocusSave; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab})
	}
	out = tp.String()
	assert.Contains(t, out, "Name:", "shift+tab back scrolls Name into view")
}

// TestTaskPaneEditFormUnclampedWhenItFits: at normal sizes the form renders
// unchanged — no window, no more-markers.
func TestTaskPaneEditFormUnclampedWhenItFits(t *testing.T) {
	repo := newGitRepo(t)
	tp := NewTaskPane()
	tp.SetSize(80, 24)
	tp.EnterCreateMode(repo)

	out := tp.String()
	assert.Contains(t, out, "Name:")
	assert.Contains(t, out, "Create")
	assert.NotContains(t, out, "↑ more")
	assert.NotContains(t, out, "↓ more")
}

// TestTaskPaneClampFormDegenerateHeights guards the clamp floor (Greptile on
// #1133): a height-1/2 pane raises the window floor to 3 lines, and a body
// shorter than the raised window must not slice past its end.
func TestTaskPaneClampFormDegenerateHeights(t *testing.T) {
	repo := newGitRepo(t)
	tp := NewTaskPane()
	tp.EnterCreateMode(repo)

	for _, h := range []int{1, 2} {
		tp.SetSize(52, h)
		assert.NotPanicsf(t, func() {
			out := tp.String()
			assert.LessOrEqualf(t, len(strings.Split(out, "\n")), 3,
				"height %d renders at most the 3-line floor", h)
		}, "height %d must not panic", h)
	}

	// Body shorter than the raised window: 2-line content at height 1 used to
	// slice body[0:2] on a 1-line body.
	tp.SetSize(52, 1)
	assert.NotPanics(t, func() {
		got := tp.clampFormToHeight("only\nhint", 0, 1)
		assert.Equal(t, "only\nhint", got, "a body shorter than the window renders unchanged")
	})
}

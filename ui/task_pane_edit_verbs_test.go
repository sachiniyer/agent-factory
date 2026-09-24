package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// typeRunes feeds each rune of s to the pane as its own KeyRunes message, so
// the single-glyph verb matching in handleEditMode (msg.String() == "r" etc.)
// is exercised exactly the way a keystroke-at-a-time user drives the form.
func typeRunes(tp *TaskPane, s string) {
	for _, c := range s {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{c}})
	}
}

// tabN advances the edit-form focus forward n stops.
func tabN(tp *TaskPane, n int) {
	for i := 0; i < n; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	}
}

// cronEditTask is a plain enabled cron task opened for editing; the project
// path is a real git repo so a save would validate, though these tests never
// submit the form.
func cronEditTask(t *testing.T) task.Task {
	t.Helper()
	return task.Task{
		ID:          "abc",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "* * * * *",
		ProjectPath: newGitRepo(t),
		Program:     "claude",
		Enabled:     true,
	}
}

// TestTaskPaneTextFocusStop pins the predicate the verb gate rests on: the
// free-text stops report true (r/x/D must type) and every selector/button stop
// reports false (r/x/D route as verbs). The cron trigger value counts as a
// text stop even though most of its cells ignore letters, because its
// Custom/raw-cron cell is a free-text input the glyphs must reach.
func TestTaskPaneTextFocusStop(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{cronEditTask(t)})
	tp.SetFocus(true)
	tp.EnterEditSelected()

	textStops := map[int]string{
		taskFocusName:         "Name",
		taskFocusTriggerValue: "TriggerValue",
		taskFocusPrompt:       "Prompt",
		taskFocusTarget:       "Target",
		taskFocusPath:         "Path",
	}
	verbStops := map[int]string{
		taskFocusTrigger:    "Trigger",
		taskFocusOnComplete: "OnComplete",
		taskFocusProgram:    "Program",
		taskFocusSave:       "Save",
	}
	for idx := 0; idx < taskFocusCount; idx++ {
		tp.focusIndex = idx
		name, isText := textStops[idx]
		if isText {
			assert.True(t, tp.textFocusStop(), "focus %d (%s) must be a text stop and accept the rune", idx, name)
			assert.True(t, tp.IsTextFieldFocused(), "IsTextFieldFocused must be true at a text stop while editing")
		} else {
			assert.False(t, tp.textFocusStop(), "focus %d (%s) must route verbs, not accept text", idx, verbStops[idx])
			assert.False(t, tp.IsTextFieldFocused(), "IsTextFieldFocused must be false at a selector/button stop")
		}
	}

	// Outside the form, IsTextFieldFocused is false regardless of focusIndex,
	// so a list-mode D is always confirmed by the app layer.
	tp.editing = false
	tp.focusIndex = taskFocusName
	assert.False(t, tp.IsTextFieldFocused(), "IsTextFieldFocused must be false in list mode")
}

// TestTaskPaneEditModeVerbGlyphsTypeIntoTextFields is the core fix: in edit
// mode r, x, and D are TYPED into the focused text field, not intercepted as
// run/toggle/delete. Each text stop is exercised on a fresh pane — Name, the
// watch command, the prompt, the target session, and the path — asserting
// every glyph was inserted and no verb fired (no run, no toggle, no delete,
// still editing).
func TestTaskPaneEditModeVerbGlyphsTypeIntoTextFields(t *testing.T) {
	cron := cronEditTask(t)
	watch := cron
	watch.ID = "watch1"
	watch.Name = "watcher"
	watch.CronExpr = ""
	watch.WatchCmd = "tail -f log"

	// tabsFromName is the Tab count from Name to the named text stop; clear
	// empties the field so the typed glyphs assert as an exact "rxD" (the
	// textinput cursor stays at its old position after a SetValue into a
	// non-empty field, and a seeded value or the temp-dir path could
	// otherwise mask or split the glyphs).
	type textCase struct {
		name  string
		tabs  int
		clear func(*TaskPane)
		value func(*TaskPane) string
		task  task.Task
	}
	cases := []textCase{
		{"Name", 0, func(tp *TaskPane) { tp.editName.SetValue("") }, func(tp *TaskPane) string { return tp.editName.Value() }, cron},
		{"Prompt", 3, func(tp *TaskPane) { tp.editPrompt.SetValue("") }, func(tp *TaskPane) string { return tp.editPrompt.Value() }, cron},
		{"Target", 4, func(tp *TaskPane) { tp.editTarget.SetValue("") }, func(tp *TaskPane) string { return tp.editTarget.Value() }, cron},
		{"Path", 6, func(tp *TaskPane) { tp.editPath.SetValue("") }, func(tp *TaskPane) string { return tp.editPath.Value() }, cron},
		{"Watch", 2, func(tp *TaskPane) { tp.editWatch.SetValue("") }, func(tp *TaskPane) string { return tp.editWatch.Value() }, watch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tp := NewTaskPane()
			tp.SetTasks([]task.Task{tc.task})
			tp.SetFocus(true)
			tp.EnterEditSelected()
			require.True(t, tp.IsEditing())
			tabN(tp, tc.tabs)
			tc.clear(tp)

			typeRunes(tp, "rxD")

			assert.Equal(t, "rxD", tc.value(tp), "r/x/D must be typed into the %s field", tc.name)
			assert.False(t, tp.HasPendingTrigger(), "r must not run from the %s field", tc.name)
			assert.True(t, tp.IsEditing(), "no glyph should exit edit mode (%s)", tc.name)
			assert.Empty(t, tp.ConsumeDeleted(), "D must not delete from the %s field", tc.name)
			tasks := tp.GetTasks()
			if assert.Len(t, tasks, 1) {
				assert.True(t, tasks[0].Enabled, "x must not toggle from the %s field", tc.name)
			}
			assert.False(t, tp.IsDirty(), "typing must not mark the task dirty (no toggle, no save)")
		})
	}
}

// TestTaskPaneEditModeCronTriggerValueDoesNotFireVerbs: the cron trigger value
// stop is a text stop (its raw-cron cell accepts text), so r/x/D must not fire
// the list verbs there even though the default schedule cells ignore letters.
// The verbs are simply swallowed by the picker, never acted on.
func TestTaskPaneEditModeCronTriggerValueDoesNotFireVerbs(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{cronEditTask(t)})
	tp.SetFocus(true)
	tp.EnterEditSelected()
	require.True(t, tp.IsEditing())
	tabN(tp, 2) // Name -> Trigger -> TriggerValue (cron schedule picker)
	require.Equal(t, taskFocusTriggerValue, tp.focusIndex)

	typeRunes(tp, "rxD")

	assert.False(t, tp.HasPendingTrigger(), "r must not run from the cron trigger value")
	assert.True(t, tp.IsEditing(), "D must not exit edit mode from the cron trigger value")
	assert.Empty(t, tp.ConsumeDeleted(), "D must not delete from the cron trigger value")
	if assert.Len(t, tp.GetTasks(), 1) {
		assert.True(t, tp.GetTasks()[0].Enabled, "x must not toggle from the cron trigger value")
	}
}

// TestTaskPaneEditModeVerbsReachableAtSelectorStops: the #1288 reachability
// guard, re-aimed at the stops that still route verbs after the fix. At every
// selector/button stop r runs, x toggles, and D is a no-op in-pane (the app
// layer confirms it — see app/task_confirmation_test.go).
func TestTaskPaneEditModeVerbsReachableAtSelectorStops(t *testing.T) {
	// (Name→Trigger=1, OnComplete=5, Program=7, Save=8)
	selectorStops := []struct {
		name string
		tabs int
	}{
		{"Trigger", 1},
		{"OnComplete", 5},
		{"Program", 7},
		{"Save", 8},
	}
	for _, sc := range selectorStops {
		t.Run(sc.name, func(t *testing.T) {
			tp := NewTaskPane()
			tp.SetTasks([]task.Task{cronEditTask(t)})
			tp.SetFocus(true)
			tp.EnterEditSelected()
			require.True(t, tp.IsEditing())
			tabN(tp, sc.tabs)

			// r runs the selected task and leaves the form open.
			typeRunes(tp, "r")
			require.True(t, tp.HasPendingTrigger(), "r at %s must run the task", sc.name)
			pending := tp.ConsumePendingTrigger()
			require.NotNil(t, pending)
			assert.Equal(t, "abc", pending.ID)
			assert.True(t, tp.IsEditing(), "run-now leaves the edit form in place")

			// x toggles the selected task and marks it dirty.
			typeRunes(tp, "x")
			require.True(t, tp.IsEditing(), "toggle must not exit edit mode")
			require.True(t, tp.IsDirty(), "toggle from %s marks the task dirty", sc.name)
			assert.False(t, tp.GetTasks()[0].Enabled)
			assert.Len(t, tp.ConsumeDirty(), 1)

			// D does not delete in-pane: the app layer confirms it. The pane
			// must stay in edit mode with the task intact.
			typeRunes(tp, "D")
			assert.True(t, tp.IsEditing(), "D at %s is app-confirmed, not an in-pane delete", sc.name)
			assert.Len(t, tp.GetTasks(), 1, "D must not remove the task from the pane")
			assert.Empty(t, tp.ConsumeDeleted(), "D must not queue a deletion at the pane level")
		})
	}
}

// TestTaskPaneEditModeKeepsListActionsReachable is the moved and updated
// #1288 guard. The one-step edit flow still keeps run/toggle reachable, but
// only after the user steps off the text field that opens focused: at the
// Name field r/x/D TYPE (not act), and at a selector stop r runs and x
// toggles. D no longer deletes in-pane — the app layer confirms it, matching
// list mode.
func TestTaskPaneEditModeKeepsListActionsReachable(t *testing.T) {
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{cronEditTask(t)})
	tp.SetFocus(true)
	tp.EnterEditSelected()
	require.True(t, tp.IsEditing())

	// Focus opens on the Name text field: r/x/D must insert, not act.
	typeRunes(tp, "rxD")
	require.False(t, tp.HasPendingTrigger(), "r at the Name field must type, not run")
	require.False(t, tp.IsDirty(), "x at the Name field must type, not toggle")
	require.True(t, tp.IsEditing(), "D at the Name field must type, not delete")
	require.Empty(t, tp.ConsumeDeleted())
	require.Equal(t, "nightlyrxD", tp.editName.Value())

	// Step to the trigger type selector: the verbs are one Tab away.
	require.True(t, tp.textFocusStop(), "sanity: Name is a text stop")
	tabN(tp, 1)
	require.False(t, tp.textFocusStop(), "Trigger is a selector stop")
	tp.editName.SetValue("nightly") // keep the buffer tidy for the run assertion

	typeRunes(tp, "r")
	require.True(t, tp.HasPendingTrigger(), "r must remain reachable from a selector stop")
	pending := tp.ConsumePendingTrigger()
	require.NotNil(t, pending)
	assert.Equal(t, "abc", pending.ID)
	assert.True(t, tp.IsEditing(), "run-now leaves the edit form in place until the app consumes it")

	typeRunes(tp, "x")
	require.True(t, tp.IsEditing(), "toggle should not kick the user out of edit")
	require.True(t, tp.IsDirty(), "toggle from a selector stop marks the task dirty")
	assert.False(t, tp.GetTasks()[0].Enabled)
	assert.Len(t, tp.ConsumeDirty(), 1)

	// D is confirmed by the app layer, not deleted here.
	typeRunes(tp, "D")
	assert.True(t, tp.IsEditing(), "D at a selector stop is confirmed by the app, not deleted in-pane")
	assert.Len(t, tp.GetTasks(), 1)
	assert.Empty(t, tp.ConsumeDeleted())
}

// TestTaskPaneCreateModeTypesVerbGlyphs: create mode never routes the verbs
// (there is no task to act on), so r/x/D type into the form like any other
// character — the asymmetry with edit mode is gone.
func TestTaskPaneCreateModeTypesVerbGlyphs(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode("/tmp/repo")
	require.True(t, tp.IsCreating())

	typeRunes(tp, "rXD")

	assert.Equal(t, "rXD", tp.editName.Value(), "verb glyphs must type in create mode")
	assert.False(t, tp.HasPendingTrigger(), "create mode must not run on r")
	assert.True(t, tp.IsCreating(), "create mode must survive r/x/D")
	assert.Empty(t, tp.ConsumeDeleted())
}

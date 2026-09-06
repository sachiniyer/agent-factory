package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/task"
)

// The task form's spawned-session lifecycle field (#2595, surface parity).
//
// `af tasks add --on-complete` shipped the verb; the TUI form neither showed nor
// set it, so a task created from the primary surface always kept every session it
// spawned, and a task that DELETES its session after each run looked, in the only
// editor most users open, exactly like one that keeps it. These pin both halves:
// the verb round-trips, and it never reaches a shape the daemon would refuse.

// editTaskWithOnComplete opens the editor on one task carrying verb.
func editTaskWithOnComplete(t *testing.T, verb, target string) *TaskPane {
	t.Helper()
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{{
		ID:            "abc",
		Name:          "nightly",
		Prompt:        "do it",
		CronExpr:      "* * * * *",
		ProjectPath:   newGitRepo(t),
		TargetSession: target,
		OnComplete:    verb,
		Enabled:       true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	assert.True(t, tp.IsEditing())
	return tp
}

// TestTaskPaneOnCompleteOffersTheServedVerbs: the picker's options ARE
// task.OnCompleteValues(), not a copy of them.
//
// The copy is the failure mode this asserts against (#1970): a surface that
// hardcodes an enum the owning package serves passes every other check while a
// verb added there silently never reaches the picker. Compared as a slice, so the
// ORDER is pinned too — OnCompleteValues documents least-destructive-first
// precisely so a picker's landing option is the safe one.
func TestTaskPaneOnCompleteOffersTheServedVerbs(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode(newGitRepo(t))

	assert.Equal(t, task.OnCompleteValues(), tp.editOnCompleteOptions,
		"the picker must offer the verbs task.OnCompleteValues serves, in that order")
	assert.Equal(t, 0, tp.editOnCompleteIdx,
		"a new task must land on the least destructive verb")
	assert.Equal(t, task.OnCompleteKeep, task.OnCompleteValues()[0],
		"…and that verb is keep — if this ever changes, the landing option changed with it")
}

// TestTaskPaneOnCompleteSeedsFromTheStoredVerb: opening the editor shows what
// the record actually holds, including the legacy empty spelling of keep.
func TestTaskPaneOnCompleteSeedsFromTheStoredVerb(t *testing.T) {
	for _, tc := range []struct{ stored, want string }{
		{"", task.OnCompleteKeep}, // written before the field existed
		{task.OnCompleteKeep, task.OnCompleteKeep},
		{task.OnCompleteArchive, task.OnCompleteArchive},
		{task.OnCompleteKill, task.OnCompleteKill},
		{"ARCHIVE", task.OnCompleteArchive}, // CanonicalOnComplete lowercases
	} {
		t.Run("stored="+tc.stored, func(t *testing.T) {
			tp := editTaskWithOnComplete(t, tc.stored, "")
			assert.Equal(t, tc.want, tp.editOnCompleteOptions[tp.editOnCompleteIdx],
				"the editor must open on the verb the record holds")
			tp.SetSize(80, 40)
			assert.Contains(t, tp.String(), "On done:",
				"the row must be on screen — an invisible lifecycle is the gap this closes")
		})
	}
}

// TestTaskPaneOnCompleteEditSavesTheChosenVerb: stepping the picker and saving
// writes the verb onto the task, and the patch the pane emits carries it.
//
// The patch half is the one that matters: the pane's in-memory task is not what
// reaches the daemon — task.DiffTask over the loaded baseline is (#1700) — so a
// field set on the struct but absent from the diff would save nothing.
func TestTaskPaneOnCompleteEditSavesTheChosenVerb(t *testing.T) {
	tp := editTaskWithOnComplete(t, "", "")

	tabTo(tp, taskFocusOnComplete)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRight}) // keep -> archive
	tabTo(tp, taskFocusSave-taskFocusOnComplete)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.IsEditing(), "save should exit edit mode")
	if tasks := tp.GetTasks(); assert.Len(t, tasks, 1) {
		assert.Equal(t, task.OnCompleteArchive, tasks[0].OnComplete)
	}
	edits := tp.ConsumeDirty()
	if assert.Len(t, edits, 1, "the edit must produce a patch") {
		if assert.NotNil(t, edits[0].Update.OnComplete, "the patch must carry on_complete") {
			assert.Equal(t, task.OnCompleteArchive, *edits[0].Update.OnComplete)
		}
	}
}

// TestTaskPaneOnCompleteKeepStoresTheEmptySpelling: choosing keep writes "",
// the form task.canonicalizeOnComplete stores, so an untouched task's record is
// byte-identical to what it was before the field was reachable.
func TestTaskPaneOnCompleteKeepStoresTheEmptySpelling(t *testing.T) {
	tp := editTaskWithOnComplete(t, task.OnCompleteArchive, "")

	tabTo(tp, taskFocusOnComplete)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyLeft}) // archive -> keep
	tabTo(tp, taskFocusSave-taskFocusOnComplete)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	if tasks := tp.GetTasks(); assert.Len(t, tasks, 1) {
		assert.Equal(t, "", tasks[0].OnComplete,
			"keep is stored as the empty string, not the literal word")
		assert.Equal(t, task.OnCompleteKeep, tasks[0].SessionLifecycle(),
			"…and still reads back as keep")
	}
}

// TestTaskPaneOnCompleteCreateCarriesTheVerb: the create form's choice reaches
// the draft the app layer turns into a task.Task.
func TestTaskPaneOnCompleteCreateCarriesTheVerb(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode(newGitRepo(t))
	fillCreateForm(t, tp, "nightly")

	tabTo(tp, taskFocusOnComplete)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown}) // keep -> archive
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyDown}) // archive -> kill
	tabTo(tp, taskFocusSave-taskFocusOnComplete)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.HasPendingCreate(), "submit should mark a pending create")
	assert.Equal(t, task.OnCompleteKill, tp.ConsumePendingCreate().OnComplete)
}

// TestTaskPaneOnCompleteCreateDefaultsToKeep: a create that never touches the
// picker stores the empty spelling, so adding this field changed no existing
// workflow's outcome.
func TestTaskPaneOnCompleteCreateDefaultsToKeep(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode(newGitRepo(t))
	fillCreateForm(t, tp, "nightly")

	tabTo(tp, taskFocusCount-1)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.HasPendingCreate())
	assert.Equal(t, "", tp.ConsumePendingCreate().OnComplete,
		"an untouched picker must produce the pre-#2595 record")
}

// TestTaskPaneOnCompleteIsInapplicableWithATargetSession: with a target session
// the row explains itself, refuses input, and saves nothing.
//
// This is the pair task.ValidateTrigger REFUSES — on_complete governs a session
// the task created, and a target-session task delivers into one the user named
// for reuse. A form that let a user assemble it would turn a save into an error
// message about a combination the form itself offered.
func TestTaskPaneOnCompleteIsInapplicableWithATargetSession(t *testing.T) {
	tp := editTaskWithOnComplete(t, "", "long-lived")
	tp.SetSize(80, 40)

	out := tp.String()
	assert.Contains(t, out, "On done:", "the row stays on screen so the refusal is visible")
	assert.Contains(t, out, "not this task's to reap",
		"…and says WHY, rather than rendering a dead picker")
	for _, verb := range []string{task.OnCompleteArchive, task.OnCompleteKill} {
		assert.NotContains(t, out, verb,
			"a verb the daemon would reject must not be offered as a choice")
	}

	tabTo(tp, taskFocusOnComplete)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	assert.Equal(t, 0, tp.editOnCompleteIdx, "the picker must refuse to move")

	tabTo(tp, taskFocusSave-taskFocusOnComplete)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if tasks := tp.GetTasks(); assert.Len(t, tasks, 1) {
		assert.Equal(t, "", tasks[0].OnComplete)
		assert.NoError(t, tasks[0].ValidateTrigger(),
			"the saved record must be one the daemon accepts")
	}
}

// TestTaskPaneOnCompleteDroppedWhenATargetIsTyped: typing a target session into
// a task that HAD a verb clears it on save, matching what the daemon's merge
// would do anyway (task.clearInapplicableOnComplete) rather than emitting a
// patch ValidateTrigger rejects.
func TestTaskPaneOnCompleteDroppedWhenATargetIsTyped(t *testing.T) {
	tp := editTaskWithOnComplete(t, task.OnCompleteArchive, "")

	tabTo(tp, taskFocusTarget)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("long-lived")})
	tabTo(tp, taskFocusSave-taskFocusTarget)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	tasks := tp.GetTasks()
	if assert.Len(t, tasks, 1) {
		assert.Equal(t, "long-lived", tasks[0].TargetSession)
		assert.Equal(t, "", tasks[0].OnComplete,
			"the verb must be dropped, not carried into a record the daemon refuses")
		assert.NoError(t, tasks[0].ValidateTrigger())
	}
	for _, e := range tp.ConsumeDirty() {
		if e.Update.OnComplete != nil {
			assert.Equal(t, "", *e.Update.OnComplete,
				"an explicitly patched verb must be the cleared one")
		}
	}
}

// TestTaskPaneOnCompleteHintNamesTheConsequence: the focused picker says what
// the selected verb DOES, and only kill's wording promises deletion.
//
// Wording, and load-bearing: kill permanently deletes the run's session and
// prunes its branch, and it sits one arrow-press from archive.
func TestTaskPaneOnCompleteHintNamesTheConsequence(t *testing.T) {
	tp := editTaskWithOnComplete(t, task.OnCompleteKill, "")
	tp.SetSize(100, 40)
	tabTo(tp, taskFocusOnComplete)

	out := tp.String()
	assert.Contains(t, out, "permanent", "kill's hint must name the destruction")

	tp = editTaskWithOnComplete(t, task.OnCompleteArchive, "")
	tp.SetSize(100, 40)
	tabTo(tp, taskFocusOnComplete)
	assert.NotContains(t, tp.String(), "permanent",
		"archive is restorable — its hint must not borrow kill's warning")
	assert.True(t, strings.Contains(tp.String(), "restorable"),
		"archive's hint must say it can be undone")
}

// TestTaskPaneOnCompleteRowSurvivesAnEightyCellPane: nothing this row says is
// lost to the form's per-line clip at the classic width.
//
// The row is the widest in the form — a value, a marker pair, and a consequence
// — and renderEditMode clips every line to the pane (fitBlockToSize). A warning
// that ends in "…" is a warning nobody read, which is the shipped-but-unreachable
// shape this parity work exists to catch: the ledger would score the capability
// "yes" either way.
//
// Asserted on the row's own line rather than on the string, because the clip is
// per line and the assertion has to be too.
func TestTaskPaneOnCompleteRowSurvivesAnEightyCellPane(t *testing.T) {
	for _, verb := range append(task.OnCompleteValues(), "") {
		t.Run("verb="+verb, func(t *testing.T) {
			tp := editTaskWithOnComplete(t, verb, "")
			tp.SetSize(80, 40)
			tabTo(tp, taskFocusOnComplete) // focused: the hint is only rendered then
			assert.False(t, strings.HasSuffix(onDoneRow(t, tp), "…"),
				"the focused On-done row is clipped at 80 cells — shorten the hint, "+
					"do not let the consequence be the half that is cut")
		})
	}

	// The inapplicable row says WHY, and that sentence has to survive the same cut.
	tp := editTaskWithOnComplete(t, "", "long-lived")
	tp.SetSize(80, 40)
	assert.False(t, strings.HasSuffix(onDoneRow(t, tp), "…"),
		"the target-session refusal is clipped at 80 cells")
}

// An 80-column terminal gives the task modal only 48 content cells. Keep
// the complete consequence in view even when the form must scroll vertically.
func TestTaskPaneOnCompleteExplanationFitsNarrowModal(t *testing.T) {
	for _, tc := range []struct {
		verb, target, explanation string
	}{
		{task.OnCompleteKeep, "", "leaves the run's session in place"},
		{task.OnCompleteArchive, "", "archives the run's session — restorable"},
		{task.OnCompleteKill, "", "deletes the run's session and its branch — permanent"},
		{"", "pending", "n/a — a target session is not this task's to reap"},
	} {
		t.Run(tc.verb+tc.target, func(t *testing.T) {
			tp := editTaskWithOnComplete(t, tc.verb, tc.target)
			tp.SetSize(48, 12)
			tabTo(tp, taskFocusOnComplete)
			out := xansi.Strip(tp.String())
			assert.Contains(t, out, "On done:")
			assert.Contains(t, strings.Join(strings.Fields(out), " "), tc.explanation,
				"the full consequence must survive wrapping and focus scrolling")
			for _, line := range strings.Split(out, "\n") {
				assert.LessOrEqual(t, xansi.StringWidth(line), 48)
			}
			assert.LessOrEqual(t, len(strings.Split(out, "\n")), 12)
			if tc.target == "" {
				assert.Contains(t, out, "◂ "+tc.verb+" ▸")
			} else {
				assert.NotContains(t, out, "◂")
			}
		})
	}
}

// onDoneRow returns the rendered form's On-done line.
func onDoneRow(t *testing.T, tp *TaskPane) string {
	t.Helper()
	for _, line := range strings.Split(tp.String(), "\n") {
		if strings.HasPrefix(line, "On done:") {
			return line
		}
	}
	t.Fatal("the form rendered no On-done row")
	return ""
}

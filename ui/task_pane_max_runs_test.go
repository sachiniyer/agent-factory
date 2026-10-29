package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/task"
)

// The task form's concurrency-cap field (#4180, surface parity).
//
// `af tasks add/update --max-concurrent-runs` shipped the cap; neither UI could
// set it, so the only way to bound a watch task built in the TUI or the web was
// discovering a CLI flag after the burst already taught you. These pin both
// halves: the cap round-trips through create AND edit, and it never reaches a
// shape the daemon would refuse — the gate is task.CapApplies itself, the same
// predicate ValidateTrigger enforces.

// editTaskWithCap opens the editor on one task carrying the given cap, trigger,
// and target session.
func editTaskWithCap(t *testing.T, cap int, watch bool, target string) *TaskPane {
	t.Helper()
	tsk := task.Task{
		ID:                "abc",
		Name:              "nightly",
		Prompt:            "do it",
		ProjectPath:       newGitRepo(t),
		TargetSession:     target,
		MaxConcurrentRuns: cap,
		Enabled:           true,
	}
	if watch {
		tsk.WatchCmd = "tail -f events.log"
	} else {
		tsk.CronExpr = "* * * * *"
	}
	tp := NewTaskPane()
	tp.SetTasks([]task.Task{tsk})
	tp.SetFocus(true)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter}) // enter edit mode
	assert.True(t, tp.IsEditing())
	return tp
}

// fillWatchCreateForm fills the create form as a WATCH task: name, trigger
// selector flipped to watch, and a watch command. Leaves focus back on Name so
// callers can navigate forward consistently, like fillCreateForm.
func fillWatchCreateForm(t *testing.T, tp *TaskPane, name string) {
	t.Helper()
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)})
	tabTo(tp, taskFocusTrigger)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRight}) // cron -> watch
	tabTo(tp, taskFocusTriggerValue-taskFocusTrigger)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tail -f events.log")})
	for i := 0; i < taskFocusTriggerValue; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab})
	}
}

// TestTaskPaneMaxRunsSeedsFromTheStoredCap: opening the editor shows what the
// record holds — a positive cap as its digits, unlimited as the empty field.
func TestTaskPaneMaxRunsSeedsFromTheStoredCap(t *testing.T) {
	for _, tc := range []struct {
		stored int
		want   string
	}{
		{0, ""},  // unlimited is the empty field, not the literal "0"
		{3, "3"}, // #1892's incident cap
	} {
		tp := editTaskWithCap(t, tc.stored, true, "")
		assert.Equal(t, tc.want, tp.editMaxRuns.Value(),
			"the editor must open on the cap the record holds (stored=%d)", tc.stored)
		tp.SetSize(80, 40)
		assert.Contains(t, tp.String(), "Max runs:",
			"the row must be on screen — an invisible cap is the gap this closes")
	}
}

// TestTaskPaneMaxRunsEditSavesTheCap: typing a bound and saving writes it onto
// the task, and the patch the pane emits carries it — the half that matters,
// because task.DiffTask over the loaded baseline is what reaches the daemon
// (#1700), not the in-memory struct.
func TestTaskPaneMaxRunsEditSavesTheCap(t *testing.T) {
	tp := editTaskWithCap(t, 0, true, "")

	tabTo(tp, taskFocusMaxRuns)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	tabTo(tp, taskFocusSave-taskFocusMaxRuns)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.False(t, tp.IsEditing(), "save should exit edit mode")
	if tasks := tp.GetTasks(); assert.Len(t, tasks, 1) {
		assert.Equal(t, 3, tasks[0].MaxConcurrentRuns)
	}
	edits := tp.ConsumeDirty()
	if assert.Len(t, edits, 1, "the edit must produce a patch") {
		if assert.NotNil(t, edits[0].Update.MaxConcurrentRuns, "the patch must carry max_concurrent_runs") {
			assert.Equal(t, 3, *edits[0].Update.MaxConcurrentRuns)
		}
	}
}

// TestTaskPaneMaxRunsEditEmptyRevertsToUnlimited: clearing the field stores 0,
// and the diff carries the EXPLICIT zero — the pointer keeps "set unlimited"
// distinguishable from "unchanged" (#1700's patch semantics).
func TestTaskPaneMaxRunsEditEmptyRevertsToUnlimited(t *testing.T) {
	tp := editTaskWithCap(t, 3, true, "")

	tabTo(tp, taskFocusMaxRuns)
	for range "3" {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	tabTo(tp, taskFocusSave-taskFocusMaxRuns)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	if tasks := tp.GetTasks(); assert.Len(t, tasks, 1) {
		assert.Equal(t, 0, tasks[0].MaxConcurrentRuns)
	}
	for _, e := range tp.ConsumeDirty() {
		if e.Update.MaxConcurrentRuns != nil {
			assert.Equal(t, 0, *e.Update.MaxConcurrentRuns,
				"reverting to unlimited must arrive as an explicit 0, not an omitted field")
		}
	}
}

// TestTaskPaneMaxRunsCreateCarriesTheCap: the create form's cap reaches the
// draft the app layer turns into a task.Task.
func TestTaskPaneMaxRunsCreateCarriesTheCap(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode(newGitRepo(t))
	fillWatchCreateForm(t, tp, "watcher")

	tabTo(tp, taskFocusMaxRuns)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("5")})
	tabTo(tp, taskFocusSave-taskFocusMaxRuns)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.HasPendingCreate(), "submit should mark a pending create")
	assert.Equal(t, 5, tp.ConsumePendingCreate().MaxConcurrentRuns)
}

// TestTaskPaneMaxRunsCreateDefaultsToUnlimited: a create that never touches the
// field stores 0, so adding it changed no existing workflow's outcome.
func TestTaskPaneMaxRunsCreateDefaultsToUnlimited(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode(newGitRepo(t))
	fillWatchCreateForm(t, tp, "watcher")

	tabTo(tp, taskFocusCount-1)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.HasPendingCreate())
	assert.Equal(t, 0, tp.ConsumePendingCreate().MaxConcurrentRuns,
		"an untouched cap field must produce the pre-#4180 record")
}

// TestTaskPaneMaxRunsInapplicableOnCron: a cron task shows the reason instead
// of the input, refuses digits, and saves no cap.
//
// This is the pair task.ValidateTrigger REFUSES — the cap bounds sessions a
// watch task spawns per event, and overlapping cron fires already coalesce. A
// form that let a user assemble it would turn a save into an error about a
// combination the form itself offered.
func TestTaskPaneMaxRunsInapplicableOnCron(t *testing.T) {
	tp := editTaskWithCap(t, 0, false, "")
	tp.SetSize(80, 40)

	out := tp.String()
	assert.Contains(t, out, "Max runs:", "the row stays on screen so the refusal is visible")
	assert.Contains(t, out, "Cron fires already coalesce.",
		"…and says WHY — the daemon's own words — rather than rendering a dead input")

	tabTo(tp, taskFocusMaxRuns)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")})
	assert.Equal(t, "", tp.editMaxRuns.Value(), "the input must refuse to take digits")

	tabTo(tp, taskFocusSave-taskFocusMaxRuns)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if tasks := tp.GetTasks(); assert.Len(t, tasks, 1) {
		assert.Equal(t, 0, tasks[0].MaxConcurrentRuns)
		assert.NoError(t, tasks[0].ValidateTrigger(),
			"the saved record must be one the daemon accepts")
	}
}

// TestTaskPaneMaxRunsInapplicableWithATarget: a watch task with a target
// session shows the other reason, refuses input, and clears a stale cap on
// save — agreeing with what task.clearInapplicableCap does on the daemon's side.
func TestTaskPaneMaxRunsInapplicableWithATarget(t *testing.T) {
	tp := editTaskWithCap(t, 3, true, "long-lived")
	tp.SetSize(80, 40)

	out := tp.String()
	assert.Contains(t, out, "Max runs:")
	assert.Contains(t, out, "Deliveries into one session already serialize.")
	assert.NotContains(t, out, "unlimited",
		"an inapplicable cap must not render as a settable value")

	tabTo(tp, taskFocusMaxRuns)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")})
	assert.Equal(t, "3", tp.editMaxRuns.Value(), "the input must refuse to move")

	tabTo(tp, taskFocusSave-taskFocusMaxRuns)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if tasks := tp.GetTasks(); assert.Len(t, tasks, 1) {
		assert.Equal(t, "long-lived", tasks[0].TargetSession)
		assert.Equal(t, 0, tasks[0].MaxConcurrentRuns,
			"the stale cap must be dropped, not carried into a record the daemon refuses")
		assert.NoError(t, tasks[0].ValidateTrigger())
	}
	for _, e := range tp.ConsumeDirty() {
		if e.Update.MaxConcurrentRuns != nil {
			assert.Equal(t, 0, *e.Update.MaxConcurrentRuns,
				"an explicitly patched cap must be the cleared one")
		}
	}
}

// TestTaskPaneMaxRunsDroppedWhenTriggerFlipsToCron: a cap typed while the
// selector said watch must not survive the selector moving to cron — the same
// agreement-with-the-merge as the target-session case.
func TestTaskPaneMaxRunsDroppedWhenTriggerFlipsToCron(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode(newGitRepo(t))
	fillWatchCreateForm(t, tp, "watcher")

	tabTo(tp, taskFocusMaxRuns)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("4")})

	// Walk back to the trigger selector and flip to cron — which needs a prompt
	// a watch task may omit, so fill one on the way to Save.
	for i := 0; i < taskFocusMaxRuns-taskFocusTrigger; i++ {
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab})
	}
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyLeft}) // watch -> cron
	tabTo(tp, taskFocusPrompt-taskFocusTrigger)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do something")})
	tabTo(tp, taskFocusSave-taskFocusPrompt)
	tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	assert.True(t, tp.HasPendingCreate())
	draft := tp.ConsumePendingCreate()
	assert.NotEqual(t, "", draft.Cron, "sanity: the flip produced a cron task")
	assert.Equal(t, "", draft.WatchCmd)
	assert.Equal(t, 0, draft.MaxConcurrentRuns,
		"the typed cap must be dropped when the trigger stops being watch")
}

// TestTaskPaneMaxRunsRefusesNonIntegers: the form refuses what ParseCapInput
// refuses — negatives, decimals, text, and the value above the shared
// safe-integer ceiling — and lands focus on the offending field. The parse
// table itself lives in the shared vectors; this pins that the form asks it.
func TestTaskPaneMaxRunsRefusesNonIntegers(t *testing.T) {
	for _, tc := range []struct{ raw string }{
		{"-1"}, {"abc"}, {"1.5"}, {"9007199254740992"},
	} {
		t.Run("raw="+tc.raw, func(t *testing.T) {
			tp := editTaskWithCap(t, 0, true, "")
			tabTo(tp, taskFocusMaxRuns)
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.raw)})
			tabTo(tp, taskFocusSave-taskFocusMaxRuns)
			tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

			assert.True(t, tp.IsEditing(), "a refused cap must keep the form open")
			assert.Equal(t, "max runs must be a non-negative integer", tp.editError)
			assert.Equal(t, taskFocusMaxRuns, tp.editErrorField,
				"the error must render under the Max runs field")
			if tasks := tp.GetTasks(); assert.Len(t, tasks, 1) {
				assert.Equal(t, 0, tasks[0].MaxConcurrentRuns, "nothing was saved")
			}
		})
	}
}

// TestTaskPaneMaxRunsRowSurvivesAnEightyCellPane: the inapplicable reason is the
// widest thing this row says, and the form clips every line to the pane —
// a reason that ends in "…" is a reason nobody read.
func TestTaskPaneMaxRunsRowSurvivesAnEightyCellPane(t *testing.T) {
	for _, tc := range []struct {
		watch  bool
		target string
	}{
		{false, ""},          // cron reason
		{true, "long-lived"}, // target reason
	} {
		tp := editTaskWithCap(t, 0, tc.watch, tc.target)
		tp.SetSize(80, 40)
		for _, line := range strings.Split(tp.String(), "\n") {
			if strings.HasPrefix(line, "Max runs:") {
				assert.False(t, strings.HasSuffix(xansi.Strip(line), "…"),
					"the Max-runs refusal is clipped at 80 cells — shorten it")
			}
		}
	}
}

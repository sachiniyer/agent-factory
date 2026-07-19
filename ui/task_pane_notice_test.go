package ui

import (
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
)

// watchTaskPane builds a focused list-mode pane holding a single watch task.
func watchTaskPane(t *testing.T, width, height int) *TaskPane {
	t.Helper()
	tp := NewTaskPane()
	tp.SetSize(width, height)
	tp.SetFocus(true)
	tp.SetTasks([]task.Task{{
		ID:       "watchy",
		Name:     "watchy",
		Prompt:   "do it",
		WatchCmd: "tail -f log",
		Program:  "claude",
		Enabled:  true,
	}})
	return tp
}

func pressRune(tp *TaskPane, r string) bool {
	return tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(r)})
}

// TestTaskPaneWatchRunNowNoticeVisibleInListMode pins #2137: pressing r on a
// watch task in LIST mode refused the run but rendered nothing, so the key
// looked dead. The refusal must be visible on the surface the user is on.
func TestTaskPaneWatchRunNowNoticeVisibleInListMode(t *testing.T) {
	tp := watchTaskPane(t, 80, 20)

	assert.True(t, pressRune(tp, "r"))
	assert.False(t, tp.HasPendingTrigger(), "a watch task must not queue a doomed run")

	out := tp.String()
	assert.Contains(t, out, "not on manual trigger",
		"r on a watch task in list mode must explain why manual run is unavailable")
}

// TestTaskPaneWatchRunNowNoticeUnclippedAt80Cols keeps the consequential half
// of the notice on screen at the reference width: a transient message whose
// tail is ellipsized away reads as no explanation at all (#1973).
func TestTaskPaneWatchRunNowNoticeUnclippedAt80Cols(t *testing.T) {
	tp := watchTaskPane(t, 80, 20)
	pressRune(tp, "r")

	out := tp.String()
	assert.Contains(t, out, "watch tasks run on their watch command's output")
	assert.Contains(t, out, "not on manual trigger")
	assert.NotContains(t, out, "…", "the notice must fit at 80 cols, not be ellipsized")
	for _, line := range strings.Split(out, "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), 80, "no rendered line may exceed the pane width")
	}
}

// TestTaskPaneWatchRunNowNoticeWrapsWhenNarrow: below the notice's width the
// message wraps instead of losing its tail, so the refusal still reads.
func TestTaskPaneWatchRunNowNoticeWrapsWhenNarrow(t *testing.T) {
	tp := watchTaskPane(t, 46, 20)
	pressRune(tp, "r")

	out := tp.String()
	assert.Contains(t, out, "not on manual trigger",
		"a narrow pane must wrap the notice, not truncate its tail")
	for _, line := range strings.Split(out, "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), 46, "no rendered line may exceed the pane width")
	}
}

// TestTaskPaneWatchRunNowNoticeIsTransient: the notice is feedback for one
// keypress, so the next action clears it rather than leaving a stale error
// hanging under an unrelated selection.
func TestTaskPaneWatchRunNowNoticeIsTransient(t *testing.T) {
	tp := watchTaskPane(t, 80, 20)
	pressRune(tp, "r")
	assert.Contains(t, tp.String(), "not on manual trigger")

	pressRune(tp, "j")
	assert.NotContains(t, tp.String(), "not on manual trigger",
		"the notice must clear on the next keypress")

	// Mouse-wheel scrolling moves the selection without a keypress, so it
	// retires the notice too.
	pressRune(tp, "r")
	assert.Contains(t, tp.String(), "not on manual trigger")
	tp.ScrollDown()
	assert.NotContains(t, tp.String(), "not on manual trigger",
		"the notice must clear when the selection scrolls")
}

// TestTaskPaneWatchRunNowNoticeSurvivesAClampedList: a task list taller than
// the pane windows its rows, and the notice must be pinned with the hint
// instead of scrolling off with them.
func TestTaskPaneWatchRunNowNoticeSurvivesAClampedList(t *testing.T) {
	tp := NewTaskPane()
	tp.SetSize(80, 10)
	tp.SetFocus(true)
	tasks := make([]task.Task, 0, 12)
	for i := 0; i < 12; i++ {
		tasks = append(tasks, task.Task{
			ID:       "t" + strconv.Itoa(i),
			Name:     "task-" + strconv.Itoa(i),
			WatchCmd: "tail -f log",
			Enabled:  true,
		})
	}
	tp.SetTasks(tasks)

	pressRune(tp, "r")
	out := tp.String()
	assert.Contains(t, out, "not on manual trigger",
		"the notice must stay on screen when the list is clamped to the pane height")
	assert.Len(t, strings.Split(out, "\n"), 10, "the pane must still fit its height")
}

// TestTaskPaneRunNowStillTriggersNonWatchTask guards the regression side: a
// cron task's r still queues the trigger and shows no refusal.
func TestTaskPaneRunNowStillTriggersNonWatchTask(t *testing.T) {
	tp := NewTaskPane()
	tp.SetSize(80, 20)
	tp.SetFocus(true)
	tp.SetTasks([]task.Task{{
		ID:       "cronny",
		Name:     "cronny",
		Prompt:   "do it",
		CronExpr: "0 9 * * 1-5",
		Program:  "claude",
		Enabled:  true,
	}})

	assert.True(t, pressRune(tp, "r"))
	assert.True(t, tp.HasPendingTrigger(), "a cron task's r must still queue a trigger")
	tsk := tp.ConsumePendingTrigger()
	if assert.NotNil(t, tsk) {
		assert.Equal(t, "cronny", tsk.ID)
	}
	assert.NotContains(t, tp.String(), "not on manual trigger")
}

package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/schedule"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func keyType(kt tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: kt} }
func keyRunes(s string) tea.KeyMsg      { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// TestSchedulePickerDefaultIsDailyNineAM pins the friendly default a brand-new
// task opens on: daily at 9:00 AM, a valid cron with no typing required.
func TestSchedulePickerDefaultIsDailyNineAM(t *testing.T) {
	p := newSchedulePicker()
	assert.Equal(t, "0 9 * * *", p.Cron())
	assert.Equal(t, "Every day at 9:00 AM", p.Describe())
	assert.Equal(t, "", p.validate(), "the default schedule must be savable")
}

// TestSchedulePickerSeedsMatchingPreset verifies an existing task's cron
// re-opens on its matching preset, and an unrecognized expression falls back to
// Custom with the raw cron preserved (#2057).
func TestSchedulePickerSeedsMatchingPreset(t *testing.T) {
	cases := []struct {
		cron     string
		wantKind schedule.Type
		wantDesc string
	}{
		{"*/15 * * * *", schedule.EveryNMinutes, "Every 15 minutes"},
		{"0 */2 * * *", schedule.EveryNHours, "Every 2 hours"},
		{"5 * * * *", schedule.Hourly, "Every hour at :05"},
		{"41 15 * * *", schedule.Daily, "Every day at 3:41 PM"},
		{"0 9 * * 1,3", schedule.Weekly, "Every week on Mon, Wed at 9:00 AM"},
		{"30 14 15 * *", schedule.Monthly, "Every month on the 15th at 2:30 PM"},
		{"0 9 * * 1-5", schedule.Custom, "Custom: 0 9 * * 1-5"},
	}
	for _, c := range cases {
		t.Run(c.cron, func(t *testing.T) {
			p := newSchedulePicker()
			sc, _ := schedule.ParseCron(c.cron)
			p.seed(sc)
			assert.Equal(t, c.wantKind, p.kind())
			assert.Equal(t, c.wantDesc, p.Describe())
			// The generated cron must round-trip the seeded expression exactly,
			// so re-saving an untouched task never rewrites its schedule.
			assert.Equal(t, c.cron, p.Cron(), "seeded cron must round-trip losslessly")
		})
	}
}

// TestSchedulePickerSeedLightsWeekdayToggles checks a weekly seed turns on the
// exact day toggles (Monday-first display order).
func TestSchedulePickerSeedLightsWeekdayToggles(t *testing.T) {
	p := newSchedulePicker()
	sc, ok := schedule.ParseCron("0 9 * * 1,3")
	require.True(t, ok)
	p.seed(sc)
	// Display order is M T W T F S S -> Monday=index 0, Wednesday=index 2.
	assert.True(t, p.weekdays[0], "Monday toggle on")
	assert.True(t, p.weekdays[2], "Wednesday toggle on")
	assert.False(t, p.weekdays[1], "Tuesday toggle off")
	assert.False(t, p.weekdays[6], "Sunday toggle off")
}

// TestSchedulePickerRenderShowsSelectorInputsPreviewCron is the core visible
// contract: the block renders the type selector, the contextual inputs, the
// plain-English preview, and the generated cron read-only.
func TestSchedulePickerRenderShowsSelectorInputsPreviewCron(t *testing.T) {
	p := newSchedulePicker()
	p.setWidth(80)
	p.setFocused(true)
	out := p.render()
	assert.Contains(t, out, "Schedule:", "type-selector label")
	assert.Contains(t, out, "Daily", "the selected type")
	assert.Contains(t, out, "AM", "the contextual time input")
	assert.Contains(t, out, "Every day at 9:00 AM", "the plain-English preview")
	assert.Contains(t, out, "0 9 * * *", "the generated cron shown read-only")
}

// TestSchedulePickerRenderWeeklyShowsDayRow verifies the weekly type reveals the
// day-of-week toggle row and its own preview.
func TestSchedulePickerRenderWeeklyShowsDayRow(t *testing.T) {
	p := newSchedulePicker()
	sc, _ := schedule.ParseCron("0 9 * * 1,3")
	p.seed(sc)
	p.setWidth(80)
	p.setFocused(true)
	out := p.render()
	assert.Contains(t, out, "Weekly")
	assert.Contains(t, out, "Days", "weekly reveals the day-of-week row")
	assert.Contains(t, out, "Every week on Mon, Wed at 9:00 AM")
}

// TestSchedulePickerRenderNarrowDoesNotPanic guards the small-terminal path:
// the block still renders (with the preview leading) at a cramped width.
func TestSchedulePickerRenderNarrowDoesNotPanic(t *testing.T) {
	p := newSchedulePicker()
	sc, _ := schedule.ParseCron("30 8 * * 1,2,3,4,5")
	p.seed(sc)
	for _, w := range []int{10, 20, 40} {
		p.setWidth(w)
		p.setFocused(true)
		assert.NotPanics(t, func() {
			out := p.render()
			assert.Contains(t, out, "Schedule:")
		})
	}
}

// TestSchedulePickerTypeSwitchChangesCron drives the type selector with the
// arrow keys and confirms the generated cron follows the chosen shape.
func TestSchedulePickerTypeSwitchChangesCron(t *testing.T) {
	p := newSchedulePicker() // Daily, cursor on the type cell
	p.setFocused(true)
	p.handleKey(keyType(tea.KeyRight)) // Daily -> Weekly
	assert.Equal(t, schedule.Weekly, p.kind())
	assert.Equal(t, "0 9 * * 1", p.Cron(), "weekly keeps the 9:00 AM time on the default Monday")

	p.handleKey(keyType(tea.KeyRight)) // Weekly -> Monthly
	assert.Equal(t, schedule.Monthly, p.kind())
	assert.Equal(t, "0 9 1 * *", p.Cron())
}

// TestSchedulePickerNumericAndMeridiemEntry drives the daily contextual inputs:
// move to the hour/minute cells, type values, and flip AM->PM.
func TestSchedulePickerNumericAndMeridiemEntry(t *testing.T) {
	p := newSchedulePicker() // Daily
	p.setFocused(true)

	p.handleKey(keyType(tea.KeyDown))      // type -> hour
	p.handleKey(keyType(tea.KeyBackspace)) // clear "9"
	p.handleKey(keyRunes("3"))
	p.handleKey(keyType(tea.KeyDown))      // hour -> minute
	p.handleKey(keyType(tea.KeyBackspace)) // clear "00" -> "0"
	p.handleKey(keyType(tea.KeyBackspace)) // -> ""
	p.handleKey(keyRunes("45"))
	p.handleKey(keyType(tea.KeyDown))  // minute -> meridiem
	p.handleKey(keyType(tea.KeySpace)) // AM -> PM

	assert.Equal(t, "45 15 * * *", p.Cron())
	assert.Equal(t, "Every day at 3:45 PM", p.Describe())
}

// TestSchedulePickerWeekdayCursorAndToggle walks the weekday row and toggles a
// second day on with space.
func TestSchedulePickerWeekdayCursorAndToggle(t *testing.T) {
	p := newSchedulePicker()
	p.setFocused(true)
	p.handleKey(keyType(tea.KeyRight)) // Daily -> Weekly (Monday on by default)

	// Move to the weekday row: type -> hour -> minute -> meridiem -> weekdays.
	for i := 0; i < 4; i++ {
		p.handleKey(keyType(tea.KeyDown))
	}
	assert.Equal(t, cellWeekdays, p.activeCell())
	p.handleKey(keyType(tea.KeyRight)) // Mon -> Tue
	p.handleKey(keyType(tea.KeyRight)) // Tue -> Wed
	p.handleKey(keyType(tea.KeySpace)) // toggle Wed on

	assert.Equal(t, "0 9 * * 1,3", p.Cron())
	assert.Equal(t, "Every week on Mon, Wed at 9:00 AM", p.Describe())
}

// TestSchedulePickerWeeklyRequiresDay confirms a weekly schedule with every day
// toggled off is rejected with a friendly message rather than an invalid cron.
func TestSchedulePickerWeeklyRequiresDay(t *testing.T) {
	p := newSchedulePicker()
	p.setFocused(true)
	p.handleKey(keyType(tea.KeyRight)) // -> Weekly, Monday on

	for i := 0; i < 4; i++ {
		p.handleKey(keyType(tea.KeyDown)) // -> weekday row, cursor on Monday
	}
	p.handleKey(keyType(tea.KeySpace)) // toggle Monday off -> no days

	assert.Equal(t, "select at least one day of the week", p.validate())
}

// TestSchedulePickerCustomAcceptsAndValidatesRaw checks the escape hatch: the
// Custom cell edits like the old raw-cron field and runs the same validator.
func TestSchedulePickerCustomAcceptsAndValidatesRaw(t *testing.T) {
	p := newSchedulePicker()
	p.setType(schedule.Custom)
	p.raw.SetValue("")
	assert.Equal(t, "cron expression is required", p.validate())

	p.raw.SetValue("bogus cron")
	assert.Contains(t, p.validate(), "invalid cron")

	p.raw.SetValue("0 9 * * 1-5")
	assert.Equal(t, "", p.validate())
	assert.Equal(t, "0 9 * * 1-5", p.Cron())
}

// TestSchedulePickerSwitchToCustomPrefillsCron verifies switching into Custom
// seeds the raw field with the immediately-previous preset's cron, so the
// escape hatch starts from a working expression.
func TestSchedulePickerSwitchToCustomPrefillsCron(t *testing.T) {
	p := newSchedulePicker() // Daily
	p.setFocused(true)
	// Daily(3) -> Weekly -> Monthly -> Custom(6): three rights. Monthly is the
	// last preset before Custom, so its cron is what prefills.
	for i := 0; i < 3; i++ {
		p.handleKey(keyType(tea.KeyRight))
	}
	require.Equal(t, schedule.Custom, p.kind())
	assert.Equal(t, "0 9 1 * *", strings.TrimSpace(p.raw.Value()),
		"custom prefills with the immediately-previous preset's cron")
}

// TestTaskPaneCreateSavesGeneratedCron drives a full create with the default
// schedule and confirms the pending create carries the generated cron.
func TestTaskPaneCreateSavesGeneratedCron(t *testing.T) {
	tp := NewTaskPane()
	tp.EnterCreateMode(newGitRepo(t))
	fillCreateForm(t, tp, "nightly")

	tabTo(tp, taskFocusSave)
	tp.HandleKeyPress(keyType(tea.KeyEnter))

	require.True(t, tp.HasPendingCreate(), "a valid schedule must submit")
	_, _, cron, watch, _, _, _ := tp.ConsumePendingCreate()
	assert.Equal(t, "0 9 * * *", cron, "the default daily schedule's cron is saved")
	assert.Equal(t, "", watch)
}

// TestTaskPaneEditSeedsPresetAndPreservesCron opens an existing weekly task,
// asserts the form seeds the matching preview, and confirms re-saving without
// changes round-trips the cron unchanged.
func TestTaskPaneEditSeedsPresetAndPreservesCron(t *testing.T) {
	tp := NewTaskPane()
	tp.SetSize(80, 24)
	tp.SetTasks([]task.Task{{
		ID:          "wk",
		Name:        "weekly-standup",
		Prompt:      "stand up",
		CronExpr:    "0 9 * * 1,3",
		ProjectPath: newGitRepo(t),
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(keyType(tea.KeyEnter)) // enter edit mode
	require.True(t, tp.IsEditing())

	assert.Contains(t, tp.String(), "Every week on Mon, Wed at 9:00 AM",
		"an existing cron seeds the matching preset preview")

	tabTo(tp, taskFocusCount-1) // walk to Save
	tp.HandleKeyPress(keyType(tea.KeyEnter))

	require.False(t, tp.IsEditing(), "save exits edit mode")
	assert.Equal(t, "0 9 * * 1,3", tp.GetTasks()[0].CronExpr,
		"re-saving an untouched schedule must not rewrite its cron")
}

// TestTaskPaneEditChangeScheduleTypeUpdatesCron seeds an every-15-minutes task,
// switches the type to daily through the picker, and confirms the saved cron
// follows the new schedule.
func TestTaskPaneEditChangeScheduleTypeUpdatesCron(t *testing.T) {
	tp := NewTaskPane()
	tp.SetSize(80, 24)
	tp.SetTasks([]task.Task{{
		ID:          "iv",
		Name:        "poller",
		Prompt:      "poll",
		CronExpr:    "*/15 * * * *",
		ProjectPath: newGitRepo(t),
		Enabled:     true,
	}})
	tp.SetFocus(true)
	tp.HandleKeyPress(keyType(tea.KeyEnter)) // edit mode, focus on Name
	require.True(t, tp.IsEditing())

	tabTo(tp, taskFocusTriggerValue) // into the schedule picker (on the type cell)
	// EveryNMinutes(0) -> Daily(3): three rights.
	for i := 0; i < 3; i++ {
		tp.HandleKeyPress(keyType(tea.KeyRight))
	}
	tabTo(tp, taskFocusCount-1-taskFocusTriggerValue) // walk to Save
	tp.HandleKeyPress(keyType(tea.KeyEnter))

	require.False(t, tp.IsEditing())
	assert.Equal(t, "0 9 * * *", tp.GetTasks()[0].CronExpr,
		"switching the schedule type rewrites the stored cron")
}

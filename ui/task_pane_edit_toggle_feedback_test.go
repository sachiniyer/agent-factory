package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/task"
)

// TestTaskPaneEditToggleIsVisibleWhereAdvertised pins #7135. At a selector or
// button stop the edit form's action row advertises "x toggle", and x flips
// the task's enabled flag in the draft. The form used to render no enabled
// state at all, so the frame was identical before and after x: the key read as
// dead, and a second press silently cancelled the first.
//
// The property: wherever the row says "x toggle", the enabled state is on
// screen and x visibly changes it. It must hold at a short overlay too, where
// the form windows to the focused stop and the title row scrolls away (at the
// Program and Save stops the title is gone below about 29 rows), so the state
// cannot live only in the title. Every stop is exercised on a cron task and on
// a watch task, whose row drops "r run".
//
// The pane's clock is pinned. The cron task's "* * * * *" is Custom, so its form
// shows a "Next run" line read from the clock, and a minute boundary between
// frames failed the restore comparison below on master (02:31 against 02:32,
// run 43351208224; #4217).
func TestTaskPaneEditToggleIsVisibleWhereAdvertised(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(nil))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	cron := cronEditTask(t)
	watch := cron
	watch.ID = "w1"
	watch.CronExpr = ""
	watch.WatchCmd = "tail -f log"

	now := time.Date(2026, time.November, 13, 2, 31, 30, 0, time.UTC)

	type size struct{ w, h int }
	// 34 is the overlay's content width at the 40-column hard minimum; 12
	// rows windows the form so the title scrolls away at the lower stops.
	sizes := []size{{80, 40}, {52, 12}, {34, 10}}

	for _, base := range []task.Task{cron, watch} {
		for _, sz := range sizes {
			advertised := 0
			for stop := 0; stop < taskFocusCount; stop++ {
				tp := NewTaskPane()
				tp.setNowForTest(func() time.Time { return now })
				tp.SetSize(sz.w, sz.h)
				tp.SetTasks([]task.Task{base})
				tp.SetFocus(true)
				tp.EnterEditSelected()
				tp.focusIndex = stop
				tp.updateEditFocus()

				before := stripANSI(tp.String())
				lines := strings.Split(before, "\n")
				if !strings.Contains(lines[len(lines)-1], "x toggle") {
					continue // x types here or is not advertised; nothing to show
				}
				advertised++
				require.Falsef(t, tp.textFocusStop(),
					"%s %dx%d stop %d: x toggle is advertised at a text stop", base.ID, sz.w, sz.h, stop)
				assert.Containsf(t, before, "[✓]",
					"%s %dx%d stop %d: the enabled task's state must be on screen where x toggle is advertised:\n%s",
					base.ID, sz.w, sz.h, stop, before)

				typeIntoPane(tp, "x")
				require.False(t, tp.GetTasks()[0].Enabled, "sanity: x toggled the draft")
				after := stripANSI(tp.String())
				assert.NotEqualf(t, before, after,
					"%s %dx%d stop %d: x toggled the task but the form did not change (#7135):\n%s",
					base.ID, sz.w, sz.h, stop, after)
				assert.Containsf(t, after, "[✗]",
					"%s %dx%d stop %d: the disabled state must be on screen after x:\n%s",
					base.ID, sz.w, sz.h, stop, after)
				assert.NotContainsf(t, after, "[✓]",
					"%s %dx%d stop %d: the stale enabled glyph must be gone after x:\n%s",
					base.ID, sz.w, sz.h, stop, after)

				typeIntoPane(tp, "x")
				assert.Equalf(t, before, stripANSI(tp.String()),
					"%s %dx%d stop %d: a second x must restore the original frame", base.ID, sz.w, sz.h, stop)
			}
			// Trigger, On done, Program and Save advertise the verbs on both
			// shapes; a cron task's inert Max runs row does too.
			assert.GreaterOrEqualf(t, advertised, 4,
				"%s %dx%d: too few stops advertised x toggle for this test to mean anything", base.ID, sz.w, sz.h)
		}
	}
}

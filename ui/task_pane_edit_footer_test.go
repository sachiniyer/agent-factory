package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/task"
)

// TestTaskPaneEditActionFooterFollowsFocus pins #5538: the action row may only
// advertise keys that fire at the focused stop. At a text stop r/x/D (and q)
// are typed input — #4838 routes the verb glyphs into the field — so the row
// advertises "esc list" alone, the way the Trigger selector's "←/→ switch"
// only appears while it is focused. At a selector/button stop the verbs are
// live and return to the row. The configured quit key is gated on !IsEditing
// in handleStateTasks (#1961), so it is dead at EVERY edit stop — text or
// selector — and the row never advertises it.
func TestTaskPaneEditActionFooterFollowsFocus(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(nil))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	tp := NewTaskPane()
	tp.SetSize(80, 24)
	tp.SetTasks([]task.Task{{
		ID: "abc", Name: "nightly", Prompt: "do it", CronExpr: "0 0 * * *",
		ProjectPath: newGitRepo(t), Program: "claude", Enabled: true,
	}})
	tp.SetFocus(true)
	tp.EnterEditSelected()
	require.Equal(t, taskFocusName, tp.focusIndex, "edit must open on the Name field")

	footer := func() string {
		lines := strings.Split(stripANSI(tp.String()), "\n")
		return lines[len(lines)-1]
	}
	textStop := map[int]bool{
		taskFocusName: true, taskFocusTriggerValue: true,
		taskFocusPrompt: true, taskFocusTarget: true, taskFocusPath: true,
	}
	for stop := 0; stop < taskFocusCount; stop++ {
		tp.focusIndex = stop
		tp.updateEditFocus()
		row := footer()
		if textStop[stop] {
			assert.NotContainsf(t, row, "r run",
				"stop %d is a text field: 'r run' invites a keystroke that types instead", stop)
			assert.NotContainsf(t, row, "x toggle",
				"stop %d is a text field: 'x toggle' invites a keystroke that types instead", stop)
			assert.NotContainsf(t, row, "D del",
				"stop %d is a text field: 'D delete/del' invites a keystroke that types instead", stop)
			assert.Containsf(t, row, "esc list",
				"stop %d: esc still exits to the list and stays advertised", stop)
		} else {
			assert.Containsf(t, row, "r run",
				"stop %d routes the verbs — they must be advertised", stop)
			assert.Containsf(t, row, "x toggle", "stop %d routes the verbs", stop)
			assert.Containsf(t, row, "D del", "stop %d routes the verbs", stop)
			assert.Containsf(t, row, "esc list", "stop %d: esc exits to the list", stop)
		}
		assert.NotContainsf(t, row, "quit",
			"stop %d: the quit key is gated on !IsEditing (#1961) — it never fires inside the form", stop)
	}
}

// TestTaskPaneEditActionFooterKeepsEscLabelWhereItFits is #3630's second half.
// The action row used to jump from its full form straight to a much shorter
// one, and of everything it dropped it dropped esc's LABEL — the one key here
// whose destination ("back to the list", not "out of the manager") is not
// guessable from the glyph.
func TestTaskPaneEditActionFooterKeepsEscLabelWhereItFits(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(nil))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	repo := newGitRepo(t)
	footerAt := func(width int) string {
		tp := NewTaskPane()
		tp.SetSize(width, 12)
		tp.SetTasks([]task.Task{{
			ID: "abc", Name: "nightly", Prompt: "do it", CronExpr: "0 0 * * *",
			ProjectPath: repo, Program: "claude", Enabled: true,
		}})
		tp.SetFocus(true)
		tp.EnterEditSelected()
		// The verbs only render where they fire (#5538): step off the opening
		// Name field onto the Trigger selector.
		tp.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
		require.Equal(t, taskFocusTrigger, tp.focusIndex)
		lines := strings.Split(stripANSI(tp.String()), "\n")
		return lines[len(lines)-1]
	}

	// The full form is 42 cells; the middle tier is 35. Every width between
	// them renders the abbreviated verbs and must keep esc's label.
	for _, w := range []int{35, 38, 41} {
		row := footerAt(w)
		assert.Containsf(t, row, "esc list",
			"width %d: esc's destination must be named wherever it fits: %q", w, row)
		assert.NotContainsf(t, row, "quit",
			"width %d: the quit key never fires inside the form (#1961): %q", w, row)
		assert.NotContainsf(t, row, "…", "width %d: the row must fit, not clip: %q", w, row)
		assert.NotContainsf(t, row, "run now",
			"width %d: the verbs still abbreviate — only the destinations are protected: %q", w, row)
	}

	// Narrower than the middle tier: esc's label is the thing that goes.
	narrow := footerAt(30)
	assert.Contains(t, narrow, "esc", "esc itself is never dropped: %q", narrow)
	assert.NotContains(t, narrow, "…", "the row must fit, not clip: %q", narrow)
}

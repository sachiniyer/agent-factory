package ui

import (
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/task"
)

// TestTaskPaneEditActionFooterFollowsFocus pins #5538 as #6400 narrowed it:
// the action row advertises a key only where it fires. At a text stop r/x/D
// (and q) are typed input, since #4838 routes the verb glyphs into the field,
// so the row lists no verb at all. It used to name them as the destination of
// the tab step ("tab → r run now · …"), and play-testers read that as "press
// r" and renamed the task. At a selector/button stop the verbs are live and
// are listed. The configured quit key is gated on !IsEditing in
// handleStateTasks (#1961), so it is dead at every edit stop and never
// advertised.
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
		taskFocusPrompt: true, taskFocusTarget: true, taskFocusHost: true, taskFocusPath: true,
	}
	for stop := 0; stop < taskFocusCount; stop++ {
		tp.focusIndex = stop
		tp.updateEditFocus()
		row := footer()
		if textStop[stop] {
			assert.NotRegexpf(t, verbKeyToken, row,
				"stop %d is a text field: a verb key here types into it (#6400): %q", stop, row)
			assert.NotContainsf(t, row, "→",
				"stop %d: no verb path either — it read as a live key (#6400): %q", stop, row)
			assert.Containsf(t, row, "tab next",
				"stop %d: tab moves on to the next stop and stays advertised: %q", stop, row)
			assert.Containsf(t, row, "esc list",
				"stop %d: esc still exits to the list, where n and r are live: %q", stop, row)
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

	// The full form is 46 cells; the middle tier is 39 (both carry the
	// toggle's [✓] state, #7135). Every width between them renders the
	// abbreviated verbs and must keep esc's label.
	for _, w := range []int{39, 42, 45} {
		row := footerAt(w)
		assert.Containsf(t, row, "esc list",
			"width %d: esc's destination must be named wherever it fits: %q", w, row)
		assert.NotContainsf(t, row, "quit",
			"width %d: the quit key never fires inside the form (#1961): %q", w, row)
		assert.NotContainsf(t, row, "…", "width %d: the row must fit, not clip: %q", w, row)
		assert.NotContainsf(t, row, "run now",
			"width %d: the verbs still abbreviate — only the destinations are protected: %q", w, row)
	}

	// Narrower than the middle tier: esc's label is the thing that goes. 34
	// is the overlay's content width at the 40-column hard minimum, the
	// narrowest the app ever sizes the form, and the narrowest tier fits it.
	narrow := footerAt(34)
	assert.Contains(t, narrow, "esc", "esc itself is never dropped: %q", narrow)
	assert.NotContains(t, narrow, "…", "the row must fit, not clip: %q", narrow)
}

// verbKeyToken matches a task verb or quit key standing as a key in a hint:
// r, x, D, n or q as its own word, or a slash-joined run of them ("r/x/D").
// Prose such as "type to edit" never matches, because no verb glyph stands
// alone in it.
var verbKeyToken = regexp.MustCompile(`(^|[\s·→])[rxDnq](/[rxDnq])*(\s|$)`)

// footerFixture is one shape of the task form: a cron task's editor, a watch
// task's editor (whose trigger value is a free-text command, not the schedule
// picker), and the create form.
type footerFixture struct {
	name string
	open func(t *testing.T, width int) *TaskPane
}

func footerFixtures(t *testing.T) []footerFixture {
	t.Helper()
	repo := newGitRepo(t)
	edit := func(tsk task.Task) func(*testing.T, int) *TaskPane {
		return func(t *testing.T, width int) *TaskPane {
			tp := NewTaskPane()
			// Tall enough that the form never windows: every hint row stays in
			// view, not only the pinned last one.
			tp.SetSize(width, 60)
			tsk.ProjectPath = repo
			tp.SetTasks([]task.Task{tsk})
			tp.SetFocus(true)
			tp.EnterEditSelected()
			require.True(t, tp.IsEditing())
			return tp
		}
	}
	return []footerFixture{
		{"cron editor", edit(task.Task{ID: "abc", Name: "nightly", Prompt: "do it",
			CronExpr: "0 0 * * *", Program: "claude", Enabled: true})},
		{"watch editor", edit(task.Task{ID: "w1", Name: "tailer", Prompt: "do it",
			WatchCmd: "tail -f log", Program: "claude", Enabled: true})},
		{"create form", func(t *testing.T, width int) *TaskPane {
			tp := NewTaskPane()
			tp.SetSize(width, 60)
			tp.SetFocus(true)
			tp.EnterCreateMode(repo)
			fillCreateForm(t, tp, "draft")
			require.True(t, tp.IsCreating())
			return tp
		}},
	}
}

// footerRows returns the form's hint rows: everything below the blank line
// that follows the Save/Create button. The editor prints two (the form-wide
// row and the contextual action row), the create form one.
func footerRows(tp *TaskPane) []string {
	lines := strings.Split(strings.TrimRight(stripANSI(tp.String()), "\n"), "\n")
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) != "" {
		last--
	}
	return lines[last+1:]
}

// focusTextStop puts focus on stop and reports whether it is a text stop for
// this form's shape.
func focusTextStop(tp *TaskPane, stop int) bool {
	tp.focusIndex = stop
	tp.updateEditFocus()
	return textStopAt(stop, tp.capApplies())
}

// TestTaskPaneTextStopFooterListsNoVerbKey is the #6400 regression, stated as
// the copy property. While a text field has focus, the footer must not list a
// key that types into that field. The play-test pressed the footer's r on the
// Name field and renamed the task to "hello-echor". It holds at every text
// stop, in every form shape, at every width tier the rows step down through.
func TestTaskPaneTextStopFooterListsNoVerbKey(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(nil))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	for _, fx := range footerFixtures(t) {
		for _, width := range []int{80, 52, 40, 34, 25, 20, 12} {
			for stop := 0; stop < taskFocusCount; stop++ {
				tp := fx.open(t, width)
				if !focusTextStop(tp, stop) {
					continue
				}
				rows := footerRows(tp)
				require.NotEmptyf(t, rows, "%s, width %d, stop %d: no hint rows rendered", fx.name, width, stop)
				for _, row := range rows {
					assert.NotRegexpf(t, verbKeyToken, row,
						"%s, width %d, stop %d is a text field: a verb key here types into it: %q",
						fx.name, width, stop, row)
					assert.NotContainsf(t, row, "→",
						"%s, width %d, stop %d: a verb path reads as a live key: %q", fx.name, width, stop, row)
				}
			}
		}
	}
}

// focusedFieldText is the text of the focused text stop: what a stray key
// would corrupt.
func focusedFieldText(tp *TaskPane) string {
	switch tp.focusIndex {
	case taskFocusName:
		return tp.editName.Value()
	case taskFocusTriggerValue:
		return tp.schedule.Cron() + "|" + tp.editWatch.Value()
	case taskFocusPrompt:
		return tp.editPrompt.Value()
	case taskFocusTarget:
		return tp.editTarget.Value()
	case taskFocusMaxRuns:
		return tp.editMaxRuns.Value()
	case taskFocusHost:
		return tp.editHost.Value()
	case taskFocusPath:
		return tp.editPath.Value()
	}
	return ""
}

// advertisedKeys reads the keys a hint row offers: the leading token of each
// "·"-separated fragment, with slash-joined runs ("tab/shift+tab", "r/x/D")
// split into their keys. Prose fragments ("type to edit") offer no key.
func advertisedKeys(row string) []string {
	var out []string
	for _, frag := range strings.Split(row, "·") {
		fields := strings.Fields(frag)
		if len(fields) == 0 || fields[0] == "type" {
			continue
		}
		out = append(out, strings.Split(fields[0], "/")...)
	}
	return out
}

func keyMsgFor(t *testing.T, k string) tea.KeyMsg {
	t.Helper()
	switch {
	case k == "tab" || strings.HasPrefix(k, "tab×"):
		return tea.KeyMsg{Type: tea.KeyTab}
	case k == "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case k == "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case k == "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case len([]rune(k)) == 1:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	t.Fatalf("hint advertises %q, which this test cannot press: teach keyMsgFor the key", k)
	return tea.KeyMsg{}
}

// TestTaskPaneTextStopFooterKeysNeverTypeIntoTheField is the same property
// checked by behaviour rather than by spelling. At every text stop it presses
// each key the footer advertises, on a fresh form, and requires the focused
// field's text to come out unchanged. The footer used to offer x and D on
// Name (they type), "enter save" on the prompt (Enter types a newline there)
// and, on the create form, "q quit" (q is typed since #1961).
func TestTaskPaneTextStopFooterKeysNeverTypeIntoTheField(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(nil))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	for _, fx := range footerFixtures(t) {
		for _, width := range []int{80, 30} {
			for stop := 0; stop < taskFocusCount; stop++ {
				probe := fx.open(t, width)
				if !focusTextStop(probe, stop) {
					continue
				}
				for _, row := range footerRows(probe) {
					for _, k := range advertisedKeys(row) {
						tp := fx.open(t, width)
						focusTextStop(tp, stop)
						before := focusedFieldText(tp)
						tp.HandleKeyPress(keyMsgFor(t, k))
						tp.focusIndex = stop
						assert.Equalf(t, before, focusedFieldText(tp),
							"%s, width %d, stop %d: the footer offers %q (row %q), and pressing it typed into the field",
							fx.name, width, stop, k, row)
					}
				}
			}
		}
	}
}

package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/task"
)

// #7175: the task manager, hooks and config editors report which row each line
// of their String() draws, and the app registers one click zone per row from
// that report. These tests check the report against what the frame actually
// drew, through oracles the report does not compute: the cursor marker's line
// must name the selected row, and each row's own text must sit on a line that
// names that row. Overflow cues, titles and hints name no row.

// drawnLines is a pane's String() split into lines with ANSI stripped.
func drawnLines(out string) []string {
	lines := strings.Split(out, "\n")
	for i := range lines {
		lines[i] = xansi.Strip(lines[i])
	}
	return lines
}

// lineWith returns the one drawn line containing needle, failing if there is
// not exactly one.
func lineWith(t *testing.T, lines []string, needle string) int {
	t.Helper()
	found := -1
	for i, l := range lines {
		if strings.Contains(l, needle) {
			require.Equal(t, -1, found, "%q is drawn on more than one line:\n%s", needle, strings.Join(lines, "\n"))
			found = i
		}
	}
	require.NotEqual(t, -1, found, "%q is not drawn:\n%s", needle, strings.Join(lines, "\n"))
	return found
}

// assertCueLinesAreChrome checks that no overflow cue or blank line names a row.
func assertCueLinesAreChrome(t *testing.T, lines []string, rows []int) {
	t.Helper()
	for k, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.Contains(l, "↓ ") && strings.Contains(l, "more") || strings.Contains(l, "↑ ") && strings.Contains(l, "more") {
			assert.Equal(t, -1, rows[k], "line %d (%q) is chrome but names row %d", k, l, rows[k])
		}
	}
}

func TestHooksPaneRowLinesFollowTheDrawnRows(t *testing.T) {
	cmds := []string{"make deps", "npm ci", "go mod download"}
	h := NewHooksPane()
	h.SetSize(60, 20)
	h.SetCommands(append([]string(nil), cmds...))
	h.SetFocus(true)

	for sel := range cmds {
		require.True(t, h.SelectRow(sel))
		require.Equal(t, sel, h.SelectedRow())
		lines := drawnLines(h.String())
		rows := h.RowLines()
		require.Len(t, rows, len(lines), "one entry per drawn line")
		for i, cmd := range cmds {
			assert.Equal(t, i, rows[lineWith(t, lines, cmd)], "the line drawing %q must name row %d", cmd, i)
		}
		assert.Equal(t, sel, rows[lineWith(t, lines, "▸ ")], "the cursor's line must name the selected row")
		assert.Equal(t, -1, rows[0], "the title names no row")
		assert.Equal(t, -1, rows[len(rows)-1], "the hint names no row")
		assertCueLinesAreChrome(t, lines, rows)
	}

	assert.False(t, h.SelectRow(len(cmds)), "a row past the list is refused")
	assert.False(t, h.SelectRow(-1))

	// A command being typed owns the pane: no row takes a click.
	h.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, h.IsEditing())
	assert.Nil(t, h.RowLines(), "an open edit field registers no rows")
	assert.False(t, h.SelectRow(0), "the cursor does not move under an open edit field")
}

// TestHooksPaneRowLinesSkipRowsTheOverflowHides: a list taller than the pane is
// cut, and the "↓ more" cue takes the last body row — the report must not name
// a hook on a line that draws the cue or a different hook.
func TestHooksPaneRowLinesSkipRowsTheOverflowHides(t *testing.T) {
	var cmds []string
	for i := 0; i < 8; i++ {
		cmds = append(cmds, fmt.Sprintf("hook-command-%d", i))
	}
	h := NewHooksPane()
	h.SetSize(60, 8)
	h.SetCommands(cmds)
	h.SetFocus(true)

	lines := drawnLines(h.String())
	rows := h.RowLines()
	require.Len(t, rows, len(lines))
	require.Len(t, lines, 8, "precondition: the pane is cut to its height")
	lineWith(t, lines, "↓ more")
	assertCueLinesAreChrome(t, lines, rows)
	for k, r := range rows {
		if r >= 0 {
			assert.Contains(t, lines[k], cmds[r], "line %d names hook %d but draws %q", k, r, lines[k])
		}
	}
}

func overlayRowTasks(n int) []task.Task {
	var tasks []task.Task
	for i := 0; i < n; i++ {
		tasks = append(tasks, task.Task{
			ID:       fmt.Sprintf("task-%d", i),
			Name:     fmt.Sprintf("task-name-%d", i),
			Prompt:   fmt.Sprintf("prompt body %d", i),
			CronExpr: "0 3 * * *",
			Enabled:  true,
		})
	}
	return tasks
}

func TestTaskPaneRowLinesFollowTheDrawnRows(t *testing.T) {
	tasks := overlayRowTasks(3)
	s := NewTaskPane()
	s.SetSize(70, 40)
	s.SetTasks(tasks)
	s.SetFocus(true)
	require.False(t, s.IsEditing(), "precondition: list mode")

	for sel := range tasks {
		require.True(t, s.SelectRow(sel))
		require.Equal(t, sel, s.SelectedRow())
		lines := drawnLines(s.String())
		rows := s.RowLines()
		require.Len(t, rows, len(lines), "one entry per drawn line")
		for i, tsk := range tasks {
			assert.Equal(t, i, rows[lineWith(t, lines, tsk.Name)], "the line drawing %q must name row %d", tsk.Name, i)
		}
		assert.Equal(t, sel, rows[lineWith(t, lines, "▸ ")], "the cursor's line must name the selected row")
		// The selected task expands its detail lines, and they belong to it.
		assert.Equal(t, sel, rows[lineWith(t, lines, tasks[sel].Prompt)], "the selected task's detail lines name it too")
		spanned := 0
		for _, r := range rows {
			if r == sel {
				spanned++
			}
		}
		assert.Greater(t, spanned, 1, "the selected task's row spans its header and details")
		assert.Equal(t, -1, rows[0], "the title names no row")
		assert.Equal(t, -1, rows[len(rows)-1], "the hint names no row")
		assertCueLinesAreChrome(t, lines, rows)
	}

	// The edit form owns the pane: no row takes a click.
	s.EnterEditSelected()
	require.True(t, s.IsEditing())
	assert.Nil(t, s.RowLines(), "the edit form registers no rows")
	assert.False(t, s.SelectRow(0))
}

// TestTaskPaneRowLinesFollowTheWindow: a list taller than the pane windows to
// keep the selection visible; the report must be cut from that same window.
func TestTaskPaneRowLinesFollowTheWindow(t *testing.T) {
	tasks := overlayRowTasks(8)
	s := NewTaskPane()
	s.SetSize(70, 12)
	s.SetTasks(tasks)
	s.SetFocus(true)

	for sel := range tasks {
		require.True(t, s.SelectRow(sel))
		lines := drawnLines(s.String())
		rows := s.RowLines()
		require.Len(t, rows, len(lines))
		require.LessOrEqual(t, len(lines), 12, "precondition: the list is windowed to the pane")
		assert.Equal(t, sel, rows[lineWith(t, lines, "▸ ")], "selection %d: the cursor's line must name it", sel)
		assertCueLinesAreChrome(t, lines, rows)
		for k, r := range rows {
			if r < 0 {
				continue
			}
			for i, tsk := range tasks {
				if i != r && strings.Contains(lines[k], tsk.Name+" ") {
					t.Errorf("selection %d: line %d names task %d but draws %q", sel, k, r, lines[k])
				}
			}
		}
	}
}

// TestConfigPaneRowLinesFollowTheScrolledWindow walks the cursor through a
// windowed config list, as TestConfigPaneKeepsTheSelectionVisible does, and
// checks the report against the cursor marker at every step.
func TestConfigPaneRowLinesFollowTheScrolledWindow(t *testing.T) {
	c := NewConfigPane()
	c.SetSize(64, paneHeight)
	c.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml", nil)
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()

	for step := 0; step < 40; step++ {
		lines := drawnLines(c.String())
		rows := c.RowLines()
		require.Len(t, rows, len(lines), "step %d: one entry per drawn line", step)
		assert.Equal(t, c.SelectedRow(), rows[lineWith(t, lines, "›")],
			"step %d: the cursor's line must name the selected row", step)
		assert.Equal(t, -1, rows[0], "step %d: the title names no row", step)
		assertCueLinesAreChrome(t, lines, rows)
		for k, r := range rows {
			if r < 0 {
				continue
			}
			require.True(t, c.rows[r].isSelectable(), "step %d: line %d names row %d, which the cursor cannot land on", step, k, r)
			if e := c.rows[r].entry; e != nil && (k == 0 || rows[k-1] != r) && k > 0 && !strings.Contains(lines[k-1], "↑") {
				// The first drawn line of an entry row is its key line, unless the
				// window starts inside the row.
				assert.Contains(t, lines[k], e.Key, "step %d: line %d names %q but draws %q", step, k, e.Key, lines[k])
			}
		}
		c.move(1)
	}
}

func TestConfigPaneSelectRowRefusesWhatTheCursorSkips(t *testing.T) {
	c := newTestConfigPane(t)
	heading, entry := -1, -1
	for i, row := range c.rows {
		if !row.isSelectable() && heading < 0 {
			heading = i
		}
		if row.entry != nil && i != c.selectedIdx && entry < 0 {
			entry = i
		}
	}
	require.GreaterOrEqual(t, heading, 0, "precondition: the list has a heading row")
	require.GreaterOrEqual(t, entry, 0, "precondition: the list has a second entry row")

	before := c.SelectedRow()
	assert.False(t, c.SelectRow(heading), "a heading is refused, as j/k skip it")
	assert.Equal(t, before, c.SelectedRow())
	assert.True(t, c.SelectRow(entry))
	assert.Equal(t, entry, c.SelectedRow())

	// An open value field owns the pane.
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, c.IsEditing())
	assert.Nil(t, c.RowLines(), "an open value field registers no rows")
	assert.False(t, c.SelectRow(before))
}

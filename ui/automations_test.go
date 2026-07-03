package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestAutomations builds a strip over a fresh projection with a frozen
// clock so next-run derivation is deterministic.
func newTestAutomations(tasks []task.Task) *AutomationsPane {
	proj := store.NewProjection()
	proj.SetTasks(tasks)
	a := NewAutomationsPane(proj)
	a.now = func() time.Time {
		return time.Date(2026, time.July, 2, 2, 0, 0, 0, time.UTC)
	}
	return a
}

func stripTasks() []task.Task {
	last := time.Date(2026, time.July, 1, 3, 0, 0, 0, time.UTC)
	return []task.Task{
		{ID: "1", Name: "nightly-sweep", CronExpr: "0 3 * * *", Enabled: true, LastRunAt: &last},
		{ID: "2", Name: "ci-watch", WatchCmd: "tail -f ci.log", Enabled: false},
	}
}

// TestAutomationsStripCompactRows pins the RFC §2.1 strip row shape: enabled
// glyph, name, trigger, next/last run.
func TestAutomationsStripCompactRows(t *testing.T) {
	a := newTestAutomations(stripTasks())
	a.SetRect(layout.Rect{W: 100, H: 3})

	out := a.View()
	require.Contains(t, out, "Automations")
	assert.Contains(t, out, "[✓]  nightly-sweep  0 3 * * *  next Jul 02 03:00 · last Jul 01 03:00",
		"an enabled cron task shows glyph, name, trigger, next fire, and last run")
	assert.Contains(t, out, "[✗]  ci-watch  watch: tail -f ci.log  stopped",
		"a disabled watch task shows glyph, name, command, and supervision state")
}

// TestAutomationsStripOneLineSummary covers the <80-col degradation (RFC
// §2.6): a single summary line, still exactly rect-sized.
func TestAutomationsStripOneLineSummary(t *testing.T) {
	a := newTestAutomations(stripTasks())
	a.SetRect(layout.Rect{W: 70, H: 1})
	a.SetCompact(true)

	out := a.View()
	requireExactRect(t, out, layout.Rect{W: 70, H: 1}, "compact strip")
	assert.Contains(t, out, "Automations: 2 (1 on)")
}

// TestAutomationsStripFocusExpandsToManager: focusing the strip swaps the
// compact rows for the full TaskPane manager, with input focus live.
func TestAutomationsStripFocusExpandsToManager(t *testing.T) {
	a := newTestAutomations(stripTasks())
	a.taskPane.SetTasks(stripTasks())
	a.SetRect(layout.Rect{W: 100, H: 14})

	a.Focus()
	require.True(t, a.Focused())
	require.True(t, a.taskPane.HasFocus(), "focusing the strip forwards input focus to the manager")

	out := a.View()
	requireExactRect(t, out, layout.Rect{W: 100, H: 14}, "focused strip")
	assert.Contains(t, out, "Tasks", "the manager's own view renders in place")
	assert.Contains(t, out, "n new", "the manager's key hints are live")

	a.Blur()
	assert.False(t, a.taskPane.HasFocus())
	assert.Contains(t, a.View(), "Automations", "blurred strip returns to compact rows")
}

// TestAutomationsStripKeyRouting: the focused strip consumes manager keys but
// bubbles Tab/Shift-Tab (the focus ring) outside a form, and q (quit) always.
func TestAutomationsStripKeyRouting(t *testing.T) {
	a := newTestAutomations(stripTasks())
	a.taskPane.SetTasks(stripTasks())
	a.Focus()

	_, consumed := a.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	assert.True(t, consumed, "list navigation is consumed")

	_, consumed = a.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	assert.False(t, consumed, "Tab bubbles to the focus ring outside a form")

	_, consumed = a.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	assert.False(t, consumed, "q bubbles so the root can quit")

	// Inside the edit form Tab moves fields and must be consumed.
	a.taskPane.EnterCreateMode(t.TempDir())
	_, consumed = a.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	assert.True(t, consumed, "Tab inside the create form moves between fields")
}

// TestAutomationsStripExactRectWithOverflow: more tasks than rows must
// truncate, never overflow the strip.
func TestAutomationsStripExactRectWithOverflow(t *testing.T) {
	var tasks []task.Task
	for i := 0; i < 30; i++ {
		tasks = append(tasks, task.Task{
			ID: "t", Name: strings.Repeat("very-long-task-name-", 6), CronExpr: "0 3 * * *", Enabled: true,
		})
	}
	a := newTestAutomations(tasks)
	r := layout.Rect{W: 90, H: 3}
	a.SetRect(r)
	requireExactRect(t, a.View(), r, "overflowing strip")
}

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

// TestAutomationsFocusShowsCursorNotManager: focusing the section adds a
// cursor to the compact rows — the full TaskPane manager must NOT render
// in-rail (it opens as a modal overlay, #1096 play-test); the section stays
// exactly rect-sized.
func TestAutomationsFocusShowsCursorNotManager(t *testing.T) {
	a := newTestAutomations(stripTasks())
	a.SetRect(layout.Rect{W: 100, H: 3})

	a.Focus()
	require.True(t, a.Focused())
	require.False(t, a.taskPane.HasFocus(),
		"focusing the section must not focus the manager — the overlay open does that")

	out := a.View()
	requireExactRect(t, out, layout.Rect{W: 100, H: 3}, "focused section")
	assert.Contains(t, out, "▸[✓]", "the cursor marks the selected row")
	assert.NotContains(t, out, "Tasks", "the manager must not render in-rail")

	a.ScrollDown()
	assert.Contains(t, a.View(), "▸[✗]", "the cursor follows ScrollDown")
	assert.Equal(t, 1, a.SelectedTaskIndex())

	a.Blur()
	assert.NotContains(t, a.View(), "▸", "the cursor leaves with focus")
}

// TestAutomationsStripKeyRouting: the focused section consumes only its
// cursor keys; everything else — Enter (overlay open), Esc (focus ring), Tab,
// q — bubbles to the root.
func TestAutomationsStripKeyRouting(t *testing.T) {
	a := newTestAutomations(stripTasks())
	a.Focus()

	_, consumed := a.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	assert.True(t, consumed, "cursor navigation is consumed")
	assert.Equal(t, 1, a.SelectedTaskIndex())
	_, consumed = a.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	assert.True(t, consumed)
	assert.Equal(t, 0, a.SelectedTaskIndex())

	_, consumed = a.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	assert.False(t, consumed, "Enter bubbles so the root can open the manager overlay")
	_, consumed = a.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	assert.False(t, consumed, "Tab bubbles to the focus ring")
	_, consumed = a.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	assert.False(t, consumed, "q bubbles so the root can quit")
}

// TestAutomationsTitleWidthAware pins the #1096 play-test fix: the header's
// hint segments drop right-to-left ("H hooks" first) and the name shrinks
// with an ellipsis so the "S manage" affordance survives even the 22-col
// rail minimum — never a bare hard clamp.
func TestAutomationsTitleWidthAware(t *testing.T) {
	tasks := stripTasks()

	wide := newTestAutomations(tasks)
	wide.SetRect(layout.Rect{W: 60, H: 3})
	out := wide.View()
	assert.Contains(t, out, "S manage", "wide rail shows the manage hint")
	assert.Contains(t, out, "H hooks", "wide rail shows the hooks hint")

	mid := newTestAutomations(tasks)
	mid.SetRect(layout.Rect{W: 36, H: 3})
	out = mid.View()
	assert.Contains(t, out, "S manage", "36-col rail keeps the manage hint")
	assert.NotContains(t, out, "H hooks", "the hooks hint drops first under width pressure")

	narrow := newTestAutomations(tasks)
	narrow.SetRect(layout.Rect{W: 22, H: 3})
	out = narrow.View()
	requireExactRect(t, out, layout.Rect{W: 22, H: 3}, "22-col section")
	assert.Contains(t, out, "S manage", "22-col rail still shows the manage affordance")
	assert.Contains(t, out, "…", "the shrunk name marks its cut with an ellipsis")

	// The 1-line degraded summary applies the same policy.
	compact := newTestAutomations(tasks)
	compact.SetRect(layout.Rect{W: 22, H: 1})
	compact.SetCompact(true)
	out = compact.View()
	requireExactRect(t, out, layout.Rect{W: 22, H: 1}, "22-col compact summary")
	assert.Contains(t, out, "S manage", "the compact summary keeps the manage affordance")
}

// TestAutomationsEmptyStateEllipsized: the no-tasks hint ellipsizes instead
// of hard-clamping mid-word.
func TestAutomationsEmptyStateEllipsized(t *testing.T) {
	a := newTestAutomations(nil)
	a.SetRect(layout.Rect{W: 22, H: 3})
	out := a.View()
	requireExactRect(t, out, layout.Rect{W: 22, H: 3}, "empty section")
	assert.Contains(t, out, "no tasks")
	assert.Contains(t, out, "…", "the truncated hint marks its cut")
}

// TestAutomationsCursorScrollsIntoView: with more tasks than rows, moving the
// cursor below the fold scrolls the window so the selection stays visible.
func TestAutomationsCursorScrollsIntoView(t *testing.T) {
	var tasks []task.Task
	for i := 0; i < 6; i++ {
		tasks = append(tasks, task.Task{
			ID: string(rune('a' + i)), Name: "task-" + string(rune('a'+i)),
			CronExpr: "0 3 * * *", Enabled: true,
		})
	}
	a := newTestAutomations(tasks)
	a.SetRect(layout.Rect{W: 40, H: 3})
	a.Focus()

	for i := 0; i < 5; i++ {
		a.ScrollDown()
	}
	out := a.View()
	requireExactRect(t, out, layout.Rect{W: 40, H: 3}, "scrolled section")
	assert.Contains(t, out, "▸[✓]  task-f", "the cursor's row scrolled into view")
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

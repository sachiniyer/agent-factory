package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
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

// TestAutomationsCollapsedRowsAreTitleOnly pins the #1126 collapsed shape:
// every row is just the enabled glyph and the task title — no always-on
// trailing cron/next/last text.
func TestAutomationsCollapsedRowsAreTitleOnly(t *testing.T) {
	a := newTestAutomations(stripTasks())
	a.SetRect(layout.Rect{W: 100, H: 3})

	out := a.View()
	require.Contains(t, out, "Automations")
	assert.Contains(t, out, "[✓]  nightly-sweep", "an enabled task shows its glyph and title")
	assert.Contains(t, out, "[✗]  ci-watch", "a disabled task shows its glyph and title")
	assert.NotContains(t, out, "0 3 * * *", "the collapsed row hides the cron trigger")
	assert.NotContains(t, out, "next Jul 02 03:00", "the collapsed row hides the next-run detail")
	assert.NotContains(t, out, "watch: tail -f ci.log", "the collapsed row hides the watch command")
}

// TestAutomationsExpandedRowRevealsDetail pins the #1126 expansion: the focused
// cursor's row reveals its trigger and next/last-run detail on a line beneath
// the title, and no other row does.
func TestAutomationsExpandedRowRevealsDetail(t *testing.T) {
	a := newTestAutomations(stripTasks())
	a.SetRect(layout.Rect{W: 100, H: 4})
	a.Focus()

	out := a.View()
	assert.Contains(t, out, "▾[✓]  nightly-sweep", "the focused row is marked expanded")
	assert.Contains(t, out, "0 3 * * *", "the expanded row reveals its cron trigger")
	assert.Contains(t, out, "next Jul 02 03:00 · last Jul 01 03:00",
		"the expanded row reveals its next/last-run detail")
	assert.NotContains(t, out, "watch: tail -f ci.log",
		"a collapsed (unselected) row keeps its detail hidden")

	a.ScrollDown()
	out = a.View()
	assert.Contains(t, out, "▾[✗]  ci-watch", "expansion follows the cursor")
	assert.Contains(t, out, "watch: tail -f ci.log", "the newly expanded row reveals its command")
	assert.NotContains(t, out, "0 3 * * *", "the previously expanded row re-collapses")
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
	assert.Contains(t, out, "▾[✓]", "the cursor marks (and expands) the selected row")
	assert.NotContains(t, out, "Tasks", "the manager must not render in-rail")

	a.ScrollDown()
	assert.Contains(t, a.View(), "▾[✗]", "the cursor follows ScrollDown")
	assert.Equal(t, 1, a.SelectedTaskIndex())

	a.Blur()
	assert.NotContains(t, a.View(), "▾", "the cursor (and expansion) leaves with focus")
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
// hint segments drop right-to-left (hooks first) and the name shrinks with an
// ellipsis so the manage affordance survives even the 22-col rail minimum —
// never a bare hard clamp.
func TestAutomationsTitleWidthAware(t *testing.T) {
	tasks := stripTasks()
	manageHint := automationHelpKey(keys.KeyTaskList) + " manage"
	hooksHint := automationHelpKey(keys.KeyHooks) + " hooks"

	wide := newTestAutomations(tasks)
	wide.SetRect(layout.Rect{W: 60, H: 3})
	out := wide.View()
	assert.Contains(t, out, manageHint, "wide rail shows the manage hint")
	assert.Contains(t, out, hooksHint, "wide rail shows the hooks hint")
	assert.NotContains(t, out, "S manage", "the old task-manager key must not be advertised by default")
	assert.NotContains(t, out, "H hooks", "the old hooks key must not be advertised by default")

	mid := newTestAutomations(tasks)
	mid.SetRect(layout.Rect{W: 36, H: 3})
	out = mid.View()
	assert.Contains(t, out, manageHint, "36-col rail keeps the manage hint")
	assert.NotContains(t, out, hooksHint, "the hooks hint drops first under width pressure")

	narrow := newTestAutomations(tasks)
	narrow.SetRect(layout.Rect{W: 22, H: 3})
	out = narrow.View()
	requireExactRect(t, out, layout.Rect{W: 22, H: 3}, "22-col section")
	assert.Contains(t, out, manageHint, "22-col rail still shows the manage affordance")
	assert.Contains(t, out, "…", "the shrunk name marks its cut with an ellipsis")

	// The 1-line degraded summary applies the same policy.
	compact := newTestAutomations(tasks)
	compact.SetRect(layout.Rect{W: 22, H: 1})
	compact.SetCompact(true)
	out = compact.View()
	requireExactRect(t, out, layout.Rect{W: 22, H: 1}, "22-col compact summary")
	assert.Contains(t, out, manageHint, "the compact summary keeps the manage affordance")
}

func TestAutomationsHintsReflectKeymapRebinds(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(map[string][]string{
		"tasks": {"f"},
		"hooks": {"g"},
	}))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	a := newTestAutomations(nil)
	a.SetRect(layout.Rect{W: 80, H: 3})

	out := a.View()
	assert.Contains(t, out, "f manage", "manage title hint follows the rebound task-manager key")
	assert.Contains(t, out, "g hooks", "hooks title hint follows the rebound hooks key")
	assert.Contains(t, out, "press f, then n", "empty-state hint follows the rebound task-manager key")
	assert.NotContains(t, out, "m manage", "default task-manager key must not be hardcoded")
	assert.NotContains(t, out, "e hooks", "default hooks key must not be hardcoded")
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
	assert.Contains(t, out, "▾[✓]  task-f", "the cursor's row scrolled into view")
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

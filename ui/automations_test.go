package ui

import (
	"fmt"
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

// TestAutomationsExpandedRowOmitsUnsatisfiableNextRun is the rail's half of the
// same trap the schedule picker's Custom preview hits: "0 0 31 2 *" validates
// and parses, but robfig's Next gives up after five years and returns the ZERO
// time.Time, which formats as a plausible-looking "next Jan 01 00:00". The row
// must not promise a fire time the task will never reach.
func TestAutomationsExpandedRowOmitsUnsatisfiableNextRun(t *testing.T) {
	a := newTestAutomations([]task.Task{
		{ID: "1", Name: "feb-31", CronExpr: "0 0 31 2 *", Enabled: true},
	})
	a.SetRect(layout.Rect{W: 100, H: 4})
	a.Focus()

	out := a.View()
	require.Contains(t, out, "feb-31", "precondition: the row renders at all")
	assert.NotContains(t, out, "next Jan 01 00:00",
		"the zero time must never be formatted as a real next run:\n%s", out)
	assert.Contains(t, out, "No upcoming run",
		"say so plainly rather than dropping the fragment:\n%s", out)
}

// TestAutomationsExpandedRowRevealsDetail pins the #1126 expansion: the focused
// cursor's row reveals its trigger and next/last-run detail on a line beneath
// the title, and no other row does.
func TestAutomationsExpandedRowRevealsDetail(t *testing.T) {
	a := newTestAutomations(stripTasks())
	// title + the expanded row (title + detail) + a collapsed row + the reserved
	// bottom-margin row (#1560) — the size the grid grows the section to for two
	// tasks (2 + margin + 2).
	a.SetRect(layout.Rect{W: 100, H: 5})
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

// TestAutomationsReservesBottomMargin pins #1560: the section always keeps its
// last row blank so the workspace frame's bottom border can never land on the
// same line as a task row (or the compact summary). It mirrors the sidebar's
// leading blank row, which keeps the workspace's TOP border off the rail.
func TestAutomationsReservesBottomMargin(t *testing.T) {
	tasks := make([]task.Task, 8)
	for i := range tasks {
		tasks[i] = task.Task{
			ID: fmt.Sprintf("%d", i), Name: fmt.Sprintf("task-%d", i),
			CronExpr: "0 3 * * *", Enabled: true,
		}
	}
	lastBlank := func(lines []string) bool {
		return strings.TrimRight(lines[len(lines)-1], " ") == ""
	}

	// Compact summary: the region is 2 rows (summary + reserved margin).
	compact := newTestAutomations(tasks)
	compact.SetRect(layout.Rect{W: 40, H: layout.AutomationsCompactRows})
	compact.SetCompact(true)
	clines := plainLines(compact.View())
	require.Len(t, clines, layout.AutomationsCompactRows)
	assert.Contains(t, clines[0], "Automations")
	assert.True(t, lastBlank(clines), "the compact summary reserves a blank bottom-margin row")

	// Full mode, focused, with more tasks than fit: content wants every row, but
	// the last row still stays blank.
	full := newTestAutomations(tasks)
	full.SetRect(layout.Rect{W: 40, H: 5})
	full.Focus()
	flines := plainLines(full.View())
	require.Len(t, flines, 5)
	assert.Contains(t, strings.Join(flines[:len(flines)-1], "\n"), "task-",
		"tasks render in the rows above the reserved margin")
	assert.True(t, lastBlank(flines),
		"the last row stays blank so the workspace border can't abut a task row")

	// No capacity regression below the grid's floor: a direct caller that hands
	// the pane a tighter, content-exact full-mode rect than the grid ever
	// produces keeps every task row rather than losing one to the margin. The
	// grid always sizes the real section >= AutomationsRows tall, so this is a
	// direct-caller-only path — the margin only matters at the grid's real sizes.
	tight := newTestAutomations(stripTasks())
	require.Less(t, 3, layout.AutomationsRows, "H:3 must be below the margin floor for this to test the tight path")
	tight.SetRect(layout.Rect{W: 100, H: 3})
	out := tight.View()
	assert.Contains(t, out, "nightly-sweep", "H:3 full mode keeps the first task row")
	assert.Contains(t, out, "ci-watch", "H:3 full mode keeps the second task row (no capacity regression)")
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
	assert.Contains(t, out, "No tasks")
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

// TestAutomationsEmptyStateUsesSentenceCase is #3632, pinning the rail's
// no-tasks line positively at a width that does not ellipsize it.
func TestAutomationsEmptyStateUsesSentenceCase(t *testing.T) {
	a := newTestAutomations(nil)
	a.SetRect(layout.Rect{W: 60, H: 3})

	out := stripANSI(a.View())
	assert.Contains(t, out, "No tasks — press",
		"the empty automations section renders in sentence case:\n%s", out)
	assert.NotContains(t, out, "no tasks — press",
		"the lowercase form must be gone:\n%s", out)
}

// railHeaderLine renders the automations section at a rail width and returns its
// header row with trailing padding stripped.
func railHeaderLine(t *testing.T, tasks []task.Task, w int) string {
	t.Helper()
	a := newTestAutomations(tasks)
	a.SetRect(layout.Rect{W: w, H: 4})
	return strings.TrimRight(strings.Split(stripANSI(a.View()), "\n")[0], " ")
}

func nSimpleTasks(n int) []task.Task {
	var out []task.Task
	for i := 0; i < n; i++ {
		out = append(out, task.Task{
			ID: fmt.Sprintf("t%d", i), Name: fmt.Sprintf("task-%d", i),
			CronExpr: "0 3 * * *", Enabled: true,
		})
	}
	return out
}

// TestAutomationsHeaderNeverEllipsizesIntoTheSeparator is #3630. Below 100
// columns the header rendered "Automation…· m manage": the " · " separator's
// leading space lived on the END of the title, so every shrink of the title ate
// it and the ellipsis welded itself to the separator, reading as one mangled
// token rather than a truncated word beside a hint.
func TestAutomationsHeaderNeverEllipsizesIntoTheSeparator(t *testing.T) {
	for w := 8; w <= 45; w++ {
		line := railHeaderLine(t, nSimpleTasks(2), w)
		require.NotContainsf(t, line, "…·",
			"width %d: the ellipsis must never abut the separator: %q", w, line)
		if i := strings.Index(line, "·"); i > 0 {
			require.Equalf(t, " ", line[i-1:i],
				"width %d: the ' · ' separator must keep its leading space: %q", w, line)
		}
	}
}

// The header's only information is how many tasks exist. The width fallback used
// to replace the whole title with an ellipsized constant, so at every width below
// 110 columns the header with two tasks was byte-identical to the header with
// none (#3630).
func TestAutomationsHeaderKeepsTheCountAtEveryWidth(t *testing.T) {
	for w := 8; w <= 45; w++ {
		empty := railHeaderLine(t, nil, w)
		busy := railHeaderLine(t, nSimpleTasks(2), w)
		require.NotEqualf(t, empty, busy,
			"width %d: a header that cannot say how many tasks exist is the same line either way: %q", w, busy)
		if w >= 15 {
			require.Containsf(t, busy, "(2)",
				"width %d: the count survives every form down to the rail minimum — it is the only information the header carries: %q", w, busy)
		}
	}
}

// The ladder itself: what is shed, and in what order. The manage affordance is
// the last thing cut — the shipped contract TestAutomationsTitleWidthAware pins
// — so at the 22-column rail minimum the noun ellipsizes beside it, with the
// separator and the count both intact.
func TestAutomationsHeaderDegradationOrder(t *testing.T) {
	for _, tc := range []struct {
		w    int
		want string
	}{
		{40, " Automations (2) · m manage · e hooks"}, // everything
		{37, " Automations (2) · m manage · e hooks"}, // exactly
		{36, " Automations (2) · m manage"},           // hooks drops first
		{27, " Automations (2) · m manage"},           // exactly
		{26, " Automatio… (2) · m manage"},            // then the noun shrinks, counts intact
		{22, " Autom… (2) · m manage"},                // the 22-col rail minimum (#1090)
		{20, " Aut… (2) · m manage"},                  // …and it keeps shrinking
		{15, " (2) · m manage"},                       // the noun goes, counts and hint stay
	} {
		require.Equalf(t, tc.want, railHeaderLine(t, nSimpleTasks(2), tc.w),
			"rail width %d", tc.w)
	}
}

// The compact one-liner (#2.6, <80 cols) rides the same ladder and keeps its own
// richer counts.
func TestAutomationsCompactHeaderKeepsItsCounts(t *testing.T) {
	a := newTestAutomations(nSimpleTasks(2))
	a.SetCompact(true)
	a.SetRect(layout.Rect{W: 40, H: 1})
	line := strings.TrimRight(stripANSI(a.View()), " ")
	require.Contains(t, line, "2 (2 on)", "the compact summary must keep both numbers")
	require.NotContains(t, line, "…·", "and must not ellipsize into the separator")
}

// TestAutomationsCompactHeaderKeepsTheHintWholeAtEveryCount answers the Codex
// review on #3641. The compact summary carries TWO numbers, and at three digits
// "100 (100 on)" is 12 cells — the counts plus the guaranteed hint needed 24 of
// a 22-column rail, so the fallback clipped and truncated "m manage" itself. The
// 22-column rail is the supported minimum (#1090), not a size below it, so that
// broke the contract that the affordance is the last thing cut at exactly the
// width the contract is about.
func TestAutomationsCompactHeaderKeepsTheHintWholeAtEveryCount(t *testing.T) {
	manageHint := automationHelpKey(keys.KeyTaskList) + " manage"
	for _, n := range []int{0, 2, 9, 10, 99, 100, 999} {
		a := newTestAutomations(nSimpleTasks(n))
		a.SetCompact(true)
		a.SetRect(layout.Rect{W: 22, H: 1})
		line := strings.TrimRight(stripANSI(a.View()), " ")

		require.Containsf(t, line, manageHint,
			"%d tasks: the manage affordance is the last thing cut, and 22 is the supported rail minimum: %q", n, line)
		require.NotContainsf(t, line, "…"+railHintSeparator,
			"%d tasks: the separator must stay intact: %q", n, line)
		require.Containsf(t, line, fmt.Sprintf("%d", n),
			"%d tasks: the task count survives: %q", n, line)
	}
}

package ui

import (
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStatusBarRendersHintsOverErrorLine pins the merged region (#1024 PR 4):
// the menu hints on top, the error line underneath, exactly rect-sized.
func TestStatusBarRendersHintsOverErrorLine(t *testing.T) {
	menu := NewMenu()
	errBox := NewErrBox()
	sb := NewStatusBar(menu, errBox)
	r := layout.Rect{W: 100, H: layout.StatusBarRows}
	sb.SetRect(r)

	out := sb.View()
	requireExactRect(t, out, r, "status bar")
	assert.Contains(t, out, "new", "default menu hints render in the bar")

	errBox.SetError(errors.New("boom-error"))
	out = sb.View()
	requireExactRect(t, out, r, "status bar with error")
	assert.Contains(t, out, "boom-error", "the error line renders inside the bar")

	errBox.Clear()
	assert.NotContains(t, sb.View(), "boom-error")
}

// TestMenuFocusRegionSwitchesHints: the status-bar hints are context-sensitive
// per focus (RFC §2.1) — the automations strip advertises its own option set,
// and returning focus to the tree restores the session hints.
func TestMenuFocusRegionSwitchesHints(t *testing.T) {
	menu := NewMenu()
	menu.SetSize(120, 1)

	menu.SetFocusRegion(layout.RegionTree)
	base := menu.String()
	require.Contains(t, base, "new", "tree focus shows the session hints")

	menu.SetFocusRegion(layout.RegionAutomations)
	auto := menu.String()
	assert.Contains(t, auto, "hooks", "automations focus advertises the hooks overlay")
	assert.Contains(t, auto, "focus", "the focus-ring cycle key is advertised")
	assert.NotContains(t, auto, "new remote", "session-creation hints leave with tree focus")

	menu.SetFocusRegion(layout.RegionTree)
	assert.Equal(t, base, menu.String(), "tree focus restores the original hints")
}

// TestMenuNarrowWidthKeepsHelpAndQuit pins the hint priority (#1083
// play-test): when the instance hint row is wider than the bar, low-value
// hints are dropped first and `? help` / `q quit` are NEVER dropped — before
// this, the exact-rect clamp cut the RIGHT edge, so help/quit were the first
// hints to vanish on narrow terminals. #1422 moved tab/pane discovery above
// lower-frequency session actions, but help/quit remain the hard floor.
func TestMenuNarrowWidthKeepsHelpAndQuit(t *testing.T) {
	m := NewMenu()
	m.SetInstance(readyUIInstance())

	for _, w := range []int{110, 80, 60, 45} {
		m.SetSize(w, 1)
		out := m.String()
		assert.LessOrEqualf(t, lipgloss.Width(out), w,
			"width %d: the prioritized row must fit the bar", w)
		assert.Containsf(t, out, "q quit", "width %d: quit must survive", w)
		assert.Containsf(t, out, "? help", "width %d: help must survive", w)
	}

	// At a roomy width nothing is dropped: the scroll hints still render, and
	// their copy scopes the binding to AF's preview rather than attached input.
	m.SetSize(200, 1)
	assert.Contains(t, m.String(), "ctrl+u preview scroll", "no drops at full width")
	assert.Contains(t, m.String(), "ctrl+d preview scroll", "no drops at full width")
	assert.Contains(t, m.String(), "q quit")
}

// TestMenuLimitBlockedRowFitsNarrowWidths covers the WIDEST instance row in the
// product and closes #2085. A usage-limit-blocked session is the only state that
// shows two extra hints — `c retry limit` and `F hand off` — on a row that was
// already ~108 cells before either existed. Adding a hint is a WIDTH change
// (#1936/#1083), and TestMenuNarrowWidthKeepsHelpAndQuit above uses a Ready
// instance, so before this the widest row in the product was the one nothing
// measured.
//
// #2085 was the consequence: `c retry limit` was absent from hintDropOrder,
// which read as "never drop it" but actually meant the row had no give left once
// everything below it was gone. At 45 cells it rendered 48 and the exact-rect
// clamp cut the RIGHT edge — taking `? help` and `q quit` with it, which is the
// #1083 failure re-entered through the one row the priority list did not cover.
// Both limit hints are now droppable-last: they outlast every other optional
// hint and still let the row degrade instead of overflowing.
func TestMenuLimitBlockedRowFitsNarrowWidths(t *testing.T) {
	m := NewMenu()
	inst := &session.Instance{}
	inst.SetLimitReached(time.Time{})
	m.SetInstance(inst)

	// 40 and 45 are the widths that used to overflow. Nothing may exceed the bar
	// at ANY width — that is the whole contract of the priority list.
	for _, w := range []int{110, 80, 70, 61, 60, 50, 45, 40} {
		m.SetSize(w, 1)
		out := m.String()
		assert.LessOrEqualf(t, lipgloss.Width(out), w,
			"width %d: the limit-blocked row must fit the bar (#2085)", w)
		assert.Containsf(t, out, "q quit", "width %d: quit must survive", w)
		assert.Containsf(t, out, "? help", "width %d: help must survive", w)
	}

	// limitRowFloor is where BOTH limit actions still fit alongside the four hints
	// that are never dropped. It is arithmetic, not taste:
	//
	//   n new(5) + D kill(6) + c retry limit(13) + F hand off(10)
	//     + ? help(6) + q quit(6) + six separators(3 each) = 61
	//
	// Below it something in that set must go, and #1083 already settled which:
	// help and quit are the escape hatches, so the limit actions shed instead.
	// Pinned so a copy change that widens either fragment fails HERE, naming the
	// row it broke, rather than silently raising the width at which a stuck user
	// can see their way out.
	const limitRowFloor = 61

	m.SetSize(limitRowFloor, 1)
	atFloor := m.String()
	assert.Contains(t, atFloor, "retry limit", "at the floor the wait action must be advertised")
	assert.Contains(t, atFloor, "hand off", "at the floor the switch action must be advertised alongside it")
	assert.LessOrEqual(t, lipgloss.Width(atFloor), limitRowFloor)

	// Above the floor both survive; below it, the primary action outlasts its
	// alternative rather than both vanishing together.
	m.SetSize(limitRowFloor-1, 1)
	belowFloor := m.String()
	assert.Contains(t, belowFloor, "retry limit",
		"just below the floor the PRIMARY limit action must outlast the alternative")

	// Roomy: both are advertised together — they are the two answers to the same
	// wall, and a user who sees only one cannot weigh them.
	m.SetSize(200, 1)
	full := m.String()
	assert.Contains(t, full, "retry limit")
	assert.Contains(t, full, "hand off")
}

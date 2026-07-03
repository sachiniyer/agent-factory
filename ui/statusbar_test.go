package ui

import (
	"errors"
	"testing"

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
// hints to vanish on narrow terminals.
func TestMenuNarrowWidthKeepsHelpAndQuit(t *testing.T) {
	m := NewMenu()
	m.SetInstance(&session.Instance{Status: session.Ready})

	for _, w := range []int{110, 80, 60, 45} {
		m.SetSize(w, 1)
		out := m.String()
		assert.LessOrEqualf(t, lipgloss.Width(out), w,
			"width %d: the prioritized row must fit the bar", w)
		assert.Containsf(t, out, "q quit", "width %d: quit must survive", w)
		assert.Containsf(t, out, "? help", "width %d: help must survive", w)
	}

	// At a roomy width nothing is dropped: the scroll hints still render.
	m.SetSize(200, 1)
	assert.Contains(t, m.String(), "scroll", "no drops at full width")
	assert.Contains(t, m.String(), "q quit")
}

package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// fakeLiveView stands in for a termpane so the window's live render/resize
// plumbing (#1089 PR 1) is testable without a PTY.
type fakeLiveView struct {
	content string
	resizes [][2]int
	// cursorRenders records the showCursor flag of each Render call — the
	// window must ask for the cursor exactly while interactive (#1089 PR 2).
	cursorRenders []bool
}

func (f *fakeLiveView) Render(width, height int, showCursor bool) string {
	f.cursorRenders = append(f.cursorRenders, showCursor)
	return f.content
}
func (f *fakeLiveView) Resize(width, height int) {
	f.resizes = append(f.resizes, [2]int{width, height})
}

func TestTabbedWindowRendersLiveViewWhenBound(t *testing.T) {
	w := NewTabbedWindow(NewTabPane(), nil)
	w.SetRect(layout.Rect{W: 40, H: 12})

	fake := &fakeLiveView{content: "LIVE-EMBEDDED-TERMINAL"}
	w.SetLive(fake)
	require.True(t, w.HasLive())

	// Binding sizes the live view to the content area: rect minus the frame
	// (2) and the header row (1).
	require.Len(t, fake.resizes, 1)
	assert.Equal(t, [2]int{38, 9}, fake.resizes[0])

	assert.Contains(t, w.String(), "LIVE-EMBEDDED-TERMINAL", "bound window renders the live grid")

	w.SetLive(nil)
	require.False(t, w.HasLive())
	assert.NotContains(t, w.String(), "LIVE-EMBEDDED-TERMINAL", "unbound window falls back to the capture view")
}

func TestTabbedWindowResizesLiveViewWithRect(t *testing.T) {
	w := NewTabbedWindow(NewTabPane(), nil)
	w.SetRect(layout.Rect{W: 40, H: 12})
	fake := &fakeLiveView{content: "LIVE"}
	w.SetLive(fake)
	require.Len(t, fake.resizes, 1)

	w.SetRect(layout.Rect{W: 60, H: 20})
	require.Len(t, fake.resizes, 2)
	assert.Equal(t, [2]int{58, 17}, fake.resizes[1])

	// A zero rect (auto-hidden pane, §2.6) must NOT push a degenerate
	// winsize at the live attachment — the root model closes it instead.
	w.SetRect(layout.Rect{})
	assert.Len(t, fake.resizes, 2, "zero rect must not resize the live view")
}

func TestTabbedWindowInteractiveCue(t *testing.T) {
	w := NewTabbedWindow(NewTabPane(), nil)
	w.SetRect(layout.Rect{W: 40, H: 12})
	fake := &fakeLiveView{content: "LIVE"}
	w.SetLive(fake)
	w.Focus()

	// Nav mode: focused teal frame, no cursor requested.
	navFrame := w.String()
	require.NotEmpty(t, fake.cursorRenders)
	assert.False(t, fake.cursorRenders[len(fake.cursorRenders)-1],
		"nav-mode render must not request the cursor")

	// Interactive: the frame changes (the green keyboard-owner cue) and the
	// live render is asked for the cursor overlay.
	w.SetInteractive(true)
	require.True(t, w.Interactive())
	interactiveFrame := w.String()
	assert.True(t, fake.cursorRenders[len(fake.cursorRenders)-1],
		"interactive render must request the cursor")
	assert.NotEqual(t, navFrame, interactiveFrame,
		"interactive mode needs a visible cue distinct from nav focus")

	w.SetInteractive(false)
	assert.Equal(t, navFrame, w.String(), "leaving interactive restores the nav frame")
}

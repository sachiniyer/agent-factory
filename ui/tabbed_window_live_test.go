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
}

func (f *fakeLiveView) Render(width, height int) string { return f.content }
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

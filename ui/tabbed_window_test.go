package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
)

// setWindowSize rects the pane at origin, the test shorthand for the layout
// engine's SetRect call.
func setWindowSize(w *TabbedWindow, width, height int) {
	w.SetRect(layout.Rect{W: width, H: height})
}

// TestTabbedWindowSetRectClampsNegativeDimensions verifies that SetRect never
// propagates negative content dimensions down to the tab pane. Without
// clamping, tiny terminal windows produce negative ints that later overflow
// to huge uint16 values inside pty.Setsize, corrupting the tmux PTY size. See
// issue #276.
func TestTabbedWindowSetRectClampsNegativeDimensions(t *testing.T) {
	cases := []struct {
		name   string
		width  int
		height int
	}{
		{"zero size", 0, 0},
		{"tiny height", 10, 1},
		{"height just below threshold", 10, 5},
		{"negative inputs", -10, -10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestTabbedWindow()
			setWindowSize(w, tc.width, tc.height)

			previewW, previewH := w.GetPreviewSize()
			assert.GreaterOrEqual(t, previewW, 0, "preview width should be clamped to >= 0")
			assert.GreaterOrEqual(t, previewH, 0, "preview height should be clamped to >= 0")
			assert.GreaterOrEqual(t, w.tab.width, 0, "tab width should be clamped to >= 0")
			assert.GreaterOrEqual(t, w.tab.height, 0, "tab height should be clamped to >= 0")
		})
	}
}

// TestTabbedWindowSetRectNormal sanity-checks that reasonable sizes still
// produce positive content dimensions.
func TestTabbedWindowSetRectNormal(t *testing.T) {
	w := newTestTabbedWindow()
	setWindowSize(w, 200, 100)

	previewW, previewH := w.GetPreviewSize()
	assert.Greater(t, previewW, 0)
	assert.Greater(t, previewH, 0)
}

// TestTabbedWindowViewIsExactlyRectSized enforces the layout.Pane contract
// (#1024 PR 4): View() is exactly Rect-sized — every line exactly rect.W
// printable cells, exactly rect.H lines — so the root model can tile the
// regions with no clipping math.
func TestTabbedWindowViewIsExactlyRectSized(t *testing.T) {
	w := newTestTabbedWindow()
	setWindowSize(w, 100, 30)
	w.tab.content = tabContentState{text: "content"}

	rendered := w.View()
	lines := strings.Split(rendered, "\n")
	assert.Len(t, lines, 30, "View must render exactly rect.H lines")
	for i, line := range lines {
		assert.Equalf(t, 100, lipgloss.Width(line), "line %d must be exactly rect.W cells", i)
	}
}

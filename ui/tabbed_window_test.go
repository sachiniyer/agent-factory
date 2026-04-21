package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTabbedWindowSetSizeClampsNegativeDimensions verifies that SetSize never
// propagates negative content dimensions down to the preview/terminal panes.
// Without clamping, tiny terminal windows (height <= 5) produce negative ints
// that later overflow to huge uint16 values inside pty.Setsize, corrupting the
// tmux PTY size. See issue #276.
func TestTabbedWindowSetSizeClampsNegativeDimensions(t *testing.T) {
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
			w := NewTabbedWindow(NewPreviewPane(), NewTerminalPane())
			w.SetSize(tc.width, tc.height)

			previewW, previewH := w.GetPreviewSize()
			assert.GreaterOrEqual(t, previewW, 0, "preview width should be clamped to >= 0")
			assert.GreaterOrEqual(t, previewH, 0, "preview height should be clamped to >= 0")
			assert.GreaterOrEqual(t, w.terminal.width, 0, "terminal width should be clamped to >= 0")
			assert.GreaterOrEqual(t, w.terminal.height, 0, "terminal height should be clamped to >= 0")
		})
	}
}

// TestTabbedWindowSetSizeNormal sanity-checks that reasonable sizes still
// produce positive content dimensions.
func TestTabbedWindowSetSizeNormal(t *testing.T) {
	w := NewTabbedWindow(NewPreviewPane(), NewTerminalPane())
	w.SetSize(200, 100)

	previewW, previewH := w.GetPreviewSize()
	assert.Greater(t, previewW, 0)
	assert.Greater(t, previewH, 0)
}

package layout_test

import (
	"testing"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
)

func TestFitContentRectClampsFrameInsideOuterBounds(t *testing.T) {
	got := layout.FitContentRect(
		layout.Rect{W: 60, H: 20},
		layout.Rect{W: 40, H: 10},
		2,
		2,
	)

	assert.Equal(t, layout.Rect{W: 38, H: 8}, got)
}

func TestFitContentRectUsesAvailableForAutoDimensions(t *testing.T) {
	got := layout.FitContentRect(
		layout.Rect{W: 50},
		layout.Rect{W: 60, H: 15},
		2,
		2,
	)

	assert.Equal(t, layout.Rect{W: 50, H: 13}, got)
}

func TestFitContentRectKeepsUnboundedPreferredDimensions(t *testing.T) {
	got := layout.FitContentRect(
		layout.Rect{W: 50, H: 8},
		layout.Rect{},
		2,
		2,
	)

	assert.Equal(t, layout.Rect{W: 50, H: 8}, got)
}

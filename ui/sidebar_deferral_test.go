package ui

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSidebarDeferredSuffixAtExactViewportBoundary(t *testing.T) {
	const avail = 18
	// A measured prefix exactly fills the viewport. The unrendered suffix
	// still occupies at least one line per item and must reserve an indicator;
	// treating those deferred rows as zero height would incorrectly fit all.
	heights := make([]int, 200)
	for i := range heights {
		heights[i] = 1
	}
	end, above, below := fitWindow(heights, 0, avail)
	require.Equal(t, avail-1, end)
	require.False(t, above)
	require.True(t, below)

	// Exercise the rendering path that defers those off-screen rows as well.
	s := newWindowingSidebar(t, len(heights))
	s.SetSize(40, avail+chromeLines)
	view := s.String()
	require.Equal(t, avail+chromeLines, renderedLineCount(view))
	above, below = indicatorArrows(view)
	require.False(t, above)
	require.True(t, below)
}

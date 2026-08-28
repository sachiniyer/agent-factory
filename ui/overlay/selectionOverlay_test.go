package overlay

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestSelectionOverlayStaysWithinMaxHeight pins #3344: scroll indicators must
// be budgeted inside the frame because PlaceOverlay refuses to composite an
// oversized foreground. Sweep list sizes, cursor positions, and compact/full
// layouts at heights where the fixed chrome fits.
func TestSelectionOverlayStaysWithinMaxHeight(t *testing.T) {
	for _, maxH := range []int{7, 8, 10, 14, 20} {
		for n := 1; n <= 18; n++ {
			items := make([]string, n)
			for i := range items {
				items[i] = fmt.Sprintf("item-%02d", i)
			}

			s := NewSelectionOverlay("Choose an item", items)
			s.SetMaxSize(60, maxH)
			for selected := len(items) - 1; selected >= 0; selected-- {
				s.SetSelectedIndex(selected)
				if got := lipgloss.Height(s.Render()); got > maxH {
					t.Fatalf("selection overlay rendered %d rows with maxHeight=%d (n=%d selected=%d)", got, maxH, n, selected)
				}
			}
		}
	}
}

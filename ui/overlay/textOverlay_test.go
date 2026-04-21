package overlay

import (
	"strings"
	"testing"

	"github.com/muesli/ansi"
	"github.com/stretchr/testify/assert"
)

// wideContent produces content whose natural render width exceeds a typical
// narrow terminal (80 columns), mirroring real help-overlay contents.
const wideContent = "This is the general help overlay content. It spans well beyond eighty columns so that without a default width constraint lipgloss will render the overlay at its natural width and PlaceOverlay will skip centering and the fade effect."

// maxLineWidth returns the widest printable line in s.
func maxLineWidth(s string) int {
	widest := 0
	for _, line := range strings.Split(s, "\n") {
		if w := ansi.PrintableRuneWidth(line); w > widest {
			widest = w
		}
	}
	return widest
}

// TestNewTextOverlayHasDefaultWidth verifies that NewTextOverlay initializes
// the overlay with a sensible default width so PlaceOverlay can center and
// fade correctly on narrow terminals (regression for #273).
func TestNewTextOverlayHasDefaultWidth(t *testing.T) {
	const terminalWidth = 80

	overlay := NewTextOverlay(wideContent)
	rendered := overlay.Render()

	width := maxLineWidth(rendered)
	// With the fix, the rendered overlay should be narrower than an 80-column
	// terminal, allowing PlaceOverlay to apply centering/fade.
	assert.Less(t, width, terminalWidth,
		"expected default-width overlay (%d cols) to be narrower than terminal (%d cols)", width, terminalWidth)
}

// TestTextOverlaySetWidthOverridesDefault ensures SetWidth still controls the
// rendered overlay width after the default is applied.
func TestTextOverlaySetWidthOverridesDefault(t *testing.T) {
	overlay := NewTextOverlay(wideContent)
	overlay.SetWidth(40)

	rendered := overlay.Render()
	width := maxLineWidth(rendered)

	// width=40 plus border+padding should produce a ~44 column rendering.
	assert.LessOrEqual(t, width, 50)
}

// TestTextOverlayAllowsPlaceOverlayCentering verifies the end-to-end behavior:
// PlaceOverlay should actually apply centering/fade rather than early-returning
// the foreground when a default-width TextOverlay is placed on a narrow bg.
func TestTextOverlayAllowsPlaceOverlayCentering(t *testing.T) {
	overlay := NewTextOverlay(wideContent)
	fg := overlay.Render()

	// Build an 80x24 background of spaces.
	bgLine := strings.Repeat(" ", 80)
	bgLines := make([]string, 24)
	for i := range bgLines {
		bgLines[i] = bgLine
	}
	bg := strings.Join(bgLines, "\n")

	out := PlaceOverlay(0, 0, fg, bg, true)

	// If PlaceOverlay early-returned (because fg was wider than bg), out would
	// equal fg. With the fix, the output should instead be the composited
	// background with the overlay placed on top.
	assert.NotEqual(t, fg, out, "PlaceOverlay should not early-return when overlay has default width")
	assert.Equal(t, len(bgLines), strings.Count(out, "\n")+1,
		"output should retain background height, confirming overlay was composited")
}

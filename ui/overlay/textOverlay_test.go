package overlay

import (
	"fmt"
	"strings"
	"testing"

	"github.com/muesli/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestTextOverlayHeightWindowsContent verifies that a tall text overlay renders
// within its configured outer height instead of relying on PlaceOverlay to
// handle an oversized foreground.
func TestTextOverlayHeightWindowsContent(t *testing.T) {
	overlay := NewTextOverlay(strings.Join([]string{
		"title",
		"line 1",
		"line 2",
		"line 3",
		"line 4",
		"line 5",
		"line 6",
		"line 7",
	}, "\n"))
	overlay.SetWidth(30)
	overlay.SetHeight(6)

	rendered := overlay.Render()
	assert.Equal(t, 6, strings.Count(rendered, "\n")+1,
		"rendered overlay should fit the requested outer height")
	assert.Contains(t, rendered, "title", "initial viewport starts at the top")
	assert.Contains(t, rendered, "↓ more", "overflow below is visible")
}

func TestTextOverlayScrollsContent(t *testing.T) {
	overlay := NewTextOverlay(strings.Join([]string{
		"title",
		"line 1",
		"line 2",
		"line 3",
		"line 4",
		"line 5",
		"line 6",
		"line 7",
	}, "\n"))
	overlay.SetWidth(30)
	overlay.SetHeight(6)

	overlay.ScrollDown()
	rendered := overlay.Render()
	assert.NotContains(t, rendered, "title", "scrolling down moves the viewport")
	assert.Contains(t, rendered, "↑ more", "overflow above is visible")

	overlay.ScrollUp()
	rendered = overlay.Render()
	assert.Contains(t, rendered, "title", "scrolling up returns toward the top")
}

// visibleColumnOf returns the printable column at which sub first appears in an
// ANSI-styled line, or -1 if absent. Used to check where the box frame lands
// after PlaceOverlay composites it, independent of the styling on the line.
func visibleColumnOf(line, sub string) int {
	idx := strings.Index(line, sub)
	if idx < 0 {
		return -1
	}
	return ansi.PrintableRuneWidth(line[:idx])
}

// TestTextOverlayStaysFramedWhenLinesSoftWrapPastWidth is the #1998 regression
// guard at the component level. At 80x24 the general help wraps to lines one
// cell past the box's text width; a *soft* wrapper (wordwrap) leaves them there
// and lipgloss then re-wraps each into two rows, so the box grew one row per
// such visible line, overflowed the terminal, and PlaceOverlay fell back to
// dumping the raw frame at column 0 with its top border clipped. The wrap must
// match lipgloss's own (ansi.Wrap) so one logical line is exactly one rendered
// row; then the box height and centering hold at every scroll offset — even
// scrolled past content-end.
func TestTextOverlayStaysFramedWhenLinesSoftWrapPastWidth(t *testing.T) {
	// Interleave the real help line that soft-wraps to textWidth+1 (44→45 at the
	// 80x24 geometry) with filler so the overlay is tall enough to scroll.
	var b strings.Builder
	for i := 0; i < 30; i++ {
		if i%3 == 0 {
			b.WriteString("c              - Retry a session blocked at a usage\n")
		} else {
			b.WriteString(fmt.Sprintf("line %d filler text for the help body\n", i))
		}
	}
	ov := NewTextOverlay(strings.TrimRight(b.String(), "\n"))
	// 80x24 help geometry: width = int(0.6*80) = 48, height = 24 - 2 = 22.
	const (
		termWidth, termHeight = 80, 24
		boxHeight             = termHeight - 2 // outer height layoutTextOverlay sets
	)
	ov.SetWidth(48)
	ov.SetHeight(boxHeight)

	bgLine := strings.Repeat(" ", termWidth)
	bgLines := make([]string, termHeight)
	for i := range bgLines {
		bgLines[i] = bgLine
	}
	bg := strings.Join(bgLines, "\n")

	// Drive the scroll offset from the top well past content-end.
	for step := 0; step <= 40; step++ {
		fg := ov.Render()
		require.Equal(t, boxHeight, strings.Count(fg, "\n")+1,
			"step %d: box must stay within its %d-row height budget", step, boxHeight)

		placed := PlaceOverlay(0, 0, fg, bg, true)
		lines := strings.Split(placed, "\n")
		require.Len(t, lines, termHeight,
			"step %d: composited view must stay exactly terminal height", step)

		// The top border must be present and horizontally centered — never
		// flush at column 0 (the raw-fg fallback signature).
		topRow, topCol := -1, -1
		for i, l := range lines {
			if c := visibleColumnOf(l, "╭"); c >= 0 {
				topRow, topCol = i, c
				break
			}
		}
		require.GreaterOrEqual(t, topRow, 0, "step %d: top border ╭ must be visible", step)
		expectedCol := (termWidth - ansi.PrintableRuneWidth(strings.Split(fg, "\n")[0])) / 2
		require.Greater(t, expectedCol, 0, "sanity: centered box must have a left margin")
		require.Equal(t, expectedCol, topCol,
			"step %d: box must stay centered, not collapse toward column 0", step)

		ov.ScrollDown()
	}
}

func TestTextOverlayHeightWindowsWrappedContent(t *testing.T) {
	overlay := NewTextOverlay(strings.Join([]string{
		"title",
		"this line is intentionally long enough to wrap into several visual rows inside the overlay",
		"tail",
	}, "\n"))
	overlay.SetWidth(24)
	overlay.SetHeight(8)

	rendered := overlay.Render()
	assert.Equal(t, 8, strings.Count(rendered, "\n")+1,
		"wrapped content should still fit the requested outer height")
	assert.Contains(t, rendered, "title")
	assert.Contains(t, rendered, "↓ more")

	overlay.ScrollDown()
	rendered = overlay.Render()
	assert.Equal(t, 8, strings.Count(rendered, "\n")+1,
		"scrolled wrapped content should still fit the requested outer height")
	assert.Contains(t, rendered, "↑ more")
}

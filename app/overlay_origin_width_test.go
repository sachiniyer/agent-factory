package app

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// clusteredFamily is four emoji joined by three ZWJs — the content the width
// functions in this tree disagree about, and the reason #3585 exists.
const clusteredFamily = "\U0001F468‍\U0001F469‍\U0001F467‍\U0001F466"

// modalOrigin reports the row and CELL COLUMN where the modal actually landed in
// a composited frame. The column is measured, not taken from strings.Index, which
// returns a byte offset — for this content the two differ by a factor of six.
func modalOrigin(t *testing.T, out, marker string) (row, col int) {
	t.Helper()
	for i, line := range strings.Split(out, "\n") {
		if at := strings.Index(line, marker); at >= 0 {
			return i, layout.Cells(line[:at])
		}
	}
	t.Fatalf("the modal never appears in the composite:\n%s", out)
	return -1, -1
}

// #3585. overlayOrigin decides BOTH where a modal is drawn and where its mouse
// zones are registered — placeOverlay renders at it, and the confirmation, search
// and selection overlays all call RegisterZones with it. It therefore has to agree
// with where PlaceOverlay actually puts the modal.
//
// It did not. overlayOrigin measured with lipgloss.Width while the compositor
// clamped with ansi.PrintableRuneWidth, so for clustered text the origin named a
// column the modal was never drawn at: measured at (col 29, row 10) against a
// modal rendered at column 0, leaving every registered button offset from the
// thing it was registered for.
//
// Both sides now measure with layout.Cells, so this holds by construction rather
// than by the two happening to agree.
func TestOverlayOriginAgreesWithTheCompositor(t *testing.T) {
	for _, tc := range []struct {
		name string
		fg   string
	}{
		{"clustered text", strings.Repeat(clusteredFamily, 11)},
		{"plain text", strings.Repeat("m", 20)},
		{"wide CJK", strings.Repeat("界", 12)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const cols, rows = 80, 24
			bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")
			fg := strings.Join([]string{tc.fg, tc.fg, tc.fg}, "\n")

			origin := overlayOrigin(fg, bg)
			out := overlay.PlaceOverlay(origin.X, origin.Y, fg, bg, false)

			marker := string([]rune(tc.fg)[0])
			gotRow, gotCol := modalOrigin(t, out, marker)
			if gotCol != origin.X {
				t.Errorf("modal drawn at column %d but its zones are registered at %d: "+
					"every button in it would be offset by %d cells", gotCol, origin.X, origin.X-gotCol)
			}
			if gotRow != origin.Y {
				t.Errorf("modal drawn on row %d but its zones are registered at %d", gotRow, origin.Y)
			}
			// #3578's guarantee, still holding: the frame survives underneath.
			if len(strings.Split(out, "\n")) != rows {
				t.Errorf("the frame must survive compositing: got %d rows, want %d",
					len(strings.Split(out, "\n")), rows)
			}
		})
	}
}

// The origin must also stay inside the frame for a modal the compositor will clip.
// A negative or past-the-edge origin would register zones off-screen entirely.
func TestOverlayOriginStaysInsideTheFrame(t *testing.T) {
	const cols, rows = 40, 10
	bg := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", cols)+"\n", rows), "\n")
	oversized := strings.Repeat(clusteredFamily, 30)
	fg := strings.TrimSuffix(strings.Repeat(oversized+"\n", rows+5), "\n")

	origin := overlayOrigin(fg, bg)
	if origin.X < 0 || origin.X >= cols {
		t.Errorf("origin.X = %d, must be inside [0,%d)", origin.X, cols)
	}
	if origin.Y < 0 || origin.Y >= rows {
		t.Errorf("origin.Y = %d, must be inside [0,%d)", origin.Y, rows)
	}
}

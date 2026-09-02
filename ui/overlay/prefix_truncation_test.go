package overlay

import (
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// family is the four-emoji, three-ZWJ cluster the width functions disagree about:
// x/ansi (and therefore layout.Cells) says 2, runewidth counts it rune by rune at
// 8, and tmux 3.4 advances 4.
const family = "\U0001F468‍\U0001F469‍\U0001F467‍\U0001F466"

// #723, with the A/B from the reopening comment as the fixture.
//
//	bg = "A" + <ZWJ family> + 18*"b", PlaceOverlay(10, 0, "XX", bg, false)
//
//	pre-#3610   "A<family>bXX      bbbbbbbb"   overlay at visual column 4, not 10
//	post-#3610  "A<family>b      XXbbbbbbbb"   column right; 6 background cells blanked
//
// Both are the same defect: the prefix was cut with truncate.String, which
// budgets rune by rune with runewidth, while `pos` was read with the
// grapheme-aware measure everything else uses. The prefix consumed 10 cells by
// the first and 4 by the second, so the pad filled a gap that was not there and
// TruncateLeft skipped the background the pad was standing in for.
func TestPlaceOverlayPrefixDoesNotEraseBackground(t *testing.T) {
	const tailLen = 18
	bg := "A" + family + strings.Repeat("b", tailLen)
	if got, want := layout.Cells(bg), 1+2+tailLen; got != want {
		t.Fatalf("precondition: bg is %d cells, want %d", got, want)
	}

	const placeX, fg = 10, "XX"
	out := PlaceOverlay(placeX, 0, fg, bg, false)

	// The overlay covers exactly its own width; every other background cell is
	// still on screen. Six of them were blanked before the fix.
	if got, want := strings.Count(out, "b"), tailLen-layout.Cells(fg); got != want {
		t.Errorf("%d background cells survived, want %d — the prefix erased %d of them: %q",
			got, want, want-got, xansi.Strip(out))
	}
	// And the overlay is where it was asked to be, which is where its mouse zones
	// were registered (#3585).
	assertOverlayColumn(t, out, fg, placeX)
}

// The straddle case, and the decision recorded on #723: a grapheme cluster that
// would span placeX is NEVER split — the prefix is cut before it and padded with
// blanks up to placeX, so the overlay's column still equals the registered zone
// origin. Placing it a cell early or late instead would reopen exactly the zone
// mismatch #3585 closed.
func TestPlaceOverlayNeverSplitsAClusterAtTheEdge(t *testing.T) {
	for _, c := range []struct {
		name    string
		bg      string
		placeX  int
		cluster string
	}{
		{"zwj family straddles", "a" + family + strings.Repeat("b", 12), 2, family},
		{"skin tone straddles", "a\U0001F44D\U0001F3FD" + strings.Repeat("b", 12), 2, "\U0001F44D\U0001F3FD"},
		{"vs16 straddles", "a❤️" + strings.Repeat("b", 12), 2, "❤️"},
		{"cjk straddles", "ab你" + strings.Repeat("c", 12), 3, "你"},
		{"cluster ends exactly at placeX", "a" + family + strings.Repeat("b", 12), 3, family},
	} {
		const fg = "XX"
		out := PlaceOverlay(c.placeX, 0, fg, c.bg, false)
		assertOverlayColumn(t, out, fg, c.placeX)

		// Whatever happened to the cluster, it is present whole or absent whole —
		// never half of it. The modifier/joiner runes are what a rune-wise cut
		// leaves orphaned, so they are what to look for.
		prefix := out[:strings.Index(out, fg)]
		for _, orphan := range []string{"‍", "️", "\U0001F3FD"} {
			if strings.Contains(prefix, orphan) && !strings.Contains(prefix, c.cluster) {
				t.Errorf("%s: the prefix kept an orphaned %q without its cluster: %q",
					c.name, orphan, xansi.Strip(prefix))
			}
		}
		if strings.Contains(prefix, c.cluster) && layout.Cells(prefix) > c.placeX {
			t.Errorf("%s: the straddling cluster was kept and pushed the overlay past column %d: %q",
				c.name, c.placeX, xansi.Strip(prefix))
		}
	}
}

// A prefix with no clustered content must compose byte-identically to before, so
// switching truncators cannot move an ordinary modal.
func TestPlaceOverlayPrefixUnchangedForPlainBackground(t *testing.T) {
	bg := strings.Repeat("abcdefghij", 4)
	// placeX stays within bgWidth-fgWidth; past that PlaceOverlay clamps it.
	for _, placeX := range []int{0, 1, 5, 10, 21, 35} {
		out := PlaceOverlay(placeX, 0, "MODAL", bg, false)
		want := bg[:placeX] + "MODAL" + bg[placeX+5:]
		if got := xansi.Strip(out); got != want {
			t.Errorf("placeX=%d: got %q, want %q", placeX, got, want)
		}
	}
}

// assertOverlayColumn checks the foreground starts at exactly placeX cells in —
// the property the mouse zones are registered against (#3585).
func assertOverlayColumn(t *testing.T, out, fg string, placeX int) {
	t.Helper()
	line := strings.SplitN(out, "\n", 2)[0]
	i := strings.Index(line, fg)
	if i < 0 {
		t.Fatalf("the overlay is not in the composed row: %q", xansi.Strip(line))
	}
	if got := layout.Cells(line[:i]); got != placeX {
		t.Errorf("the overlay is drawn at column %d, was placed at %d — its mouse "+
			"zones are registered at %d (#3585): %q", got, placeX, placeX, xansi.Strip(line))
	}
}

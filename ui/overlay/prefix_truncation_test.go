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

// This file's tests once pinned the prefix: the slice of the BACKGROUND the
// compositor kept to the left of a modal, cut at placeX by a cell-accurate
// truncator. #5297 retired that prefix entirely — on a row the modal occupies,
// the cells either side of it are generated blank backdrop, because any cut of
// real background content renders as chopped fragments beside the box (the
// ~14-column strip of the issue report, and the rail glyphs clipped mid-cell).
//
// What survives of the original contract, and what the tests below pin, is the
// placement half: the modal lands at exactly placeX regardless of what the
// background carried — the column its mouse zones were registered against
// (#3585) — and the blank margin can never strand a grapheme cluster, a style,
// or a hyperlink the way a truncated prefix could.

func TestPlaceOverlayBlankMarginsCarryNoBackgroundCells(t *testing.T) {
	const tailLen = 18
	bg := "A" + family + strings.Repeat("b", tailLen)
	if got, want := layout.Cells(bg), 1+2+tailLen; got != want {
		t.Fatalf("precondition: bg is %d cells, want %d", got, want)
	}

	const placeX, fg = 10, "XX"
	out := PlaceOverlay(placeX, 0, fg, bg, false)

	// Every cell outside the modal on its row is blank: none of the
	// background's glyphs survive there, whole or clipped.
	for _, ch := range []string{"A", "b", family} {
		if strings.Contains(out, ch) {
			t.Errorf("background content %q survived beside the modal — that is "+
				"the chopped-fragment strip #5297 is about: %q", ch, xansi.Strip(out))
		}
	}
	// And the overlay is where it was asked to be, which is where its mouse zones
	// were registered (#3585).
	assertOverlayColumn(t, out, fg, placeX)
}

// The straddle cases below used to exercise prefix cutting (#723): a grapheme
// cluster spanning placeX could be orphaned or push the modal off its column.
// The margin is blank backdrop now, so neither can happen — a straddling
// cluster is blanked wholesale, never split. What stays pinned is that the
// modal still lands at exactly placeX across all these background shapes.
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

		// Whatever the cluster was, it is absent whole — never half of it. The
		// modifier/joiner runes are what a rune-wise cut leaves orphaned, so they
		// are what to look for.
		prefix := out[:strings.Index(out, fg)]
		for _, orphan := range []string{"‍", "️", "\U0001F3FD"} {
			if strings.Contains(prefix, orphan) && !strings.Contains(prefix, c.cluster) {
				t.Errorf("%s: the margin kept an orphaned %q without its cluster: %q",
					c.name, orphan, xansi.Strip(prefix))
			}
		}
		if strings.Contains(prefix, c.cluster) && layout.Cells(prefix) > c.placeX {
			t.Errorf("%s: the straddling cluster pushed the overlay past column %d: %q",
				c.name, c.placeX, xansi.Strip(prefix))
		}
	}
}

// A plain background yields a margin of plain blanks at every placeX, so the
// modal lands at its column no matter where it sits.
func TestPlaceOverlayPlainBackgroundMarginsAreBlank(t *testing.T) {
	bg := strings.Repeat("abcdefghij", 4)
	// placeX stays within bgWidth-fgWidth; past that PlaceOverlay clamps it.
	for _, placeX := range []int{0, 1, 5, 10, 21, 35} {
		out := PlaceOverlay(placeX, 0, "MODAL", bg, false)
		want := strings.Repeat(" ", placeX) + "MODAL" + strings.Repeat(" ", layout.Cells(bg)-placeX-5)
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

// Codex P2 on #3657 originally guarded a prefix cut mid-SGR: a truncated
// background could carry an open style or hyperlink into the modal's cells.
// #5297 made that unreachable — the margin is generated blank backdrop, never
// a cut of styled background. The check still earns its keep as a guard that
// margin EMISSION itself writes nothing unclosed before the modal, and that
// the modal stays on its registered column (#3585).
func TestPlaceOverlayBlankMarginCarriesNoOpenStyling(t *testing.T) {
	for _, c := range []struct {
		name string
		bg   string
		x    int
	}{
		{"sgr opened just past the cut", "AAAA\x1b[31mBBBB", 4},
		{"cut inside a styled run", "\x1b[32mAAAA\x1b[31mBBBB", 2},
		{"already closed", "AAAA\x1b[31mBBBB\x1b[0m", 4},
		{"hyperlink past the cut", "AAAA\x1b]8;;https://example.com\x1b\\BBBB\x1b]8;;\x1b\\", 4},
	} {
		out := PlaceOverlay(c.x, 0, "XX", c.bg, false)
		i := strings.Index(out, "XX")
		if i < 0 {
			t.Fatalf("%s: the overlay is not in the composed row: %q", c.name, out)
		}
		prefix := out[:i]
		// Whatever the margin emitted, it must be closed before the overlay starts.
		if sgr := sgrRegex.FindAllString(prefix, -1); len(sgr) > 0 {
			if last := sgr[len(sgr)-1]; last != "\x1b[0m" && last != "\x1b[m" {
				t.Errorf("%s: the overlay inherits the margin's style %q: %q",
					c.name, last, out)
			}
		}
		if n := strings.Count(prefix, "\x1b]8;;"); n%2 != 0 {
			t.Errorf("%s: the margin left a hyperlink open across the overlay: %q", c.name, out)
		}
		// And the fix must not have moved the overlay.
		assertOverlayColumn(t, out, "XX", c.x)
	}
}

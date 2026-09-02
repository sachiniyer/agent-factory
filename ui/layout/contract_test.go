package layout

import (
	"strings"
	"testing"
)

// tmuxAdvance is what tmux 3.4 was measured advancing for the strings this file
// uses, on an isolated socket (see Cells' table). No library function in the tree
// reports it, which is the whole of #3614 — so the assertions below are written
// against the BOUND, which is the thing the contract can actually promise.
const tmuxAdvanceFamily = 4

// #3614. The row from the issue: "ab" + <ZWJ family> + "cd" into a 10-cell
// rectangle. Every measure in the tree reports 6, so ClampToRect padded 4 blanks
// onto a row that tmux draws 12 cells wide and called it 10 — an overflow that
// the width branch never sees, because the row does not look too wide going in.
//
// The contract is NEVER OVERFLOW, so the assertion is on the upper bound: the row
// that leaves the clamp must not be able to exceed the rectangle, whatever tmux
// does with the cluster. Under-filling is the accepted cost and is asserted as
// such rather than smoothed over.
func TestClampToRectCannotOverflowForClusteredContent(t *testing.T) {
	row := "ab" + joinedFamily + "cd"
	if got := Cells(row); got != 6 {
		t.Fatalf("precondition: every measure in the tree reports this row as 6, got %d", got)
	}

	const w = 10
	out := ClampToRect(row, Rect{W: w, H: 1})
	if got := CellsUpperBound(out); got > w {
		t.Errorf("clamped row can occupy %d cells, over the %d-cell rectangle: %q",
			got, w, out)
	}
	// And the true tmux width, which is 4 for the family rather than the bound's 8.
	trueWidth := Cells(out) - Cells(joinedFamily) + tmuxAdvanceFamily
	if strings.Contains(out, joinedFamily) && trueWidth > w {
		t.Errorf("row draws %d cells in tmux, over the %d-cell rectangle: %q", trueWidth, w, out)
	}
	if trueWidth == w {
		t.Log("note: this row happens to fill the rectangle exactly; the contract only promises it cannot exceed it")
	}
}

// The other half of the same promise: rows the measures AGREE on are untouched.
// The upper bound is a deliberate overestimate, so letting it near ordinary
// content is how #3610 cut the ● status dot off every clustered sidebar row. This
// pins that it stays in its branch.
func TestClampToRectIsUnchangedForOrdinaryRows(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		r    Rect
		want string
	}{
		{"ascii pads", "hi", Rect{W: 6, H: 1}, "hi    "},
		{"ascii truncates", "abcdefgh", Rect{W: 4, H: 1}, "abcd"},
		{"cjk truncates whole runes", "日本語", Rect{W: 5, H: 1}, "日本 "},
		{"emoji without a joiner", "\U0001F600ab", Rect{W: 6, H: 1}, "\U0001F600ab  "},
		{"box drawing", "╭──╮", Rect{W: 6, H: 1}, "╭──╮  "},
	} {
		if got := ClampToRect(c.in, c.r); got != c.want {
			t.Errorf("%s: ClampToRect(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		if got, want := Cells(ClampToRect(c.in, c.r)), c.r.W; got != want {
			t.Errorf("%s: an ordinary row must still be EXACTLY %d cells, got %d", c.name, want, got)
		}
	}
}

// Every clamped row, ordinary or not, is bounded by the rectangle. Sweeps the
// disagreement corpus so a new entry cannot land without going through here.
func TestClampToRectNeverExceedsTheRectangle(t *testing.T) {
	for _, s := range []string{
		"plain",
		joinedFamily,
		"a" + joinedFamily,
		"\U0001F469‍❤️‍\U0001F468", // ZWJ couple, with a VS16 inside
		"\U0001F44D\U0001F3FD",     // skin-tone modifier
		"❤️",                       // heart + VS16
		"1️⃣",                      // keycap
		"fam" + joinedFamily + "zzz",
		"\x1b[31m" + joinedFamily + "\x1b[0m",
		"\x1b]8;;https://example.com\x1b\\" + joinedFamily + "\x1b]8;;\x1b\\",
	} {
		for w := 1; w <= 12; w++ {
			out := ClampToRect(s, Rect{W: w, H: 1})
			if got := CellsUpperBound(out); got > w {
				t.Errorf("%q at w=%d: clamped row can occupy %d cells: %q", s, w, got, out)
			}
			if strings.Count(out, "\n") != 0 {
				t.Errorf("%q at w=%d: clamp emitted more than one line", s, w)
			}
		}
	}
}

// contentMeasuresDisagree is a fact about the STRING, never a model of how tmux
// clusters it (#3614 rejects direction (1) for exactly that reason). Pinned so a
// later "improvement" that starts reasoning about clustering fails here first.
func TestContentMeasuresDisagreeIsACodePointTest(t *testing.T) {
	for _, s := range []string{
		joinedFamily,
		"\U0001F469‍\U0001F4BB",    // ZWJ profession
		"\U0001F44D\U0001F3FD",     // skin tone
		"❤️",                       // VS16
		"a‍b",                      // a joiner between two ASCII letters: still the class
		"prefix \U0001F3FF suffix", // a bare modifier
	} {
		if !contentMeasuresDisagree(s) {
			t.Errorf("%q carries a code point the measures disagree about, not detected", s)
		}
		if CellsUpperBound(s) < Cells(s) {
			t.Errorf("%q: the bound must never be below Cells", s)
		}
	}
	for _, s := range []string{
		"", "plain ascii", "日本語", "한글", "ＡＢ", "\U0001F600", "❤", "é",
		"╭──╮ ▸ ▾ ● ○ ◌ ▧ ◆ ⎇ ├ └ ─ ▲ ▼ … · ⚠",
		"\x1b[31mstyled\x1b[0m",
	} {
		if contentMeasuresDisagree(s) {
			t.Errorf("%q is ordinary content and must take Cells unchanged", s)
		}
		if got, want := contractCells(s), Cells(s); got != want {
			t.Errorf("%q: contract measure %d must equal Cells %d for ordinary content", s, got, want)
		}
	}
}

// The bound is only useful if it actually bounds tmux. This is the corpus #3610
// measured, asserted against the tmux column from Cells' table.
func TestCellsUpperBoundNeverUnderReportsTmux(t *testing.T) {
	for _, c := range []struct {
		name string
		s    string
		tmux int
	}{
		{"ascii", "abcd", 4},
		{"cjk", "你好", 4},
		{"emoji", "\U0001F600", 2},
		{"heart+vs16", "❤️", 2},
		{"heart text", "❤", 1},
		{"zwj family", joinedFamily, 4},
		{"zwj couple", "\U0001F469‍❤️‍\U0001F468", 2},
		{"zwj profession", "\U0001F469‍\U0001F4BB", 2},
		{"skin tone", "\U0001F44D\U0001F3FD", 2},
		{"regional", "\U0001F1FA\U0001F1F8", 2},
		{"combining", "é", 1},
		{"keycap", "1️⃣", 2},
		{"hangul", "한글", 4},
		{"fullwidth", "ＡＢ", 4},
	} {
		if got := CellsUpperBound(c.s); got < c.tmux {
			t.Errorf("%s: CellsUpperBound = %d, under the %d cells tmux advances — "+
				"the bound would let a row overflow", c.name, got, c.tmux)
		}
	}
	// Escapes are discounted, including an OSC hyperlink's URI, which
	// PrintableRuneWidth would otherwise count as visible text (#3433).
	link := "\x1b]8;;https://example.com\x1b\\LINK\x1b]8;;\x1b\\"
	if got, want := CellsUpperBound(link), 4; got != want {
		t.Errorf("CellsUpperBound(hyperlink) = %d, want %d", got, want)
	}
	// A block takes the widest line, like Cells.
	if got, want := CellsUpperBound("ab\n"+joinedFamily+"\nabc"), 8; got != want {
		t.Errorf("CellsUpperBound(block) = %d, want the widest line %d", got, want)
	}
}

// #3614's structural half. A right-edge affix is laid out FROM THE EDGE, so an
// over-estimated middle costs a shorter middle and the affix survives — the
// measured loss (#3610) was a sidebar row whose ● status dot was truncated away
// because the glyph sat at the end of the flowed text.
func TestRowWithRightAffixKeepsTheAffix(t *testing.T) {
	const dot = " ●"
	for _, c := range []struct {
		name  string
		flex  string
		width int
	}{
		{"clustered title", "fam" + joinedFamily + "zzz", 12},
		{"clustered title, tight", "fam" + joinedFamily + "zzz", 6},
		{"skin tone", "\U0001F44D\U0001F3FD name", 8},
		{"plain, fits", "short", 20},
		{"plain, overflows", strings.Repeat("x", 40), 12},
		{"exactly the affix plus one", "y", 3},
	} {
		out := RowWithRightAffix(c.flex, dot, c.width)
		if !strings.HasSuffix(out, dot) {
			t.Errorf("%s: the affix was truncated away: %q", c.name, out)
		}
		if got := CellsUpperBound(out); got > c.width {
			t.Errorf("%s: row can occupy %d cells, over the %d asked for: %q",
				c.name, got, c.width, out)
		}
		if got, want := contractCells(out), c.width; got != want {
			t.Errorf("%s: row measures %d in the contract measure, want exactly %d: %q",
				c.name, got, want, out)
		}
	}
	// Narrower than its own affix: no elastic middle is left, so the row degrades
	// with the affix rather than emitting something wider than the rectangle.
	for w := 0; w <= 2; w++ {
		out := RowWithRightAffix("title", dot, w)
		if got := CellsUpperBound(out); got > w {
			t.Errorf("w=%d: degenerate row occupies %d cells: %q", w, got, out)
		}
	}
}

// For ordinary content the helper must lay out exactly what the hand-rolled
// "place the flex in width-N, then join the affix" did, or adopting it moves
// every sidebar row that was already correct.
func TestRowWithRightAffixMatchesPlainLayout(t *testing.T) {
	for _, c := range []struct{ flex, affix string }{
		{"session-name", " ●"},
		{"", " ●"},
		{"日本語", " ●"},
		{"exactly-fits-here", " *"},
	} {
		const width = 24
		want := c.flex + strings.Repeat(" ", width-Cells(c.affix)-Cells(c.flex)) + c.affix
		if got := RowWithRightAffix(c.flex, c.affix, width); got != want {
			t.Errorf("RowWithRightAffix(%q, %q) = %q, want %q", c.flex, c.affix, got, want)
		}
	}
}

// A truncated styled flex must close its own styling, or the colour bleeds into
// the pad and then into the affix — the status dot would come out the title's
// colour instead of its own.
func TestRowWithRightAffixClosesStyleAtTheCut(t *testing.T) {
	out := RowWithRightAffix("\x1b[31m"+strings.Repeat("x", 40)+"\x1b[0m", " ●", 10)
	if !strings.Contains(out, "\x1b[0m ●") {
		t.Errorf("the cut must be reset-terminated before the affix: %q", out)
	}
}

// The sibling shape: an indicator that belongs NEXT TO its content rather than at
// the rectangle's edge — a tab row's " *" active cue (#1983). Same reservation in
// the same measure, so the clamp has no tail to take; the blanks just sit after
// the marker instead of before it.
func TestRowKeepingTailReservesTheTailAdjacent(t *testing.T) {
	const marker = " *"
	for _, c := range []struct {
		name  string
		flex  string
		width int
	}{
		{"clustered label", "  \u251c 1 " + joinedFamily + "tab", 16},
		{"clustered label, tight", "  \u251c 1 " + joinedFamily + "tab", 12},
		{"plain", "  \u251c 1 Agent", 24},
		{"plain, overflows", "  \u251c 1 " + strings.Repeat("n", 40), 20},
	} {
		out := RowKeepingTail(c.flex, marker, c.width)
		if !strings.Contains(out, marker) {
			t.Errorf("%s: the active marker was truncated away: %q", c.name, out)
		}
		if got := CellsUpperBound(out); got > c.width {
			t.Errorf("%s: row can occupy %d cells, over the %d asked for: %q",
				c.name, got, c.width, out)
		}
		if got, want := contractCells(out), c.width; got != want {
			t.Errorf("%s: row measures %d, want exactly %d: %q", c.name, got, want, out)
		}
		// Adjacent, not edge-pinned: the blanks come AFTER the marker. Moving it to
		// the edge would be a redesign of the cue, which is why the two shapes are
		// separate functions.
		if trailing := out[strings.LastIndex(out, marker)+len(marker):]; strings.TrimLeft(trailing, " ") != "" {
			t.Errorf("%s: only blanks may follow the marker, got %q", c.name, trailing)
		}
	}
	// For plain content it is exactly the hand-rolled "append the marker, then pad".
	if got, want := RowKeepingTail("\u251c 1 Agent", marker, 20), "\u251c 1 Agent *         "; got != want {
		t.Errorf("RowKeepingTail = %q, want %q", got, want)
	}
}

// The detection set was swept against a wider corpus than the triage named, and
// this pins both halves of what came back.
//
// IN: the England flag is a base plus TAG characters — no ZWJ, no variation
// selector, no modifier — and measures 2 to x/ansi against 8 to
// PrintableRuneWidth. Identical shape and identical size of disagreement to the
// chained family this issue is about, on content a user can put in a session
// title, so leaving it out would be a silent overflow of the kind being fixed.
//
// OUT: ordinary script clusters where the measures also disagree. There
// PrintableRuneWidth OVER-counts, so bounding by it shortens real titles in those
// scripts on every row that carries them — the measured harm that made #3610
// withdraw the blanket overestimate — to buy a guarantee against a disagreement
// nobody has measured tmux on. If that measurement ever exists, this test is
// where the decision changes.
func TestDetectionCoversEmojiClustersAndNotOrdinaryScripts(t *testing.T) {
	const englandFlag = "\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F"

	if !contentMeasuresDisagree(englandFlag) {
		t.Error("a tag-sequence flag carries no ZWJ, VS16 or modifier and still splits the " +
			"measures 2-against-8; it must be bounded like the chained family")
	}
	if got, want := CellsUpperBound(englandFlag), 8; got != want {
		t.Errorf("CellsUpperBound(england flag) = %d, was %d when measured", got, want)
	}
	for w := 1; w <= 10; w++ {
		out := ClampToRect("ab"+englandFlag+"cd", Rect{W: w, H: 1})
		if got := CellsUpperBound(out); got > w {
			t.Errorf("w=%d: a row with a tag-sequence flag can occupy %d cells: %q", w, got, out)
		}
	}

	// Excluded, each with the disagreement that makes it tempting, so removing it
	// from this list is a deliberate act rather than an oversight.
	for _, c := range []struct {
		name       string
		s          string
		xansi, prw int
	}{
		{"devanagari", "\u0915\u094d\u0937", 1, 3},
		{"thai", "\u0e01\u0e33", 1, 2},
		{"hangul jamo", "\u1100\u1161\u11a8", 2, 4},
	} {
		if contentMeasuresDisagree(c.s) {
			t.Errorf("%s is ordinary script text; bounding it by PrintableRuneWidth (%d against "+
				"x/ansi's %d) shortens real titles in that script. Adding it needs a tmux "+
				"measurement first", c.name, c.prw, c.xansi)
		}
		if got, want := contractCells(c.s), Cells(c.s); got != want {
			t.Errorf("%s: contract measure %d must equal Cells %d", c.name, got, want)
		}
	}
}

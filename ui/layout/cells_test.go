package layout

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/ansi"
)

// joinedFamily is four emoji joined by three ZWJs — the case the three width
// functions in this tree disagree about most, and the one tmux 3.4 was measured
// advancing 4 cells for.
const joinedFamily = "\U0001F468‍\U0001F469‍\U0001F467‍\U0001F466"

// Cells against what tmux actually advances. Exact everywhere except the chained
// ZWJ family, which is recorded as a KNOWN deviation rather than smoothed over:
// x-ansi reports 2 where tmux advances 4.
//
// The overestimating alternative was tried first and measured worse — it cut the
// ● status dot off every sidebar row whose session title carries clustered emoji.
// See Cells' doc comment. This table is the evidence for both halves, so a change
// of measure has to come back through it.
func TestCellsMatchesTmuxExceptTheRecordedDeviation(t *testing.T) {
	for _, c := range []struct {
		name string
		s    string
		tmux int
		// known reports the value Cells gives when it differs from tmux; 0 means
		// they must agree.
		known int
	}{
		{"ascii", "abcd", 4, 0},
		{"cjk", "你好", 4, 0},
		{"emoji", "\U0001F600", 2, 0},
		{"heart+vs16", "❤️", 2, 0},
		{"heart text", "❤", 1, 0},
		{"zwj family", joinedFamily, 4, 2},
		{"zwj couple", "\U0001F469‍❤️‍\U0001F468", 2, 0},
		{"zwj profession", "\U0001F469‍\U0001F4BB", 2, 0},
		{"skin tone", "\U0001F44D\U0001F3FD", 2, 0},
		{"regional", "\U0001F1FA\U0001F1F8", 2, 0},
		{"combining", "é", 1, 0},
		{"keycap", "1️⃣", 2, 0},
		{"hangul", "한글", 4, 0},
		{"fullwidth", "ＡＢ", 4, 0},
	} {
		want := c.tmux
		if c.known != 0 {
			want = c.known
		}
		if got := Cells(c.s); got != want {
			t.Errorf("%s: Cells = %d, want %d (tmux advances %d)", c.name, got, want, c.tmux)
		}
	}
}

// The one deviation, pinned on its own so it cannot drift silently: if x-ansi ever
// starts reporting the chained family as tmux advances it, this fails and the
// table above should be re-measured rather than edited.
func TestCellsUnderReportsTheChainedFamily(t *testing.T) {
	if got := Cells(joinedFamily); got != 2 {
		t.Fatalf("Cells(chained family) = %d, was 2 when measured; tmux advances 4. "+
			"Re-measure the corpus before changing the table", got)
	}
}

// Escape sequences occupy no cells, whichever form they take. PrintableRuneWidth
// counts an OSC hyperlink's URI as visible text, so it is fed the stripped string.
func TestCellsDiscountsEscapeSequences(t *testing.T) {
	for _, c := range []struct {
		name string
		s    string
		want int
	}{
		{"sgr", "\x1b[31mLINK\x1b[0m", 4},
		{"osc8 ST", "\x1b]8;;https://example.com\x1b\\LINK\x1b]8;;\x1b\\", 4},
		{"osc8 BEL", "\x1b]8;;https://example.com\aLINK", 4},
		{"osc8 C1 ST", "\x1b]8;;https://example.com\u009cLINK", 4},
	} {
		if got := Cells(c.s); got != c.want {
			t.Errorf("%s: Cells = %d, want %d", c.name, got, c.want)
		}
	}
}

// For plain content every candidate agrees, so adopting the shared helper cannot
// silently move anything that was already correct.
func TestCellsAgreesWithEveryCandidateOnPlainContent(t *testing.T) {
	for _, s := range []string{"", "plain", strings.Repeat("x", 200), "你好世界", "한글", "ＡＢ"} {
		want := lipgloss.Width(s)
		if got := Cells(s); got != want {
			t.Errorf("Cells(%q) = %d, lipgloss.Width = %d", s, got, want)
		}
		if got := ansi.PrintableRuneWidth(s); got != want {
			t.Errorf("PrintableRuneWidth(%q) = %d, lipgloss.Width = %d: precondition", s, got, want)
		}
	}
}

// BlockWidth takes the widest LINE; Cells measures one line and, handed a block,
// returns the SUM of its rows because that is what x/ansi does with newlines.
//
// This is not a nicety. overlayOrigin measures a whole frame to centre a modal in
// it, and summing its rows reported a 120-column frame as far wider, pushed the
// modal to the right-hand clamp, and left every mouse zone registered somewhere
// the modal was not drawn — #3585's own defect, reintroduced by its fix. The
// real-terminal play-test caught it; this pins it here so it fails in a second.
func TestBlockWidthTakesTheWidestLineNotTheSum(t *testing.T) {
	block := "abcd\nab\nabcdefgh"
	if got, want := BlockWidth(block), 8; got != want {
		t.Fatalf("BlockWidth = %d, want %d (the widest line)", got, want)
	}
	if Cells(block) <= 8 {
		t.Fatal("precondition: Cells is expected to SUM a multi-line block, which is why " +
			"BlockWidth exists; if that changed, revisit both")
	}
	if got, want := BlockWidth("one line"), Cells("one line"); got != want {
		t.Fatalf("for a single line the two must agree: BlockWidth=%d Cells=%d", got, want)
	}
	if got := BlockWidth(""); got != 0 {
		t.Fatalf("BlockWidth(\"\") = %d, want 0", got)
	}
}

// And BlockWidth must agree with lipgloss.Width, whose callers it replaced, for
// content where the underlying measures agree — otherwise adopting it silently
// moves every modal.
func TestBlockWidthMatchesLipglossOnPlainBlocks(t *testing.T) {
	for _, b := range []string{
		"one\ntwo\nthree",
		"╭──────╮\n│ hi   │\n╰──────╯",
		strings.Repeat("x", 80) + "\n" + strings.Repeat("y", 40),
	} {
		if got, want := BlockWidth(b), lipgloss.Width(b); got != want {
			t.Errorf("BlockWidth = %d, lipgloss.Width = %d for %q", got, want, b)
		}
	}
}

// #3585 review. ClampToRect promises a rectangle of exactly r.W cells per row.
// Measuring with Cells while truncating with x/ansi broke that promise wherever
// the two disagree: "\U0001F44D\U0001F3FD" is 4 cells to Cells and 2 to x/ansi, so
// clamping it into 3 columns returned the grapheme untouched — still 4 by Cells,
// so the pad was skipped — and the row went out narrower than the rectangle.
func TestClampToRectFillsTheRectangleForOverestimatedGraphemes(t *testing.T) {
	const skinTone = "\U0001F44D\U0001F3FD"
	if Cells(skinTone) <= 3 {
		t.Skipf("this case needs Cells to over-report the grapheme; got %d", Cells(skinTone))
	}
	for _, w := range []int{1, 2, 3, 4, 6} {
		out := ClampToRect(skinTone, Rect{W: w, H: 1})
		if got := Cells(out); got != w {
			t.Errorf("ClampToRect(w=%d) produced %d cells, want exactly %d: %q", w, got, w, out)
		}
	}
}

// TruncateToCells must satisfy the measure its callers pad by, and must never cut
// through a control sequence while doing it.
func TestTruncateToCellsFitsTheMeasureAndKeepsSequencesWhole(t *testing.T) {
	link := "\x1b]8;;https://example.com\x1b\\LINK\x1b]8;;\x1b\\"
	for _, c := range []struct {
		name  string
		s     string
		width int
	}{
		{"skin tone", "\U0001F44D\U0001F3FD", 3},
		{"zwj family", joinedFamily, 3},
		{"plain", strings.Repeat("x", 50), 10},
		{"hyperlink", link, 2},
		{"already fits", "abc", 10},
	} {
		out := TruncateToCells(c.s, c.width)
		if got := Cells(out); got > c.width {
			t.Errorf("%s: TruncateToCells gave %d cells, over the %d asked for", c.name, got, c.width)
		}
		if strings.Count(out, "\x1b]8;;") == 1 {
			t.Errorf("%s: a hyperlink was cut in half: %q", c.name, out)
		}
	}
	if got := TruncateToCells("abc", 0); got != "" {
		t.Errorf("zero width must yield empty, got %q", got)
	}
}

package layout

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/ansi"
)

// joinedFamily is four emoji joined by three ZWJs — the case the three width
// functions in this tree disagree about most, and the one tmux 3.4 was measured
// advancing 4 cells for.
const joinedFamily = "\U0001F468‍\U0001F469‍\U0001F467‍\U0001F466"

// Cells must never report FEWER cells than tmux advances. An underestimate
// overflows the frame and wraps, and a wrapped row makes every height budget
// above it a lie (#3430); an overestimate merely clips, which is visible and
// bounded now that #3578 clips instead of dropping the frame.
//
// The tmux column is measured, not assumed: see Cells' doc comment for the run.
func TestCellsNeverUnderestimatesWhatTmuxAdvances(t *testing.T) {
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
		{"combining", "é", 1},
		{"keycap", "1️⃣", 2},
		{"hangul", "한글", 4},
		{"fullwidth", "ＡＢ", 4},
	} {
		if got := Cells(c.s); got < c.tmux {
			t.Errorf("%s: Cells = %d, tmux advances %d — an underestimate overflows the frame and wraps",
				c.name, got, c.tmux)
		}
	}
}

// The single case that forces the max(): x-ansi alone under-reports the chained
// family, which is exactly the content #3433 is about.
func TestCellsCoversTheChainedFamilyXAnsiUnderReports(t *testing.T) {
	if xansi.StringWidth(joinedFamily) >= 4 {
		t.Skip("x-ansi no longer under-reports this; the max() may be reconsidered from fresh measurements")
	}
	if got := Cells(joinedFamily); got < 4 {
		t.Fatalf("Cells = %d, want at least the 4 cells tmux advances", got)
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

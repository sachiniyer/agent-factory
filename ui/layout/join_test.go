package layout

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// For plain content the contract joins must be byte-identical to lipgloss's, or
// adopting them silently moves every frame af has ever drawn.
func TestJoinsMatchLipglossOnPlainBlocks(t *testing.T) {
	rail := "aaaa\nbbbb\ncccc"
	pane := "1234567\n1234567\n1234567"
	short := "xy\nz"
	bar := "status bar"

	for _, c := range []struct {
		name   string
		blocks []string
	}{
		{"equal heights", []string{rail, pane}},
		{"ragged heights", []string{rail, short, pane}},
		{"single block", []string{rail}},
		{"single RAGGED block", []string{short}},
		{"with an empty block", []string{rail, "", pane}},
		{"styled", []string{"\x1b[31mred!\x1b[0m\nblue", pane}},
	} {
		if got, want := JoinHorizontal(c.blocks...), lipgloss.JoinHorizontal(lipgloss.Top, c.blocks...); got != want {
			t.Errorf("%s: JoinHorizontal = %q, lipgloss = %q", c.name, got, want)
		}
	}
	for _, c := range []struct {
		name   string
		blocks []string
	}{
		{"stacked rail", []string{rail, "────", short}},
		{"frame plus bar", []string{rail, bar}},
		{"single block", []string{rail}},
		{"single RAGGED block", []string{short}},
	} {
		if got, want := JoinVertical(c.blocks...), lipgloss.JoinVertical(lipgloss.Left, c.blocks...); got != want {
			t.Errorf("%s: JoinVertical = %q, lipgloss = %q", c.name, got, want)
		}
	}
	if got := JoinHorizontal(); got != "" {
		t.Errorf("JoinHorizontal() = %q, want empty", got)
	}
}

// #3614's other half, and the reason these exist at all. A row ClampToRect
// deliberately under-fills — because its true width cannot be known — measures
// SHORT to lipgloss, so lipgloss's Join pads it back out and restores exactly the
// overflow the clamp had just removed. Measured:
//
//	clamped row      x/ansi 16, upper bound 22 — inside a 22-cell rectangle
//	after lipgloss   x/ansi 22, upper bound 28 — 2 cells past it in tmux
//
// So the clamp alone would have been a no-op in the real app.
func TestJoinsDoNotRepadAnUnderfilledRow(t *testing.T) {
	const w = 22
	// The shape a clamped sidebar produces: every row at the rectangle's width in
	// the contract measure, the clustered one short of it in x/ansi's.
	clustered := " " + joinedFamily + strings.Repeat("b", 13)
	rail := strings.Repeat("a", w) + "\n" + clustered + "\n" + strings.Repeat("c", w)
	if got := contractCells(clustered); got != w {
		t.Fatalf("precondition: the clamped row must be exactly %d in the contract measure, got %d", w, got)
	}
	if Cells(clustered) >= w {
		t.Fatalf("precondition: the clamped row must be SHORT in x/ansi's measure, got %d", Cells(clustered))
	}

	for _, c := range []struct {
		name string
		out  string
	}{
		{"vertical", JoinVertical(rail, strings.Repeat("d", w))},
		{"horizontal", JoinHorizontal(rail, "pppppppppp\nqqqqqqqqqq\nrrrrrrrrrr")},
		{"lipgloss vertical, for contrast", lipgloss.JoinVertical(lipgloss.Left, rail, strings.Repeat("d", w))},
	} {
		bound := CellsUpperBound(c.out)
		if strings.HasPrefix(c.name, "lipgloss") {
			// Not an assertion about our code — the contrast that says the trap is
			// real. If this ever stops holding, these helpers can go away.
			if bound <= w {
				t.Errorf("lipgloss no longer re-pads the under-filled row (bound %d ≤ %d); "+
					"re-measure before keeping the contract joins", bound, w)
			}
			continue
		}
		if c.name == "vertical" && bound > w {
			t.Errorf("%s: joined frame can occupy %d cells, over the %d-cell rail: %q",
				c.name, bound, w, c.out)
		}
		if c.name == "horizontal" && bound > w+10 {
			t.Errorf("%s: joined frame can occupy %d cells, over the %d columns it was built from: %q",
				c.name, bound, w+10, c.out)
		}
	}
}

// A column that is short of the tallest block is filled with blank rows of that
// column's width, so every column after it starts at the same screen coordinate on
// every row — the property the mouse zones are registered against (#3585).
func TestJoinHorizontalKeepsColumnsAligned(t *testing.T) {
	out := JoinHorizontal("aaa\nbbb\nccc", "1\n2", "zz")
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 rows, got %d: %q", len(lines), out)
	}
	for i, line := range lines {
		if got, want := Cells(line), 3+1+2; got != want {
			t.Errorf("row %d is %d cells, want %d: %q", i, got, want, line)
		}
	}
	if lines[2] != "ccc"+" "+"  " {
		t.Errorf("short columns must fill with blanks, got %q", lines[2])
	}
}

// The differential that actually found something. The hand-written cases above
// were written by the same person who wrote the code, and they missed a real
// divergence for the oldest reason: the "single block" case used equal-width
// lines, so the padding under test was a no-op and it passed.
//
// lipgloss short-circuits a lone block and returns it untouched. This padded its
// lines out to the widest, so a RAGGED single block came back padded here and
// unpadded there — invisible in the app today, since every seam in home_view
// joins at least two blocks, but a divergence on precisely the path where these
// helpers are supposed to be invisible.
//
// Randomised over the alphabet the frame is actually built from, because the
// content that breaks a width helper is never the content you think to type.
func TestJoinsMatchLipglossOverRandomPlainBlocks(t *testing.T) {
	alphabet := []string{"a", "b", "z", " ", "你", "─", "│", "▸", "\x1b[31mr\x1b[0m", "…", "é", "한"}
	rng := rand.New(rand.NewSource(20260902))

	block := func() string {
		lines := make([]string, rng.Intn(4)+1)
		for i := range lines {
			var b strings.Builder
			for j := 0; j < rng.Intn(8); j++ {
				b.WriteString(alphabet[rng.Intn(len(alphabet))])
			}
			lines[i] = b.String()
		}
		return strings.Join(lines, "\n")
	}

	for iter := 0; iter < 5000; iter++ {
		blocks := make([]string, rng.Intn(3)+1)
		for i := range blocks {
			blocks[i] = block()
		}
		if got, want := JoinHorizontal(blocks...), lipgloss.JoinHorizontal(lipgloss.Top, blocks...); got != want {
			t.Fatalf("JoinHorizontal diverged for %q:\n got  %q\n want %q", blocks, got, want)
		}
		if got, want := JoinVertical(blocks...), lipgloss.JoinVertical(lipgloss.Left, blocks...); got != want {
			t.Fatalf("JoinVertical diverged for %q:\n got  %q\n want %q", blocks, got, want)
		}
	}
}

// Codex P1 on #3657, and the answer is "yes, and that is the recorded trade" —
// but the test that was supposed to cover it only checked a loose bound, so it
// could not have told the difference. This pins the offset exactly.
//
// What JoinHorizontal guarantees is that column N begins at the sum of the
// contract widths of the columns before it, ON EVERY ROW. That is the space the
// Grid solves in, the space panes are clamped in, and the space mouse zones are
// registered in, so it is the offset that has to be exact.
//
// What it does NOT guarantee is the DRAWN offset on a row carrying content whose
// true width nothing can report. Such a row is deliberately under-filled, so the
// next column is drawn LEFT of its allocation by the disagreement — up to 4 cells
// for the family below. That is #3614's accepted cost, stated here rather than
// implied: an overflow would wrap the row and make every height budget above it a
// lie (#3430), and this is the other side of that choice. It is not new either —
// the same row was drawn 2 cells RIGHT of its allocation before, plus a wrap.
func TestJoinHorizontalPutsEachColumnAtItsAllocatedOffset(t *testing.T) {
	const railW, paneW = 22, 10
	railLines := []string{
		strings.Repeat("a", railW),
		" " + joinedFamily + strings.Repeat("b", 13), // contract-exact, x/ansi short
		strings.Repeat("c", railW),
	}
	paneLines := []string{
		strings.Repeat("p", paneW),
		strings.Repeat("q", paneW),
		strings.Repeat("r", paneW),
	}
	for i, l := range railLines {
		if got := contractCells(l); got != railW {
			t.Fatalf("precondition: rail line %d is %d in the contract measure, want %d", i, got, railW)
		}
	}

	out := JoinHorizontal(strings.Join(railLines, "\n"), strings.Join(paneLines, "\n"))
	rows := strings.Split(out, "\n")
	if len(rows) != len(railLines) {
		t.Fatalf("want %d rows, got %d", len(railLines), len(rows))
	}
	for i, row := range rows {
		// The exact bytes: rail line, padded to the rail's contract width, then the
		// pane line. No slack anywhere for an offset to hide in.
		if want := padToContract(railLines[i], railW) + padToContract(paneLines[i], paneW); row != want {
			t.Errorf("row %d: got %q, want %q", i, row, want)
		}
		// And said as an offset, which is the property the seam depends on.
		if got := contractCells(row) - contractCells(paneLines[i]); got != railW {
			t.Errorf("row %d: the second column begins at contract column %d, want %d",
				i, got, railW)
		}
	}

	// The under-fill is real and bounded, and the row that carries it is named.
	// If this ever reports 0 the deliberate under-fill has stopped happening and
	// the trade above needs re-reading.
	drawnShortfall := contractCells(railLines[1]) - Cells(railLines[1])
	if drawnShortfall != 6 {
		t.Errorf("the clustered rail row is short by %d cells in x/ansi's measure, was 6 when "+
			"measured; the second column is drawn that much left of its allocation", drawnShortfall)
	}
}

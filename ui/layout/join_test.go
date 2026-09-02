package layout

import (
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

package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// #2149: fitLine is the width clamp behind every task-modal row. It used to
// measure with a plain rune width, which counts each byte of an escape
// sequence as a visible cell. Styled content was therefore cut many cells
// early and — since the cut landed at a byte offset — usually in the middle of
// an escape sequence. A terminal swallows everything after an unterminated CSI
// until a final byte arrives, so the mangled row lost its own padding and right
// border and the pane behind printed inside the modal.

// truecolor pins the color profile so styles actually emit SGR sequences.
// Under the default test profile every style renders as plain text and this
// whole class of damage is invisible.
func truecolor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// hasTruncatedEscape reports whether s ends inside an escape sequence — a CSI
// introduced but never closed by a final byte in 0x40-0x7E.
func hasTruncatedEscape(s string) bool {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] != 0x1b {
			continue
		}
		if i+1 >= len(b) {
			return true
		}
		if b[i+1] != '[' {
			i++
			continue
		}
		j := i + 2
		for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
			j++
		}
		if j >= len(b) {
			return true
		}
		i = j
	}
	return false
}

func TestFitLineMeasuresStyledContentInCells(t *testing.T) {
	truecolor(t)

	styled := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#dcdccc")).Render("At 3 : 00  AM")
	if got, want := lipgloss.Width(styled), 13; got != want {
		t.Fatalf("test fixture is %d cells wide, want %d", got, want)
	}
	if got := fitLine(styled, 20); got != styled {
		t.Fatalf("fitLine truncated content that fits in 20 cells (%d visible): got %q", lipgloss.Width(styled), got)
	}
}

func TestFitLineNeverSplitsAnEscapeSequence(t *testing.T) {
	truecolor(t)

	style := lipgloss.NewStyle().Foreground(lipgloss.Color("#dcdccc"))
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString(style.Render("chunk "))
	}
	styled := b.String()

	for width := 1; width <= lipgloss.Width(styled); width++ {
		out := fitLine(styled, width)
		if hasTruncatedEscape(out) {
			t.Fatalf("fitLine(_, %d) cut inside an escape sequence: %q", width, out)
		}
		if got := lipgloss.Width(out); got > width {
			t.Fatalf("fitLine(_, %d) returned %d cells: %q", width, got, out)
		}
	}
}

// TestFitLinePlainTextUnchanged pins the behavior every other caller relies on:
// plain text still clips to the requested cell count with a trailing ellipsis,
// dropped when even the ellipsis cannot fit, and wide runes count as 2 cells.
func TestFitLinePlainTextUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{"fits", "nightly sweep", 20, "nightly sweep"},
		{"exact", "nightly", 7, "nightly"},
		{"clipped", "nightly sweep", 8, "nightly…"},
		{"no room for the tail", "nightly", 0, ""},
		{"wide runes", "日本語です", 4, "日…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fitLine(tc.in, tc.width); got != tc.want {
				t.Fatalf("fitLine(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
			}
		})
	}
}

// TestSchedulePickerRowsSurviveStyling is the #2149 repro at the row level: the
// schedule editor's rows fit the pane width easily, so styling them must not
// cost a single visible cell.
func TestSchedulePickerRowsSurviveStyling(t *testing.T) {
	truecolor(t)

	p := newSchedulePicker()
	p.setWidth(48) // the task modal's content width at 80x24
	p.setFocused(true)
	p.hourStr = "3"

	rows := strings.Split(p.render(), "\n")
	for i, row := range rows {
		if hasTruncatedEscape(row) {
			t.Fatalf("schedule row %d ends inside an escape sequence: %q", i, row)
		}
	}
	joined := strings.Join(rows, "\n")
	for _, want := range []string{"At", "3", "00", "AM", "Every day at 3:00 AM"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("schedule editor dropped %q from its rows:\n%q", want, joined)
		}
	}
}

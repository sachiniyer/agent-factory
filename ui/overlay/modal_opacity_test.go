package overlay

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// #2149: a modal must be OPAQUE — every cell inside its rectangle (border,
// field rows, and blank padding alike) is painted by the modal, so the pane
// behind it can never show through or visually merge with the form.
//
// The tests below assert that property against the real render path: the real
// ui.TaskPane edit form, framed the way app/render.go frames it, composited by
// the real PlaceOverlay over a background pane that has content.
//
// Two things broke it, and both are checked here:
//
//   - The compositor left the tail of a short foreground row transparent, so a
//     modal whose rows are not all padded to its full width showed background
//     through the gap.
//   - ui.fitLine measured styled content with an ANSI-blind width, so the
//     schedule picker's rows were truncated far too early and, worse, mid
//     escape sequence. A terminal swallows every byte after an unterminated
//     CSI until a final byte arrives — eating the modal's own padding and
//     right border, and letting the background print inside the form.

// continuationCell marks the second cell of a double-width grapheme so column
// arithmetic over the rasterized grid stays in terminal cells, not runes.
const continuationCell = '\x00'

// blankCell is what an untouched terminal cell shows.
const blankCell = ' '

// rasterizeLine converts one rendered line into the cells a terminal would
// actually display. It implements just enough of the ANSI ground/CSI state
// machine to be faithful about the failure this issue is about: a CSI runs
// from "\x1b[" until a final byte in 0x40-0x7E, and everything it consumes
// prints nothing. A sequence truncated mid-parameter therefore eats the
// characters that follow it — which is precisely how a mangled modal row loses
// its padding and right border and lets the pane behind bleed into the form.
func rasterizeLine(line string) []rune {
	var cells []rune
	b := []byte(line)
	for i := 0; i < len(b); {
		if b[i] == 0x1b {
			i++
			if i < len(b) && b[i] == '[' {
				i++
				for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) {
					i++
				}
				if i < len(b) {
					i++ // the final byte, consumed by the sequence
				}
				continue
			}
			if i < len(b) {
				i++ // two-byte escape
			}
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		i += size
		w := runewidth.RuneWidth(r)
		if w <= 0 {
			continue
		}
		cells = append(cells, r)
		for k := 1; k < w; k++ {
			cells = append(cells, continuationCell)
		}
	}
	return cells
}

// rasterize renders a whole frame into a cell grid, padding every row out to
// width so callers can index a rectangle without bounds checks. Cells the
// frame never wrote read as blanks, exactly as they would on screen.
func rasterize(frame string, width int) [][]rune {
	lines := strings.Split(frame, "\n")
	grid := make([][]rune, len(lines))
	for i, line := range lines {
		row := rasterizeLine(line)
		if len(row) > width {
			row = row[:width]
		}
		for len(row) < width {
			row = append(row, blankCell)
		}
		grid[i] = row
	}
	return grid
}

// backgroundFrame is a stand-in for a session pane left showing terminal
// output — the git-commit summary from the #2149 repro, on every row, so any
// transparent cell anywhere in the modal rectangle shows a recognizable glyph.
func backgroundFrame(width, height int) string {
	const content = "1 file changed, 3 insertions(+) >> README.md "
	row := strings.Repeat(content, width/len(content)+2)[:width]
	lines := make([]string, height)
	for i := range lines {
		lines[i] = row
	}
	return strings.Join(lines, "\n")
}

// modalFrameStyle mirrors app/render.go's hooksOverlayStyle — the frame every
// pane-hosted modal (tasks, hooks, config) is rendered in. Only the accent
// border color is dropped; it has no bearing on opacity.
func modalFrameStyle(width, height int) lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(1, 2).
		Width(width).
		Height(height)
}

// taskEditModal renders the real task-edit form for a cron task, focused on the
// schedule field (the #2149 repro: `m` on an existing cron task), inside the
// modal frame the app draws around it.
func taskEditModal(t *testing.T, frameW, frameH int) string {
	t.Helper()
	pane := ui.NewTaskPane()
	pane.SetTasks([]task.Task{{
		ID:          "t1",
		Name:        "nightly",
		Prompt:      "run the nightly sweep",
		CronExpr:    "0 3 * * *",
		ProjectPath: "/repo",
		Enabled:     true,
	}})
	pane.SelectTask(0)
	pane.EnterEditSelected()
	// Tab from Name past Trigger onto the schedule picker, so the picker draws
	// its focused cells and hint row.
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})

	style := modalFrameStyle(frameW, frameH)
	pane.SetSize(frameW-style.GetHorizontalPadding(), frameH-style.GetVerticalPadding())
	return style.Render(pane.String())
}

// taskCreateModal renders the real "New task" form (the `n` path) in the same
// frame, so the adjacent-surface audit covers create as well as edit.
func taskCreateModal(t *testing.T, frameW, frameH int) string {
	t.Helper()
	pane := ui.NewTaskPane()
	pane.EnterCreateMode("/repo")
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})
	pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyTab})

	style := modalFrameStyle(frameW, frameH)
	pane.SetSize(frameW-style.GetHorizontalPadding(), frameH-style.GetVerticalPadding())
	return style.Render(pane.String())
}

// truecolor pins the color profile so styles actually emit SGR sequences.
// Under the default test profile every style renders as plain text and the
// whole class of escape-sequence damage is invisible.
func truecolor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// placement mirrors PlaceOverlay's centering and clamping so a test can name
// the rectangle the modal occupies in the composed frame.
func placement(fg, bg string) (x, y, w, h int) {
	fgLines := strings.Split(fg, "\n")
	bgLines := strings.Split(bg, "\n")
	w, h = lipgloss.Width(fg), len(fgLines)
	bgW, bgH := lipgloss.Width(bg), len(bgLines)
	x = layout.ClampInt((bgW-w)/2, 0, bgW-w)
	y = layout.ClampInt((bgH-h)/2, 0, bgH-h)
	return x, y, w, h
}

// TestTaskEditModalOccludesBackgroundPane is the #2149 repro. At 80x24, with
// the task-edit form open over a pane showing commit output, every cell of the
// modal rectangle in the composed frame must be the modal's own — never the
// background's.
func TestTaskEditModalOccludesBackgroundPane(t *testing.T) {
	truecolor(t)

	const termW, termH = 80, 24
	// app/render.go: preferredOverlayWidth(52) and preferredOverlayHeight() at
	// 80x24, less the frame's border.
	fg := taskEditModal(t, 50, 12)
	bg := backgroundFrame(termW, termH)

	x, y, w, h := placement(fg, bg)
	composed := rasterize(PlaceOverlay(0, 0, fg, bg, true), termW)
	modal := rasterize(fg, w)

	for row := 0; row < h; row++ {
		for col := 0; col < w; col++ {
			got, want := composed[y+row][x+col], modal[row][col]
			if got != want {
				t.Fatalf("background bled into the modal at row %d col %d: got %q, want %q\n"+
					"composed row: %q\nmodal row:    %q",
					row, col, string(got), string(want),
					string(composed[y+row][x:x+w]), string(modal[row]))
			}
		}
	}
}

// TestTaskEditModalPaintsItsWholeRectangle checks the other half of the
// property: the modal itself paints every cell of its rectangle. A row that
// rasterizes short — because a truncated escape sequence swallowed its padding
// and right border — is a hole in the modal no compositor can fill correctly.
func TestTaskEditModalPaintsItsWholeRectangle(t *testing.T) {
	truecolor(t)

	fg := taskEditModal(t, 50, 12)
	lines := strings.Split(fg, "\n")
	width := lipgloss.Width(fg)

	for i, line := range lines {
		cells := rasterizeLine(line)
		if len(cells) != width {
			t.Fatalf("modal row %d paints %d cells, want %d (the rest is transparent): %q",
				i, len(cells), width, line)
		}
		if last := cells[len(cells)-1]; last == blankCell {
			t.Fatalf("modal row %d lost its right border: %q", i, string(cells))
		}
	}
}

// TestPlaceOverlayFillsRaggedForegroundRows isolates the compositor. A modal
// whose rows are not all padded to its full width must still occlude the
// background across its whole rectangle — the blank tail of a short row is
// modal, not a window onto the pane below.
func TestPlaceOverlayFillsRaggedForegroundRows(t *testing.T) {
	fg := strings.Join([]string{
		"+--------------+",
		"| short",
		"|",
		"| a longer row |",
		"+--------------+",
	}, "\n")
	bg := backgroundFrame(40, 9)

	x, y, w, h := placement(fg, bg)
	composed := rasterize(PlaceOverlay(0, 0, fg, bg, true), 40)
	modal := rasterize(fg, w)

	for row := 0; row < h; row++ {
		got, want := string(composed[y+row][x:x+w]), string(modal[row])
		if got != want {
			t.Fatalf("background bled through modal row %d: got %q, want %q", row, got, want)
		}
	}
}

// TestEveryModalOccludesBackgroundPane is the adjacent-surface audit #2149
// asks for: the create-task form and every ui/overlay modal must hold the same
// opacity property as the edit form, over the same busy pane.
func TestEveryModalOccludesBackgroundPane(t *testing.T) {
	truecolor(t)

	const termW, termH = 80, 24
	bg := backgroundFrame(termW, termH)

	confirm := NewConfirmationOverlay("Delete session 'nightly-sweep'?")
	confirm.SetMaxSize(termW, termH)
	picker := NewProjectPickerOverlay([]Project{
		{Name: "agent-factory", Root: "/repo/agent-factory"},
		{Name: "notes", Root: "/repo/notes"},
	}, "/repo/agent-factory")
	picker.SetMaxSize(termW, termH)
	selection := NewSelectionOverlay("Choose an agent", []string{"claude", "aider", "codex"})
	selection.SetMaxSize(termW, termH)
	prompt := NewPromptOverlay("Initial prompt", "fix the flaky test")
	prompt.SetMaxSize(termW, termH)
	search := NewSearchOverlay(nil)
	search.SetMaxSize(termW, termH)
	help := NewTextOverlay("Help\n\nj/k move\nenter attach\nq quit")
	help.SetWidth(50)
	help.SetHeight(12)

	for _, tc := range []struct {
		name string
		fg   string
	}{
		{"task edit form", taskEditModal(t, 50, 12)},
		{"task create form", taskCreateModal(t, 50, 12)},
		{"confirmation", confirm.Render()},
		{"project picker", picker.Render()},
		{"program selection", selection.Render()},
		{"initial prompt", prompt.Render()},
		{"session search", search.Render()},
		{"help", help.Render()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, y, w, h := placement(tc.fg, bg)
			composed := rasterize(PlaceOverlay(0, 0, tc.fg, bg, true), termW)
			modal := rasterize(tc.fg, w)
			for row := 0; row < h; row++ {
				got, want := string(composed[y+row][x:x+w]), string(modal[row])
				if got != want {
					t.Fatalf("background bled into modal row %d: got %q, want %q", row, got, want)
				}
				if strings.Contains(got, "file changed") {
					t.Fatalf("background text printed inside modal row %d: %q", row, got)
				}
			}
		})
	}
}

// TestPlaceOverlayBlanksTheBackgroundBesideTheModal guards the #5297 contract,
// the other direction from opacity: making the modal opaque must not widen it
// into background territory either. On a row the modal occupies, every cell
// outside its rectangle is BLANK backdrop — never a chopped slice of the
// background's own text (the strip of pane-preview fragments the issue shows
// bleeding past the modal's right edge, and the rail glyphs clipped mid-cell
// at its left). Rows the modal does not touch still render the background, so
// the frame keeps its faded context above and below.
func TestPlaceOverlayBlanksTheBackgroundBesideTheModal(t *testing.T) {
	fg := strings.Join([]string{"+---+", "|   |", "+---+"}, "\n")
	bg := backgroundFrame(20, 5)

	out := PlaceOverlay(0, 0, fg, bg, true)
	x, y, w, h := placement(fg, bg)
	composed := rasterize(out, 20)
	plainBg := rasterize(bg, 20)

	for row := range composed {
		onModalRow := row >= y && row < y+h
		for col := 0; col < 20; col++ {
			inModal := onModalRow && col >= x && col < x+w
			if inModal {
				continue
			}
			if onModalRow {
				if composed[row][col] != blankCell {
					t.Fatalf("a modal's row must be blank outside its rectangle — "+
						"row %d col %d shows %q (chopped background beside the box is #5297)",
						row, col, string(composed[row][col]))
				}
				continue
			}
			if composed[row][col] != plainBg[row][col] {
				t.Fatalf("a row the modal does not touch must keep its background — "+
					"row %d col %d: got %q, want %q",
					row, col, string(composed[row][col]), string(plainBg[row][col]))
			}
		}
	}
	// The modal itself is unchanged by the fill: it was already rectangular.
	modal := rasterize(fg, w)
	for row := 0; row < h; row++ {
		if string(composed[y+row][x:x+w]) != string(modal[row]) {
			t.Fatalf("rectangular overlay row %d changed: got %q, want %q",
				row, string(composed[y+row][x:x+w]), string(modal[row]))
		}
	}
}

// The backdrop a modal replaces can carry the closer for a style a row above
// opened — a pane preview mid-colour-run or a hyperlink still open. Dropping
// the row drops the reset too, and without one the style bleeds into the
// blank margins, an unstyled modal, and every row below (Codex review on
// #5297). The compositor must close backdrop state before the blank margin,
// then restore whatever is still open on the first backdrop row below the
// band — a closer on a dropped row is honoured, an unclosed span is not lost.
func TestPlaceOverlayClosesBackdropStateOnModalRows(t *testing.T) {
	tests := []struct {
		name     string
		bg       string
		wantRow1 string
		wantRow2 string // "{OPEN}" is the (faded) opener from row 0
	}{
		{
			name:     "sgr span closed on the dropped row",
			bg:       "\x1b[31mAAAA\nBBBB\x1b[0m\nCCCC",
			wantRow1: "\x1b[0mMM  ",
			wantRow2: "CCCC",
		},
		{
			// Codex's example: the span's reset sits BELOW the band, so the
			// row under the modal is still inside it.
			name:     "sgr span closed below the band",
			bg:       "\x1b[31mAAAA\nBBBB\nCCCC\x1b[0m",
			wantRow1: "\x1b[0mMM  ",
			wantRow2: "{OPEN}CCCC\x1b[0m",
		},
		{
			name:     "sgr span never closed",
			bg:       "\x1b[31mAAAA\nBBBB\nCCCC",
			wantRow1: "\x1b[0mMM  ",
			wantRow2: "{OPEN}CCCC",
		},
		{
			name:     "hyperlink closed on the dropped row",
			bg:       "\x1b]8;;https://x\x1b\\AAAA\nBBBB\x1b]8;;\x1b\\\nCCCC",
			wantRow1: "\x1b]8;;\x1b\\MM  ",
			wantRow2: "CCCC",
		},
		{
			name:     "hyperlink never closed",
			bg:       "\x1b]8;;https://x\x1b\\AAAA\nBBBB\nCCCC",
			wantRow1: "\x1b]8;;\x1b\\MM  ",
			wantRow2: "{OPEN}CCCC",
		},
	}
	for _, tc := range tests {
		out := strings.Split(PlaceOverlay(0, 1, "MM", tc.bg, false), "\n")
		if len(out) != 3 {
			t.Fatalf("%s: expected 3 rows, got %d: %q", tc.name, len(out), out)
		}
		if out[1] != tc.wantRow1 {
			t.Fatalf("%s: modal row must open with the backdrop closer — got %q, want %q",
				tc.name, out[1], tc.wantRow1)
		}
		// Row 0 is <opener>AAAA: everything before the text is the styling the
		// backdrop stream left open, already faded to the backdrop colours.
		opener := out[0][:strings.Index(out[0], "AAAA")]
		if want := strings.ReplaceAll(tc.wantRow2, "{OPEN}", opener); out[2] != want {
			t.Fatalf("%s: row below the band — got %q, want %q", tc.name, out[2], want)
		}
	}
}

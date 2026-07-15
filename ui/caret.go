package ui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// caretStyle draws the caret cell in reverse video, so it picks up the terminal's
// own foreground/background rather than a hardcoded colour and stays legible under
// every theme.
var caretStyle = lipgloss.NewStyle().Reverse(true)

// asciiCaret is the caret for terminals that render no SGR at all. A left half
// block needs no styling to be visible, and the TUI already assumes this glyph
// range elsewhere (the ● / ◌ / ○ / ◆ status icons).
const asciiCaret = "▌"

// InputCaret is the insertion point for the TUI's inline text inputs — the search
// query, the project-picker path, and the hooks-pane command editor. Callers append
// it after the styled text they have already rendered.
//
// It is a reverse-video cell rather than a literal "_" because an underscore is
// indistinguishable from a typed one, and it is STATIC: no blink, per the
// no-animation doctrine #1766 set for the status indicators. Either form is exactly
// one cell wide, so the caret never shifts the text it trails.
func InputCaret() string {
	// termenv drops EVERY sequence — reverse included — under the Ascii profile
	// (TERM=dumb, NO_COLOR), which would render the styled caret as a bare space:
	// an invisible insertion point. Degrade to a glyph that stands on its own.
	if lipgloss.ColorProfile() == termenv.Ascii {
		return asciiCaret
	}
	return caretStyle.Render(" ")
}

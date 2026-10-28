package ui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// caretStyle uses the contrast-checked primary/focus pair for its static cell.
var caretStyle = lipgloss.NewStyle().Foreground(activeTheme.Surface).Background(activeTheme.Accent)

// asciiCaret is the caret for terminals that render no SGR at all. A left half
// block needs no styling to be visible, and the TUI already assumes this glyph
// range elsewhere (the ● / ◌ / ○ / ◆ status icons).
const asciiCaret = "▌"

// InputCaret is the insertion point for the TUI's inline text inputs — the search
// query, the project-picker path, and the hooks-pane command editor. Callers append
// it after the styled text they have already rendered.
//
// It is a fixed-colour cell rather than a literal "_" because an underscore is
// indistinguishable from a typed one, and it is STATIC: no blink, per the
// no-animation doctrine #1766 set for the status indicators. Either form is exactly
// one cell wide, so the caret never shifts the text it trails.
func InputCaret() string {
	// termenv drops EVERY sequence — colour included — under the Ascii profile
	// (TERM=dumb, NO_COLOR), which would render the styled caret as a bare space:
	// an invisible insertion point. Degrade to a glyph that stands on its own.
	if lipgloss.ColorProfile() == termenv.Ascii {
		return asciiCaret
	}
	return caretStyle.Render(" ")
}

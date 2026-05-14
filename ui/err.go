package ui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// ansiEscapeRegex matches CSI escape sequences (e.g. SGR colors) so they can
// be stripped before width-based truncation. Truncating a string that still
// contains escape sequences risks cutting one mid-byte, leaking visible
// garbage like "[31m" into the rendered output (issue #525).
var ansiEscapeRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

type ErrBox struct {
	height, width int
	err           error
}

var errStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{
	Light: "#FF0000",
	Dark:  "#FF0000",
})

func NewErrBox() *ErrBox {
	return &ErrBox{}
}

func (e *ErrBox) SetError(err error) {
	e.err = err
}

func (e *ErrBox) Clear() {
	e.err = nil
}

func (e *ErrBox) SetSize(width, height int) {
	e.width = width
	e.height = height
}

func (e *ErrBox) String() string {
	if e.width <= 0 || e.height <= 0 {
		return ""
	}
	var err string
	if e.err != nil {
		err = e.err.Error()
		// Agent pane output can reach us via wrapped errors (see #502) and
		// carry ANSI escape sequences. Strip them so width math and the
		// final Truncate operate on plain text only.
		err = ansiEscapeRegex.ReplaceAllString(err, "")
		lines := strings.Split(err, "\n")
		err = strings.Join(lines, "//")
		if runewidth.StringWidth(err) > e.width {
			// Only add ellipsis when the string is long enough that the
			// truncated content plus "..." is shorter than the original.
			// Otherwise just hard-truncate to avoid losing more content
			// to the 3-char ellipsis than we save by truncating.
			tail := "..."
			tailWidth := runewidth.StringWidth(tail)
			if e.width < tailWidth {
				// Container is too narrow to fit "..."; drop the tail to
				// avoid overflowing past e.width (lipgloss.Place won't clip).
				tail = ""
			} else if runewidth.StringWidth(err) <= e.width+tailWidth {
				tail = ""
			}
			err = runewidth.Truncate(err, e.width, tail)
		}
	}
	return lipgloss.Place(e.width, e.height, lipgloss.Center, lipgloss.Center, errStyle.Render(err))
}

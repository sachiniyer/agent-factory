package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

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
		lines := strings.Split(err, "\n")
		err = strings.Join(lines, "//")
		if runewidth.StringWidth(err) > e.width {
			// Only add ellipsis when the string is long enough that the
			// truncated content plus "..." is shorter than the original.
			// Otherwise just hard-truncate to avoid losing more content
			// to the 3-char ellipsis than we save by truncating.
			tail := "..."
			if runewidth.StringWidth(err) <= e.width+runewidth.StringWidth(tail) {
				tail = ""
			}
			err = runewidth.Truncate(err, e.width, tail)
		}
	}
	return lipgloss.Place(e.width, e.height, lipgloss.Center, lipgloss.Center, errStyle.Render(err))
}

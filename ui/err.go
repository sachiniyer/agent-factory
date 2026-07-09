package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/sachiniyer/agent-factory/keys"
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

func (e *ErrBox) FullError() string {
	if e.err == nil {
		return ""
	}
	return sanitizeError(e.err.Error())
}

func (e *ErrBox) String() string {
	if e.width <= 0 || e.height <= 0 {
		return ""
	}
	var err string
	if e.err != nil {
		err = e.statusLine()
	}
	return lipgloss.Place(e.width, e.height, lipgloss.Center, lipgloss.Center, errStyle.Render(err))
}

func (e *ErrBox) statusLine() string {
	line := strings.Join(strings.Split(e.FullError(), "\n"), "//")
	if runewidth.StringWidth(line) <= e.width {
		return line
	}
	if hint := errorDetailsHint(); hint != "" {
		const sep = "  "
		hintWidth := runewidth.StringWidth(sep + hint)
		if prefixWidth := e.width - hintWidth; prefixWidth > 3 {
			return truncateStatusText(line, prefixWidth) + sep + hint
		}
	}
	return truncateStatusText(line, e.width)
}

func sanitizeError(raw string) string {
	// Agent pane output can reach us via wrapped errors (see #502) and carry
	// ANSI escape sequences — CSI (SGR colors, private-mode like \x1b[?25l)
	// and OSC (e.g. the OSC 8 hyperlink protocol from #565). Use xansi.Strip
	// so width math and the final truncate operate on plain text only — a
	// bespoke regex repeatedly missed variants (#525 → #552 → #565).
	clean := xansi.Strip(raw)
	// xansi.Strip handles ANSI escapes but leaves bare \r untouched. Hook
	// scripts commonly emit \r from progress indicators on stderr (see #668);
	// a \r reaching the terminal moves the cursor back to column 0, overwriting
	// lipgloss.Place's padding and corrupting the box.
	return strings.ReplaceAll(clean, "\r", "")
}

func truncateStatusText(text string, width int) string {
	// Only add ellipsis when the string is long enough that the truncated
	// content plus "..." is shorter than the original. Otherwise just
	// hard-truncate to avoid losing more content to the 3-char ellipsis than
	// we save by truncating.
	tail := "..."
	tailWidth := runewidth.StringWidth(tail)
	if width < tailWidth {
		// Container is too narrow to fit "..."; drop the tail to avoid
		// overflowing past width (lipgloss.Place won't clip).
		tail = ""
	} else if runewidth.StringWidth(text) <= width+tailWidth {
		tail = ""
	}
	return runewidth.Truncate(text, width, tail)
}

func errorDetailsHint() string {
	binding, ok := keys.GlobalKeyBindings[keys.KeyErrorDetails]
	if !ok {
		return ""
	}
	help := binding.Help()
	if help.Key == "" || help.Desc == "" {
		return ""
	}
	return help.Key + " " + help.Desc
}

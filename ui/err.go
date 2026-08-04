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
	// err is what the bar RENDERS. It goes nil when the notice's visual timer
	// expires (Expire) or when the action that raised it takes it back (Clear).
	err error
	// retained is the most recent notice, kept alive past its visual expiry so
	// `E details` can still open it (#2618). Retraction drops it as well: a
	// notice that stopped being TRUE must not stay reachable, while one that
	// merely stopped being VISIBLE must.
	//
	// Retaining it is the whole point of the affordance. `E` exists for a
	// message too long for the bar, and it used to go dead 3 seconds later — at
	// exactly the moment the clipped half left the screen — so the one case it
	// was built for was the one case it could not serve.
	retained error
	// retainedIsFailure records whether the retained notice reported a real
	// failure (handleError) or informational guidance (handleNotice / a success
	// message), which is what the details overlay titles itself from.
	retainedIsFailure bool
}

var errStyle = lipgloss.NewStyle().Foreground(activeTheme.Error)

func NewErrBox() *ErrBox {
	return &ErrBox{}
}

// SetError shows a real operation failure.
func (e *ErrBox) SetError(err error) { e.set(err, true) }

// SetNotice shows informational guidance: an action af deliberately declined or
// redirected, or something it did on the user's behalf. Same bar, different
// category — see retainedIsFailure.
func (e *ErrBox) SetNotice(err error) { e.set(err, false) }

func (e *ErrBox) set(err error, failure bool) {
	e.err = err
	if err == nil {
		return
	}
	e.retained = err
	e.retainedIsFailure = failure
}

// Expire stops rendering the notice when its visual timer runs out, leaving it
// readable through `E details`.
func (e *ErrBox) Expire() {
	e.err = nil
}

// Clear RETRACTS the notice: the caller is saying it is no longer true (a
// "Starting…" that finished, narrow-width guidance the resize resolved), so it
// leaves no trace behind for `E details` to reopen.
func (e *ErrBox) Clear() {
	e.err = nil
	e.retained = nil
	e.retainedIsFailure = false
}

func (e *ErrBox) SetSize(width, height int) {
	e.width = width
	e.height = height
}

// FullError is the notice currently ON the bar, sanitized. Empty once it
// expires or is retracted.
func (e *ErrBox) FullError() string {
	if e.err == nil {
		return ""
	}
	return sanitizeError(e.err.Error())
}

// RetainedNotice is the most recent notice that has not been retracted, whether
// or not the bar is still showing it, plus whether it reported a failure. This
// is what `E details` opens (#2618).
func (e *ErrBox) RetainedNotice() (text string, failure bool) {
	if e.retained == nil {
		return "", false
	}
	return sanitizeError(e.retained.Error()), e.retainedIsFailure
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
	line := strings.Join(strings.Split(e.FullError(), "\n"), " · ")
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
	// content plus "…" is shorter than the original. Otherwise just
	// hard-truncate to avoid losing more content to the ellipsis than
	// we save by truncating.
	tail := "…"
	tailWidth := runewidth.StringWidth(tail)
	if width < tailWidth {
		// Container is too narrow to fit "…"; drop the tail to avoid
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

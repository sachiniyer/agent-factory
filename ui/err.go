package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/theme"
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
	// detailsHint, when set, replaces the error_details binding's hint on a
	// clipped notice. A modal text field types `E` as a character, so it
	// advertises the key that does open the details from there (#4123).
	detailsHint string
	// hideDetailsHint drops the hint altogether, for a field where no key opens
	// the details: advertising one there would name an action that does nothing.
	hideDetailsHint bool
	// standing is a condition that stays true until its source retracts it
	// (SetStanding("")): it paints only while no notice is on the bar, never
	// lands in retained, and is not reachable through `E details`.
	standing string
}

var errStyle = lipgloss.NewStyle().Foreground(activeTheme.Dead)

func NewErrBox() *ErrBox {
	return &ErrBox{}
}

// SetError shows a real operation failure.
func (e *ErrBox) SetError(err error) { e.set(err, true) }

// SetNotice shows informational guidance: an action af deliberately declined or
// redirected, or something it did on the user's behalf. Same bar, different
// category — see retainedIsFailure.
func (e *ErrBox) SetNotice(err error) { e.set(err, false) }

// SetHint shows an ephemeral acknowledgment that `E details` must not be able
// to reopen: it paints on the same bar and fades on the same Expire timer, but
// it never lands in retained. That is the difference between answering a key
// and creating a notice — "the store is empty" is itself not a notice, so a
// second `E` after the hint expires must find the store still empty rather
// than opening "Last notice" on the hint's own text (#5351 review).
func (e *ErrBox) SetHint(err error) { e.err = err }

// SetStanding shows a persistent one-line condition on the bar whenever no
// notice is up, until it is set to "". Used for a state that is true now and
// stays true (the daemon's web listener is unbound, #6386) rather than for
// the outcome of an action.
func (e *ErrBox) SetStanding(text string) { e.standing = sanitizeError(text) }

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

// SetDetailsHint overrides the hint a clipped notice carries; "" restores the
// error_details binding's own hint.
func (e *ErrBox) SetDetailsHint(hint string) {
	e.detailsHint = hint
}

// HideDetailsHint drops the hint from a clipped notice while hide is set.
func (e *ErrBox) HideDetailsHint(hide bool) {
	e.hideDetailsHint = hide
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
	switch {
	case e.err != nil:
		err = e.statusLine()
	case e.standing != "":
		err = e.standingLine()
	}
	style := theme.Styles().Body
	if e.retainedIsFailure {
		style = theme.Styles().Error
	}
	return lipgloss.Place(e.width, e.height, lipgloss.Center, lipgloss.Center, style.Render(err))
}

func (e *ErrBox) standingLine() string {
	if layout.Cells(e.standing) <= e.width {
		return e.standing
	}
	return truncateStatusText(e.standing, e.width)
}

func (e *ErrBox) statusLine() string {
	line := strings.Join(strings.Split(e.FullError(), "\n"), " · ")
	// Measure on the contract measure the status bar's ClampToRect enforces,
	// not on runewidth: the two under-count what tmux advances on the ordinary
	// emoji classes (heart+VS16, keycap, regional indicators) and on
	// lipgloss-expanded tabs, so a line this test accepts can still be wider
	// than the bar to the clamp, which then silently right-cuts it — taking the
	// trailing content and the `E details` hint itself with no ellipsis.
	// layout.Cells is "the ONE width answer for the TUI" (see layout/cells.go).
	if layout.Cells(line) <= e.width {
		return line
	}
	hint := e.detailsHint
	if hint == "" {
		hint = errorDetailsHint()
	}
	if hint != "" && !e.hideDetailsHint {
		const sep = "  "
		hintWidth := layout.Cells(sep + hint)
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
	// Strip escape sequences first so OSC terminators still delimit payloads.
	// Then remove bare C0, DEL, and UTF-8 C1 controls that can move the cursor
	// or trigger terminal effects. Newlines become the same separator used by
	// the status bar so recovery details occupy one line.
	clean = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' || r >= 0x7f && r <= 0x9f {
			return -1
		}
		return r
	}, clean)
	// Tabs are expanded to four spaces — what lipgloss's default TabWidth
	// renders — so the width budget and final truncate count what the
	// terminal actually draws. A kept tab is 0 cells to every measure, so it
	// passes the fit test only for the status bar's clamp to find the 4-cell
	// expansion lipgloss emits and silently cut the trailing content (the
	// same hard-cut shape #3614 took the contract measure to close, by way of
	// a different cause). Expanding here makes the budget match the render.
	clean = strings.ReplaceAll(clean, "\t", "    ")
	return strings.ReplaceAll(clean, "\n", " · ")
}

func truncateStatusText(text string, width int) string {
	// Only add ellipsis when the string is long enough that the truncated
	// content plus "…" is shorter than the original. Otherwise just
	// hard-truncate to avoid losing more content to the ellipsis than
	// we save by truncating.
	//
	// All width decisions are on layout.Cells (the contract measure
	// ClampToRect enforces); xansi.Truncate cuts on the same measure and
	// reserves the tail's width inside the budget it is given. The two are
	// the ONE width answer for the TUI, so a result that fits here fits the
	// bar's clamp without the silent right-cut runewidth's under-reporting
	// used to leave behind (the `E details` hint itself could be cut to
	// `E deta` on a heart+VS16 notice the fit test accepted).
	if width <= 0 {
		return ""
	}
	if layout.Cells(text) <= width {
		return text
	}
	tail := "…"
	if width < layout.Cells(tail) {
		// Container is too narrow to fit "…"; drop the tail to avoid
		// overflowing past width (lipgloss.Place won't clip).
		tail = ""
	} else if layout.Cells(text) <= width+layout.Cells(tail) {
		// Truncating would hide at most as many cells as the tail costs,
		// so keep the content and skip the ellipsis marker.
		tail = ""
	}
	// xansi.Truncate cuts by its own grapheme-cluster count, which the
	// Cells measure can call over-wide after the fact: the keycap grapheme
	// is the one in this fix's corpus — its leading ASCII character is
	// printed before its modifier continuation, so the truncator counts
	// one cell, keeps the whole 2-cell cluster, and returns it at a width
	// of one. Ask the truncator for progressively less until the result
	// actually fits the budget on the contract measure; this is the same
	// progressive loop layout.TruncateToCells and truncateToContract use
	// when the truncator and the measure disagree.
	for w := width; w >= 0; w-- {
		out := xansi.Truncate(text, w, tail)
		if layout.Cells(out) <= width {
			return out
		}
	}
	return ""
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

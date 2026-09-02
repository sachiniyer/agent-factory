package overlay

import (
	"regexp"
	"strconv"
	"strings"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/ansi"
	"github.com/muesli/reflow/truncate"
	"github.com/muesli/termenv"
)

// Most of this code is modified from https://github.com/charmbracelet/lipgloss/pull/102

// Faded gray tones used by the overlay fade effect.
const (
	fadedFg = "38;5;240" // Medium gray foreground
	fadedBg = "48;5;236" // Dark gray background
)

// sgrRegex matches any SGR (Select Graphic Rendition) sequence so the fade
// pass can parse its parameters as a whole. A single pass over the full
// sequence is required to correctly handle combined FG+BG sequences such as
// \x1b[38;5;232;48;5;189m, which earlier per-color regexes mishandled by
// dropping the foreground portion (#701).
var sgrRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// extendedColorLen returns the number of tokens an extended-color introducer
// (38 or 48) spans, given tokens[i] is the introducer. 38;5;n / 48;5;n span 3
// tokens; 38;2;r;g;b / 48;2;r;g;b span 5. Consuming these together is what
// prevents the inner parameters (e.g. the "5" in 48;5;189) from being
// misread as a standalone attribute such as blink.
func extendedColorLen(tokens []string, i int) int {
	if i+1 < len(tokens) {
		switch tokens[i+1] {
		case "5":
			return 3
		case "2":
			return 5
		}
	}
	return 1
}

// fadeSGR rewrites a single SGR sequence to its faded equivalent. It detects
// whether the sequence sets a foreground and/or background color (or any other
// fadeable attribute) and emits faded gray codes for whichever are present,
// combining both into one sequence when the input was combined. Pure resets
// (\x1b[0m, \x1b[m) and sequences with no fadeable parameters are preserved
// unchanged so styled regions still close correctly. Default-color resets
// (SGR 39 foreground, 49 background) are preserved verbatim rather than faded,
// so a region that returns to the terminal default stays default instead of
// gaining a spurious gray (#728).
func fadeSGR(match string) string {
	params := strings.TrimSuffix(strings.TrimPrefix(match, "\x1b["), "m")
	if params == "" || params == "0" {
		return match
	}

	tokens := strings.Split(params, ";")
	hasFg, hasBg, hasOtherFadeable := false, false, false
	fgReset, bgReset := false, false
	for i := 0; i < len(tokens); {
		code, err := strconv.Atoi(tokens[i])
		if err != nil || code == 0 {
			i++
			continue
		}
		switch {
		case code == 38: // extended foreground (38;5;n or 38;2;r;g;b)
			hasFg = true
			i += extendedColorLen(tokens, i)
		case code == 48: // extended background (48;5;n or 48;2;r;g;b)
			hasBg = true
			i += extendedColorLen(tokens, i)
		case code == 39: // reset foreground to default
			// An explicit reset, NOT a color: the region wants the terminal
			// default foreground, so it must be preserved verbatim rather than
			// substituted with the faded gray (or, before #728, mis-folded to a
			// faded foreground via the default branch).
			fgReset = true
			i++
		case code == 49: // reset background to default
			// An explicit reset, NOT a color. Preserve it so a default-bg region
			// stays default; before #728 this fell into the default branch and
			// wrongly emitted a faded *foreground* instead.
			bgReset = true
			i++
		case (code >= 30 && code <= 37) || (code >= 90 && code <= 97):
			hasFg = true // basic/bright foreground
			i++
		case code == 7 || (code >= 40 && code <= 47) || (code >= 100 && code <= 107):
			hasBg = true // reverse video or basic/bright background
			i++
		default:
			// Non-color attribute (bold, italic, …). Tracked separately so it
			// doesn't inject a foreground gray when a real background color is
			// present (preserving the bg-only fade of e.g. \x1b[1;41m).
			hasOtherFadeable = true
			i++
		}
	}

	if !hasFg && !hasBg && !fgReset && !bgReset {
		// Attribute-only sequence (e.g. bold \x1b[1m): fold to foreground gray,
		// matching the long-standing behavior for such 16-color sequences.
		if hasOtherFadeable {
			return "\x1b[" + fadedFg + "m"
		}
		return match
	}
	parts := make([]string, 0, 2)
	// A real color in a channel wins over a reset in the same sequence: it is
	// what the region ends up showing, so emit the faded substitute. Otherwise
	// a bare reset (39/49) is preserved so the channel returns to default.
	switch {
	case hasFg:
		parts = append(parts, fadedFg)
	case fgReset:
		parts = append(parts, "39")
	}
	switch {
	case hasBg:
		parts = append(parts, fadedBg)
	case bgReset:
		parts = append(parts, "49")
	}
	return "\x1b[" + strings.Join(parts, ";") + "m"
}

// widestLine measures the widest of these rows with the same measure getLines
// uses, so a re-measure after clipping cannot disagree with the original.
func widestLine(lines []string) int {
	widest := 0
	for _, l := range lines {
		if w := ansi.PrintableRuneWidth(l); w > widest {
			widest = w
		}
	}
	return widest
}

// osc8Regex matches an OSC 8 hyperlink introducer with either terminator, ST
// (\x1b\\) or BEL. The capture is the URI: a hyperlink is OPENED by a sequence
// carrying one and CLOSED by the same sequence with it empty.
var osc8Regex = regexp.MustCompile("\x1b\\]8;[^;]*;([^\x1b\a]*)(?:\x1b\\\\|\a)")

// closeSequences returns what must be emitted after line so that whatever follows
// it — a background segment, or the end of the frame — starts clean.
//
// CLOSING only, deliberately, and this is the whole reason the compositor does not
// carry foreground state across rows. Closing needs one question answered ("did
// this row leave anything open?"); REOPENING needs the cumulative state
// reconstructed, which means an ANSI state machine: multi-sequence SGR
// accumulation, the synthetic reset the truncator inserts at a clip, and OSC 8 on
// top. Every modal this tree renders goes through lipgloss, which closes its
// styling on every row — measured, all rows — so nothing af draws depends on
// continuation, and paying for a state machine to preserve it would buy a
// hypothetical at the price of three real failure modes.
//
// A hand-built foreground that does span a style across rows loses that
// continuation. It never rendered correctly here anyway: before this change the
// open style ran straight into the background cells the compositor writes after
// every row.
func closeSequences(line string) string {
	var closers string
	if matches := sgrRegex.FindAllString(line, -1); len(matches) > 0 {
		if last := matches[len(matches)-1]; last != "\x1b[0m" && last != "\x1b[m" {
			closers += "\x1b[0m"
		}
	}
	if matches := osc8Regex.FindAllStringSubmatch(line, -1); len(matches) > 0 {
		if last := matches[len(matches)-1]; last[1] != "" {
			// A hyperlink opened and not closed on this row would otherwise keep
			// every background cell after it clickable, and outlive the frame.
			closers += "\x1b]8;;\x1b\\"
		}
	}
	return closers
}

// Split a string into lines, additionally returning the size of the widest
// line.
//
// The measure here is ansi.PrintableRuneWidth, and it is deliberately left alone
// (#3433). Three width functions are in use in this tree and they disagree; for
// one joined-emoji family ("\U0001F468\u200D\U0001F469\u200D\U0001F467\u200D\U0001F466",
// four emoji and three ZWJs) they were measured at:
//
//	lipgloss.Width / xansi.StringWidth / runewidth.StringWidth   2 cells
//	ansi.PrintableRuneWidth (this function)                      8 cells
//	tmux 3.4, actually advancing the cursor                      4 cells
//
// The obvious repair — give the compositor the same measure the panes size
// themselves with — is NOT obviously right, and the tmux number is why. The
// compositor's job is to count the cells the terminal will advance, and af renders
// into tmux; swapping 8 for 2 would trade an overestimate of 4 for an
// underestimate of 2, still wrong and now wrong in the direction that writes past
// the frame instead of stopping short of it. Picking correctly needs the same
// measurement across the emulators af supports, which is its own change.
//
// What PlaceOverlay no longer does is turn that disagreement into a blank screen.
func getLines(s string) (lines []string, widest int) {
	lines = strings.Split(s, "\n")

	for _, l := range lines {
		w := ansi.PrintableRuneWidth(l)
		if widest < w {
			widest = w
		}
	}

	return lines, widest
}

func CalculateCenterCoordinates(foregroundLines []string, backgroundLines []string, foregroundWidth, backgroundWidth int) (int, int) {
	// Calculate the x-coordinate to horizontally center the foreground text.
	x := (backgroundWidth - foregroundWidth) / 2

	// Calculate the y-coordinate to vertically center the foreground text.
	y := (len(backgroundLines) - len(foregroundLines)) / 2

	return x, y
}

// PlaceOverlay places fg on top of bg.
// If center is true, the foreground is centered on the background; otherwise, the provided x and y are used.
func PlaceOverlay(
	x, y int,
	fg, bg string,
	center bool,
) string {
	fgLines, fgWidth := getLines(fg)
	bgLines, bgWidth := getLines(bg)
	bgHeight := len(bgLines)
	fgHeight := len(fgLines)

	// A modal bigger than the frame is CLIPPED, never dropped (#3433).
	//
	// This used to `return fg`, discarding the entire background — so a modal that
	// merely MEASURED too wide blanked the whole TUI, silently, and only on the
	// frames where the offending text was on screen. It is reachable without any
	// modal actually being oversized, because the measures disagree: panes size and
	// truncate themselves with lipgloss.Width, this compositor counts with
	// ansi.PrintableRuneWidth, and for one joined-emoji family those read 2 cells
	// and 8. A pane can therefore be certain it fits while the compositor is
	// certain it does not.
	//
	// Clipping is the right failure whichever measure is eventually agreed on: a
	// modal one cell too wide loses a cell, which is visible and bounded, instead
	// of costing the user everything behind it. Deliberately NOT a change of
	// measure — see the note on getLines for why that needs its own decision.
	if fgHeight > bgHeight {
		fgLines = fgLines[:bgHeight]
		fgHeight = bgHeight
		// RE-MEASURE. fgWidth was the widest row of the whole modal, and the widest
		// row may be one of the ones just discarded. Keeping the stale value makes
		// the width branch below fire on rows that were never too wide, padding
		// every retained row across the frame and dragging a centered modal to
		// column zero.
		fgWidth = widestLine(fgLines)
	}
	if fgWidth > bgWidth {
		for i, line := range fgLines {
			// truncate accumulates runewidth per printable rune and skips escape
			// sequences — the same accounting getLines and the placement arithmetic
			// below use — so the clipped rows really are within bgWidth by THIS
			// compositor's measure, and it closes any SGR it cuts through rather
			// than leaking the style onto the frame.
			fgLines[i] = truncate.String(line, uint(bgWidth))
		}
		fgWidth = bgWidth
	}
	// An overlay is OPAQUE: every cell inside its rectangle belongs to it,
	// including the blank tail of a row narrower than the widest one. Only the
	// row's own width used to be written, so the compositor filled the rest of
	// the rectangle from the layer below and the pane behind showed through
	// mid-modal (#2149). Padding here — rather than at each caller — makes the
	// guarantee a property of compositing, so it holds for whatever a modal
	// happens to render. lipgloss-framed modals already pad their rows, so this
	// is a no-op for them and only catches the ragged ones.
	for i, line := range fgLines {
		if w := ansi.PrintableRuneWidth(line); w < fgWidth {
			fgLines[i] = line + strings.Repeat(" ", fgWidth-w)
		}
	}

	// Apply a fade effect to the background by directly modifying each line
	fadedBgLines := make([]string, len(bgLines))

	for i, line := range bgLines {
		// Fade every SGR sequence on the line in a single pass. Parsing each
		// sequence whole lets combined FG+BG codes keep both colors (#701) while
		// still graying standalone FG-only, BG-only, and 16-color/attribute
		// sequences.
		fadedBgLines[i] = sgrRegex.ReplaceAllStringFunc(line, fadeSGR)
	}

	// Replace the original background with the faded version
	bgLines = fadedBgLines

	// Determine placement coordinates
	placeX, placeY := x, y
	if center {
		placeX, placeY = CalculateCenterCoordinates(fgLines, bgLines, fgWidth, bgWidth)
	}

	// Clamp coordinates to ensure foreground fits within background
	placeX = clamp(placeX, 0, bgWidth-fgWidth)
	placeY = clamp(placeY, 0, bgHeight-fgHeight)

	ws := &whitespace{}

	// Build the output string.
	//
	// The compositor writes background cells after the foreground on every row, so
	// a foreground row that leaves styling open would colour that row's background
	// tail — and, at the end of the frame, everything after it. Each row therefore
	// closes what it opened before the background is written. See closeSequences
	// for why this closes rather than carries.
	var b strings.Builder
	for i, bgLine := range bgLines {
		if i > 0 {
			b.WriteByte('\n')
		}
		if i < placeY || i >= placeY+fgHeight {
			b.WriteString(bgLine)
			continue
		}

		pos := 0
		if placeX > 0 {
			left := truncate.String(bgLine, uint(placeX))
			pos = ansi.PrintableRuneWidth(left)
			b.WriteString(left)
			if pos < placeX {
				b.WriteString(ws.render(placeX - pos))
				pos = placeX
			}
		}

		fgLine := fgLines[i-placeY]
		b.WriteString(fgLine)
		pos += ansi.PrintableRuneWidth(fgLine)
		b.WriteString(closeSequences(fgLine))

		right := xansi.TruncateLeft(bgLine, pos, "")
		bgLineWidth := ansi.PrintableRuneWidth(bgLine)
		rightWidth := ansi.PrintableRuneWidth(right)
		remainingWidth := bgLineWidth - pos
		if rightWidth > remainingWidth {
			// TruncateLeft returned more than fits because pos landed in the
			// middle of a wide (CJK/emoji) grapheme and the whole cluster was
			// preserved. Re-truncate from the right with the ANSI-aware helper
			// so we don't render past bgLineWidth. The dropped half-cell shows
			// as the leading pad below. (#647)
			right = xansi.Truncate(right, remainingWidth, "")
			rightWidth = ansi.PrintableRuneWidth(right)
		}
		if rightWidth < remainingWidth {
			b.WriteString(ws.render(remainingWidth - rightWidth))
		}
		b.WriteString(right)
	}

	return b.String()
}

func clamp(v, lower, upper int) int {
	return min(max(v, lower), upper)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type whitespace struct {
	style termenv.Style
	chars string
}

// Render whitespaces.
func (w whitespace) render(width int) string {
	if w.chars == "" {
		w.chars = " "
	}

	r := []rune(w.chars)
	j := 0
	b := strings.Builder{}

	// Cycle through runes and print them into the whitespace.
	for i := 0; i < width; {
		writtenRune := r[j]
		b.WriteRune(writtenRune)
		j++
		if j >= len(r) {
			j = 0
		}
		i += ansi.PrintableRuneWidth(string(writtenRune))
	}

	// Fill any extra gaps white spaces. This might be necessary if any runes
	// are more than one cell wide, which could leave a one-rune gap.
	short := width - ansi.PrintableRuneWidth(b.String())
	if short > 0 {
		b.WriteString(strings.Repeat(" ", short))
	}

	return w.style.Styled(b.String())
}

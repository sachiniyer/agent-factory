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
// unchanged so styled regions still close correctly.
func fadeSGR(match string) string {
	params := strings.TrimSuffix(strings.TrimPrefix(match, "\x1b["), "m")
	if params == "" || params == "0" {
		return match
	}

	tokens := strings.Split(params, ";")
	hasFg, hasBg, hasOtherFadeable := false, false, false
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

	if !hasFg && !hasBg {
		// Attribute-only sequence (e.g. bold \x1b[1m): fold to foreground gray,
		// matching the long-standing behavior for such 16-color sequences.
		if hasOtherFadeable {
			return "\x1b[" + fadedFg + "m"
		}
		return match
	}
	parts := make([]string, 0, 2)
	if hasFg {
		parts = append(parts, fadedFg)
	}
	if hasBg {
		parts = append(parts, fadedBg)
	}
	return "\x1b[" + strings.Join(parts, ";") + "m"
}

// WhitespaceOption sets a styling rule for rendering whitespace.
type WhitespaceOption func(*whitespace)

// Split a string into lines, additionally returning the size of the widest
// line.
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
	opts ...WhitespaceOption,
) string {
	fgLines, fgWidth := getLines(fg)
	bgLines, bgWidth := getLines(bg)
	bgHeight := len(bgLines)
	fgHeight := len(fgLines)

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

	// Check if foreground exceeds background size
	if fgWidth > bgWidth || fgHeight > bgHeight {
		return fg // Return foreground if it's larger than background
	}

	// Clamp coordinates to ensure foreground fits within background
	placeX = clamp(placeX, 0, bgWidth-fgWidth)
	placeY = clamp(placeY, 0, bgHeight-fgHeight)

	// Apply whitespace options
	ws := &whitespace{}
	for _, opt := range opts {
		opt(ws)
	}

	// Build the output string
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

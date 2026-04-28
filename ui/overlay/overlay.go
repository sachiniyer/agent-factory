package overlay

import (
	"regexp"
	"strings"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/ansi"
	"github.com/muesli/reflow/truncate"
	"github.com/muesli/termenv"
)

// Most of this code is modified from https://github.com/charmbracelet/lipgloss/pull/102

// Pre-compiled regexes for ANSI color code replacement in overlay fade effect.
var (
	bgColorRegex     = regexp.MustCompile(`\x1b\[(?:[0-9;]*;)?48;[25];[0-9;]+m`)
	fgColorRegex     = regexp.MustCompile(`\x1b\[(?:[0-9;]*;)?38;[25];[0-9;]+m`)
	simpleColorRegex = regexp.MustCompile(`\x1b\[[0-9]+m`)
)

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
		// Replace background color codes with a faded version
		content := bgColorRegex.ReplaceAllString(line, "\x1b[48;5;236m") // Dark gray background

		// Replace foreground color codes with a faded version
		content = fgColorRegex.ReplaceAllString(content, "\x1b[38;5;240m") // Medium gray foreground

		// Replace simple color codes with a faded version
		content = simpleColorRegex.ReplaceAllStringFunc(content, func(match string) string {
			// Skip reset codes
			if match == "\x1b[0m" {
				return match
			}
			// Replace with dimmed color
			return "\x1b[38;5;240m" // Medium gray
		})

		fadedBgLines[i] = content
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
		if rightWidth <= bgLineWidth-pos {
			b.WriteString(ws.render(bgLineWidth - rightWidth - pos))
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

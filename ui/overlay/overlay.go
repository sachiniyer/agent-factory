package overlay

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/ui/theme"

	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// Most of this code is modified from https://github.com/charmbracelet/lipgloss/pull/102

// Backdrop colours use existing roles, resolved once per overlay frame.
func backdropColors() (string, string) {
	c := theme.Roles()
	profile := lipgloss.ColorProfile()
	if profile == termenv.Ascii {
		return "39", "49"
	}
	return profile.FromColor(c.InkMuted).Sequence(false), profile.FromColor(c.Surface).Sequence(true)
}

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

func fadeSGRColors(match, fadedFg, fadedBg string) string {
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

// lineWidth is the compositor's width measure. It is layout.Cells — the ONE width
// answer shared with the panes and with app's overlayOrigin (#3585), so the
// compositor can no longer disagree with the code that decides where a modal goes
// or where its mouse zones are registered. See layout.Cells for the tmux
// measurements that chose it.
func lineWidth(s string) int {
	return layout.Cells(s)
}

// clipLine truncates one foreground row to width. layout.TruncateToCells is the
// shared implementation — the compositor and the pane clamp had the same
// measure-versus-truncator problem and now have one answer to it (#3585).
func clipLine(line string, width int) string {
	return layout.TruncateToCells(line, width)
}

// widestLine measures the widest of these rows with the same measure getLines
// uses, so a re-measure after clipping cannot disagree with the original.
func widestLine(lines []string) int {
	widest := 0
	for _, l := range lines {
		if w := lineWidth(l); w > widest {
			widest = w
		}
	}
	return widest
}

// osc8Stops holds every character that ends an OSC string in a terminal — BEL,
// the C1 ST (U+009C, two bytes in UTF-8), ESC, and the cancellation bytes CAN
// (0x18) and SUB (0x1A). The parameter field stops at all of them exactly as
// the URI field does; a class that admitted them (say [^;]*) would let a
// sequence ended mid-parameter cross the byte that ended it and manufacture an
// opener the terminal never made — "\x1b]8;foo\x1b[31m;FAKE\x1b[0m" ended at
// the first ESC, or "\x1b]8;foo\x18bar;u\x1b\\" cancelled at the CAN.
const osc8Stops = "\x07\x18\x1a\x1b\u009c"

// osc8Seq is one terminated OSC 8 sequence found in a row. uri is the field
// after the second ';': nonempty OPENS a hyperlink, empty CLOSES one. raw is
// the sequence byte-for-byte, introducer through terminator — the form carried
// forward when a link outlives a modal.
type osc8Seq struct {
	raw string
	uri string
}

// scanOSC8 reads OSC 8 hyperlink sequences exactly as a terminal does, where a
// non-overlapping regex match cannot:
//
//   - ESC ends an OSC wherever it lands — mid-parameter field included — and
//     is reconsidered as the introducer of the NEXT escape. A bare ESC shared
//     by two sequences ("\x1b]8;;\x1b]8;;u\x1b\\" — a closer whose terminator
//     opens the next one) is rescanned here, where FindAllStringSubmatch
//     consumes it and misses the second sequence. The bare-ESC form itself is
//     a fourth terminator the x/ansi parser discounts but the old regex never
//     matched, so "\x1b]8;;u\x1b[31m" kept the whole surface clickable (#7560).
//   - CAN and SUB end the string as an ABORT — the command is cancelled, no
//     link ever opens, and nothing may be carried. They stop the fields just
//     like the true terminators.
//   - A sequence a terminal can't complete — the row ends mid-OSC, or the ';'
//     separator never arrives — yields no event at all. One gap is shared
//     with the parser the width measure uses: the C1 OSC INTRODUCER (U+009D),
//     which xansi.Strip does not handle either, is not an introducer here.
//
// Its coverage of the four real terminators is PINNED to the parser's by a
// test rather than left to drift: a terminator the parser discounts in the
// width but this scan does not recognise is a link the compositor never closes.
func scanOSC8(line string) []osc8Seq {
	var seqs []osc8Seq
	pos := 0
	for {
		off := strings.Index(line[pos:], "\x1b]8;")
		if off < 0 {
			return seqs
		}
		start := pos + off
		p := start + len("\x1b]8;")

		// Parameter field, up to the ';' that introduces the URI — or to any
		// character that ends the string first, which ends the sequence
		// unreadable (a true terminator means the ';' never arrived; CAN/SUB
		// abort it).
		off = strings.IndexAny(line[p:], ";"+osc8Stops)
		if off < 0 {
			return seqs
		}
		p += off
		if line[p] != ';' {
			_, size := utf8.DecodeRuneInString(line[p:])
			pos = p + size
			if line[p] == '\x1b' {
				pos = p // the terminating ESC may introduce the next sequence
			}
			continue
		}

		// URI field, up to the character that ends the string.
		p++
		uri := p
		off = strings.IndexAny(line[p:], osc8Stops)
		if off < 0 {
			return seqs
		}
		p += off
		r, size := utf8.DecodeRuneInString(line[p:])
		switch r {
		case '\x18', '\x1a':
			pos = p + size // CAN/SUB abort: no link opened, nothing carries
			continue
		case '\x1b':
			pos = p // bare ESC or ST: reconsider the ESC for the next sequence
		default:
			pos = p + size
		}
		end := p + size
		if r == '\x1b' && p+1 < len(line) && line[p+1] == '\\' {
			end = p + 2 // ST consumes the backslash with the ESC
		}
		seqs = append(seqs, osc8Seq{raw: line[start:end], uri: line[uri:p]})
	}
}

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
	if seqs := scanOSC8(line); len(seqs) > 0 {
		if last := seqs[len(seqs)-1]; last.uri != "" {
			// A hyperlink opened and not closed on this row would otherwise keep
			// every background cell after it clickable, and outlive the frame.
			closers += "\x1b]8;;\x1b\\"
		}
	}
	return closers
}

// backdropOpen tracks what the backdrop stream leaves open after line: the
// SGR openers since the last full reset (so the state can be re-emitted, not
// just detected) and the OSC 8 introducer of the open link, if any. A closer
// clears its channel; a row with no sequences inherits the carry — exactly
// how the terminal interprets the stream. The carry matters because the
// backdrop CAN leave styling open: a pane preview holds whatever its PTY
// emitted, and a curses app may run one colour across a whole screenful of
// rows.
func backdropOpen(line string, sgrOpen []string, osc8Open string) ([]string, string) {
	for _, m := range sgrRegex.FindAllString(line, -1) {
		if m == "\x1b[0m" || m == "\x1b[m" {
			sgrOpen = sgrOpen[:0]
		} else {
			sgrOpen = append(sgrOpen, m)
		}
	}
	if seqs := scanOSC8(line); len(seqs) > 0 {
		if last := seqs[len(seqs)-1]; last.uri != "" {
			// The carry is re-emitted verbatim below the modal, so it must end
			// in a real terminator. A bare-ESC match ends in a dangling ESC a
			// terminal would dispatch as the next escape's start — eating the
			// first byte of the row it precedes. Normalise by appending the
			// backslash that turns it into ST, keeping the parameter field
			// (the id= that associates cells with the link) intact rather
			// than rebuilding from the URI alone.
			osc8Open = last.raw
			if strings.HasSuffix(osc8Open, "\x1b") {
				osc8Open += "\\"
			}
		} else {
			osc8Open = ""
		}
	}
	return sgrOpen, osc8Open
}

// Split a string into lines, additionally returning the size of the widest
// line.
//
// The measure here is layout.Cells, the ONE width answer shared with the panes and
// with app's overlayOrigin (#3585). It was ansi.PrintableRuneWidth alone until
// then, and the disagreement that caused is why #3433 exists: Three width functions are in use in this tree and they disagree; for
// one joined-emoji family ("\U0001F468\u200D\U0001F469\u200D\U0001F467\u200D\U0001F466",
// four emoji and three ZWJs) they were measured at:
//
//	lipgloss.Width / xansi.StringWidth / runewidth.StringWidth   2 cells
//	ansi.PrintableRuneWidth (this function)                      8 cells
//	tmux 3.4, actually advancing the cursor                      4 cells
//
// The obvious repair — adopt the measure the panes used — was not obviously right
// either, and the tmux row is why: swapping 8 for 2 trades an overestimate for an
// UNDERestimate, which writes past the frame instead of stopping short of it.
// layout.Cells resolves it by never reporting fewer cells than tmux advances; its
// doc carries the full corpus those numbers come from.
func getLines(s string) (lines []string, widest int) {
	lines = strings.Split(s, "\n")

	for _, l := range lines {
		w := lineWidth(l)
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
	// truncated themselves with lipgloss.Width while this compositor counted with
	// ansi.PrintableRuneWidth — for one joined-emoji family, 2 cells against 8 — so
	// a pane could be certain it fitted while the compositor was certain it did
	// not. Both sides now measure with layout.Cells (#3585), but the clip stays:
	// it is what makes an over-wide modal cost a few cells instead of the frame.
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
			fgLines[i] = clipLine(line, bgWidth)
		}
		// An OSC row can still measure over the frame here, because the measure
		// over-counts it rather than because it is wide (see clipLine). Cap the
		// placement width so the clamp below stays inside the frame either way.
		if fgWidth = widestLine(fgLines); fgWidth > bgWidth {
			fgWidth = bgWidth
		}
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
		if w := lineWidth(line); w < fgWidth {
			fgLines[i] = line + strings.Repeat(" ", fgWidth-w)
		}
	}

	// Apply a fade effect to the background by directly modifying each line
	fadedBgLines := make([]string, len(bgLines))
	fgColor, bgColor := backdropColors()

	for i, line := range bgLines {
		// Fade every SGR sequence on the line in a single pass. Parsing each
		// sequence whole lets combined FG+BG codes keep both colors (#701) while
		// still graying standalone FG-only, BG-only, and 16-color/attribute
		// sequences.
		fadedBgLines[i] = sgrRegex.ReplaceAllStringFunc(line, func(s string) string { return fadeSGRColors(s, fgColor, bgColor) })
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
	// On a row the modal occupies, the cells either side of it are BLANK
	// backdrop — never the background's own cells (#5297). Writing the
	// background there chopped it at the modal's edges: the left margin showed
	// rail glyphs clipped mid-row (▾ beta╭, ⎇-de│), and the right margin showed
	// the tail ends of whatever the pane preview had behind the modal — a strip
	// of fragments that read as layout garbage, especially at 80 columns where
	// a centered modal leaves only ~14 columns of it. Rows the modal does not
	// touch still render the faded backdrop, so the frame keeps its context;
	// the modal just never shares a row with background content.
	//
	// A foreground row that leaves styling open would colour whatever follows
	// it — the blank margin, and at the end of the frame everything after — so
	// each row closes what it opened first. See closeSequences for why this
	// closes rather than carries.
	var b strings.Builder
	var bgSgrOpen []string
	var bgOsc8Open string
	reopenPending := false
	for i, bgLine := range bgLines {
		if i > 0 {
			b.WriteByte('\n')
		}
		if i < placeY || i >= placeY+fgHeight {
			if reopenPending {
				// The band's blank rows emitted closers for whatever the
				// backdrop had open, but that state was still live in the
				// original stream — a colour run or hyperlink whose closer
				// sits below the modal. Re-emit the openers so rows under the
				// band keep the styling they would have had (Codex review on
				// #5297). Sequences a dropped row closed are already gone
				// from the carry, so a closer inside the band is honoured.
				for _, s := range bgSgrOpen {
					b.WriteString(s)
				}
				if bgOsc8Open != "" {
					b.WriteString(bgOsc8Open)
				}
				reopenPending = false
			}
			b.WriteString(bgLine)
			bgSgrOpen, bgOsc8Open = backdropOpen(bgLine, bgSgrOpen, bgOsc8Open)
			continue
		}

		// The dropped background row can carry the closer for a style a row
		// above the modal opened — a pane preview mid-colour-run, a hyperlink
		// left open. Without it that state bleeds into the blank margins, an
		// unstyled modal, and every row below (Codex review on #5297). Close
		// whatever the emitted stream has open BEFORE the margin, so the
		// margins are truly blank. Keep walking the carry over the dropped
		// row's own sequences afterwards, so the reopen at the band's end
		// reflects what the ORIGINAL stream would still have open there.
		if len(bgSgrOpen) > 0 {
			b.WriteString("\x1b[0m")
		}
		if bgOsc8Open != "" {
			b.WriteString("\x1b]8;;\x1b\\")
		}
		reopenPending = true
		bgSgrOpen, bgOsc8Open = backdropOpen(bgLine, bgSgrOpen, bgOsc8Open)

		pos := placeX
		if pos > 0 {
			b.WriteString(ws.render(pos))
		}

		fgLine := fgLines[i-placeY]
		b.WriteString(fgLine)
		pos += lineWidth(fgLine)
		b.WriteString(closeSequences(fgLine))

		if remaining := lineWidth(bgLine) - pos; remaining > 0 {
			b.WriteString(ws.render(remaining))
		}
	}

	return b.String()
}

func clamp(v, lower, upper int) int {
	return min(max(v, lower), upper)
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
		// layout.Cells, like every other measurement here, so the padding this
		// emits is counted in the same cells the compositor budgeted for it. The
		// max(…,1) is a hang guard, not a width decision: a zero-width rune in the
		// pattern (a ZWJ, a combining mark) never advances i, and the loop would
		// spin forever appending it.
		if advance := layout.Cells(string(writtenRune)); advance > 0 {
			i += advance
		} else {
			i++
		}
	}

	// Fill any extra gaps white spaces. This might be necessary if any runes
	// are more than one cell wide, which could leave a one-rune gap.
	short := width - layout.Cells(b.String())
	if short > 0 {
		b.WriteString(strings.Repeat(" ", short))
	}

	return w.style.Styled(b.String())
}

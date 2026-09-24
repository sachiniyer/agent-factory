package hooklog

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ExcerptLines is the most hook output lines a diagnostic quotes (#4853). The
// complete output stays in the per-run log file the diagnostic names; the
// excerpt only has to say which way the hook died, and a build's last few
// lines do that. Quoting more is what put 227 lines of one failed `make` into
// the daemon log.
const ExcerptLines = 5

// ExcerptPrefix starts every quoted output line. The daemon log is read line by
// line — by people and by tools — and an unprefixed output line such as
// " ERROR  @scope/pkg#build" reads as a top-level entry of its own.
const ExcerptPrefix = "  | "

// ExcerptLineBytes bounds one quoted line, ellipsis included, not counting
// ExcerptPrefix. A progress bar or a minified bundle can be a single line of many
// kilobytes, and a hook that never prints a newline is one line in total.
const ExcerptLineBytes = 240

// ansiEscape matches CSI and OSC terminal sequences. Build tools colour their
// output even into a file, and the escapes are noise in a log.
var ansiEscape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-_])`)

// Excerpt renders the last ExcerptLines non-empty lines of a hook's output as a
// suffix for a one-line diagnostic: a short label, then each line on its own
// row behind ExcerptPrefix. It returns "" when the output has nothing to show.
//
// Terminal escapes and other control characters are dropped, and a line that a
// carriage return redrew keeps only what was drawn last, which is what the
// terminal showed. A line longer than ExcerptLineBytes keeps its end.
func Excerpt(output string) string {
	output = strings.TrimPrefix(output, truncatedMarker())
	var lines []string
	for _, raw := range strings.Split(output, "\n") {
		if line := excerptLine(raw); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > ExcerptLines {
		lines = lines[len(lines)-ExcerptLines:]
	}
	label := "last output line"
	if len(lines) > 1 {
		label = fmt.Sprintf("last %d output lines", len(lines))
	}
	return "; " + label + ":\n" + ExcerptPrefix + strings.Join(lines, "\n"+ExcerptPrefix)
}

func excerptLine(raw string) string {
	raw = ansiEscape.ReplaceAllString(raw, "")
	if i := strings.LastIndexByte(strings.TrimRight(raw, "\r"), '\r'); i >= 0 {
		raw = raw[i+1:]
	}
	raw = strings.ToValidUTF8(raw, "�")
	line := strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, raw))
	if len(line) <= ExcerptLineBytes {
		return line
	}
	// Keep the END of an overlong line: a hook that dies after a long unbroken
	// run of output prints its reason last.
	cut := len(line) - (ExcerptLineBytes - len("…"))
	for cut < len(line) && !utf8.RuneStart(line[cut]) {
		cut++
	}
	return "…" + line[cut:]
}

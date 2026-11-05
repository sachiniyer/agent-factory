package session

import (
	"strconv"
	"strings"
	"time"
)

// ageUnits are the extra units ParseAgeDuration accepts on top of
// time.ParseDuration's: retention ages are written in days and weeks, which Go
// has no words for. Both are fixed lengths — months and years are not, so they
// stay invalid rather than guess at a retention window.
var ageUnits = map[string]float64{"d": 24, "w": 7 * 24}

// ParseAgeDuration parses what time.ParseDuration does — the same grammar,
// signs, fractions and overflow rules — plus the day and week units age flags
// are written in: "30d", "2w" and "2d12h" all work where only "720h" did.
// Malformed input still reports time.ParseDuration's own error.
func ParseAgeDuration(value string) (time.Duration, error) {
	if d, err := time.ParseDuration(value); err == nil {
		return d, nil
	}
	// Rewrite each <number><d|w> segment into its hour count and hand the whole
	// string back to time.ParseDuration, which stays the grammar authority: a
	// segment that is not a number followed by exactly d or w is copied through
	// untouched, so malformed input fails with Go's own error text.
	isDigitOrDot := func(c byte) bool { return c == '.' || '0' <= c && c <= '9' }
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); {
		start := i
		for i < len(value) && isDigitOrDot(value[i]) {
			i++
		}
		num := value[start:i]
		unitStart := i
		for i < len(value) && !isDigitOrDot(value[i]) {
			i++
		}
		unit := value[unitStart:i]
		if mult, ok := ageUnits[unit]; ok && num != "" {
			if f, err := strconv.ParseFloat(num, 64); err == nil {
				b.WriteString(strconv.FormatFloat(f*mult, 'f', -1, 64))
				b.WriteByte('h')
				continue
			}
		}
		b.WriteString(value[start:i])
	}
	return time.ParseDuration(b.String())
}

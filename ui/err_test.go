package ui

import (
	"errors"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

// TestErrBoxNarrowWidthDoesNotOverflow is a regression test for issue #337.
// When the container width is less than the width of the "..." tail,
// runewidth.Truncate(..., width, "...") returns "..." which is wider than
// the container. lipgloss.Place does not clip, so the overflow is visible.
// The fix drops the ellipsis tail when the container can't fit it.
func TestErrBoxNarrowWidthDoesNotOverflow(t *testing.T) {
	for _, width := range []int{1, 2, 3} {
		width := width
		t.Run("width="+itoa(width), func(t *testing.T) {
			e := NewErrBox()
			e.SetSize(width, 1)
			e.SetError(errors.New("a very long error message that needs truncation"))

			out := e.String()
			// lipgloss.Place pads with spaces; check each line's rune width.
			for _, line := range strings.Split(out, "\n") {
				if got := runewidth.StringWidth(stripANSI(line)); got > width {
					t.Errorf("rendered line width %d exceeds container width %d (line=%q)", got, width, line)
				}
			}
		})
	}
}

// itoa is a tiny helper to avoid importing strconv just for subtest names.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// stripANSI removes ANSI escape sequences (CSI) so width measurements
// reflect only visible runes.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// Skip until a final byte in the range 0x40-0x7e.
			j := i + 2
			for j < len(s) {
				c := s[j]
				j++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			i = j - 1
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

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

// TestErrBoxStripsANSIBeforeTruncating is a regression test for issue #525.
// When an error string carries embedded ANSI escape sequences (e.g. from an
// agent's pane output reaching ErrBox via #502), runewidth.Truncate would
// cut sequences mid-byte and leak visible garbage like "[31m". Stripping
// ANSI before truncation keeps the rendered text clean.
func TestErrBoxStripsANSIBeforeTruncating(t *testing.T) {
	e := NewErrBox()
	e.SetSize(20, 1)
	// Red SGR, payload, reset, then filler beyond the 20-cell width.
	e.SetError(errors.New("\x1b[31merror in red\x1b[0m " + strings.Repeat("x", 200)))

	out := e.String()
	// The raw ESC byte must not appear anywhere in the input portion — only
	// in lipgloss's own styling wrapping the result. Easier check: the
	// literal "[31m" and "[0m" payloads must not survive into the output.
	if strings.Contains(out, "[31m") {
		t.Errorf("output leaked partial ANSI input sequence [31m: %q", out)
	}
	if strings.Contains(out, "[0m ") {
		t.Errorf("output leaked partial ANSI input sequence [0m: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if got := runewidth.StringWidth(stripANSI(line)); got > 20 {
			t.Errorf("rendered line width %d exceeds container width 20 (line=%q)", got, line)
		}
	}
}

// TestErrBoxWithoutANSIUnchanged ensures the strip pass is a no-op for
// plain-text errors (no spurious changes to existing rendering).
func TestErrBoxWithoutANSIUnchanged(t *testing.T) {
	e := NewErrBox()
	e.SetSize(500, 1)
	e.SetError(errors.New("plain error message"))

	out := e.String()
	if !strings.Contains(stripANSI(out), "plain error message") {
		t.Errorf("expected plain message to render unchanged, got %q", out)
	}
}

// TestErrBoxStripsPrivateModeCSI is a regression test for issue #552. The
// original strip regex only allowed [0-9;] in the parameter byte slot, so
// private-mode sequences like \x1b[?25l (cursor hide/show), \x1b[?1049h
// (alt-screen), and \x1b[?7l (autowrap off) leaked through. runewidth then
// counted "?25l" etc. as visible runes and the box truncated prematurely.
func TestErrBoxStripsPrivateModeCSI(t *testing.T) {
	payload := "agent crashed"
	// Mix display SGR with several private-mode sequences.
	raw := "\x1b[?25l\x1b[?1049h\x1b[?7l\x1b[31m" + payload + "\x1b[0m\x1b[?25h\x1b[?1049l"
	stripped := ansiEscapeRegex.ReplaceAllString(raw, "")
	if stripped != payload {
		t.Fatalf("private-mode CSI not stripped: got %q want %q", stripped, payload)
	}
	if got := runewidth.StringWidth(stripped); got != runewidth.StringWidth(payload) {
		t.Errorf("width after strip = %d, want %d", got, runewidth.StringWidth(payload))
	}

	// End-to-end: a wide-enough box must render the full payload without
	// truncation now that private-mode bytes no longer inflate the width.
	e := NewErrBox()
	e.SetSize(80, 1)
	e.SetError(errors.New(raw))
	out := e.String()
	if !strings.Contains(stripANSI(out), payload) {
		t.Errorf("payload missing from rendered output: %q", out)
	}
	if strings.Contains(out, "?25l") || strings.Contains(out, "?1049h") || strings.Contains(out, "?7l") {
		t.Errorf("private-mode sequence leaked into output: %q", out)
	}
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

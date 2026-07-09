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

func TestErrBoxTruncatedErrorShowsDetailsHint(t *testing.T) {
	const full = "no clipboard tool found (install xclip/wl-clipboard, or pbcopy on macOS); PR URL: https://example.invalid/pr/987"
	e := NewErrBox()
	e.SetSize(64, 1)
	e.SetError(errors.New(full))

	out := stripANSI(e.String())
	if !strings.Contains(out, "E details") {
		t.Fatalf("truncated error should advertise the full-details key, got %q", out)
	}
	if strings.Contains(out, "https://example.invalid/pr/987") {
		t.Fatalf("test precondition failed: rendered status line was not truncated: %q", out)
	}
	if got := e.FullError(); got != full {
		t.Fatalf("FullError() = %q, want %q", got, full)
	}
}

// TestErrBoxStripsANSIVariants is a regression matrix covering every ANSI
// variant the strip pass has had to learn the hard way:
//   - Plain CSI / SGR (#525, original bug — partial CSI bytes leaked).
//   - Private-mode CSI like \x1b[?25l (#552 — the bespoke regex's [0-9;]
//     parameter class didn't allow the "?" prefix, so these inflated width).
//   - OSC 8 hyperlinks terminated by ST (\x1b\\) (#565 — the bespoke regex
//     never matched \x1b] at all, so OSC payload counted as visible runes).
//   - OSC terminated by BEL (\x07), the legacy xterm terminator that some
//     emitters (e.g. iTerm OSC 1337) still use instead of ST.
//
// Switching to xansi.Strip handles all of these uniformly; this test pins
// that behavior so the next exotic variant doesn't require another fix-up.
func TestErrBoxStripsANSIVariants(t *testing.T) {
	const payload = "agent crashed"
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "plain_csi_sgr",
			raw:  "\x1b[31m" + payload + "\x1b[0m",
		},
		{
			name: "private_mode_csi",
			raw:  "\x1b[?25l\x1b[?1049h\x1b[?7l\x1b[31m" + payload + "\x1b[0m\x1b[?25h\x1b[?1049l",
		},
		{
			name: "osc8_hyperlink_st_terminated",
			// OSC 8 ; params ; URI ST <link text> OSC 8 ;; ST
			raw: "\x1b]8;;https://example.com/very/long/url/that/blows/up/width/math\x1b\\" + payload + "\x1b]8;;\x1b\\",
		},
		{
			name: "osc_bel_terminated",
			// OSC 0 (set window title) terminated by BEL. The title payload
			// must not be width-counted.
			raw: "\x1b]0;a title that is much longer than the payload\x07" + payload,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Box wide enough to fit the visible payload but NOT wide enough
			// to fit any inflated count from leaked escape bytes. This is
			// the condition that historically tripped truncation.
			width := runewidth.StringWidth(payload) + 4
			e := NewErrBox()
			e.SetSize(width, 1)
			e.SetError(errors.New(tc.raw))

			out := e.String()

			// 1. Visible payload survives intact (no premature truncation).
			if !strings.Contains(stripANSI(out), payload) {
				t.Errorf("payload missing from rendered output: %q", out)
			}
			// 2. No escape-sequence carcass leaks as visible characters.
			//    Spot-check signature fragments from each variant.
			for _, leak := range []string{"[31m", "[?25l", "[?1049h", "8;;https://", "0;a title"} {
				if strings.Contains(out, leak) {
					t.Errorf("escape fragment %q leaked into output: %q", leak, out)
				}
			}
			// 3. The rendered line width must equal the visible payload
			//    width (within the box). If OSC/private-mode bytes had been
			//    width-counted, this would fail with an inflated width.
			for _, line := range strings.Split(out, "\n") {
				if got := runewidth.StringWidth(stripANSI(line)); got > width {
					t.Errorf("rendered line width %d exceeds container width %d (line=%q)", got, width, line)
				}
			}
		})
	}
}

// TestErrBoxStripsCarriageReturns is a regression test for issue #668. Hook
// scripts (see session/backend_hook.go, which wraps mixed stdout/stderr from
// CombinedOutput into error messages) commonly emit \r from progress
// indicators. A \r reaching the terminal moves the cursor to column 0,
// overwriting lipgloss.Place's padding and corrupting the error box.
func TestErrBoxStripsCarriageReturns(t *testing.T) {
	e := NewErrBox()
	e.SetSize(40, 1)
	// Simulate a progress indicator that overwrites itself with \r.
	e.SetError(errors.New("Loading...\rLoading....\rLoading.....\rReady"))

	out := e.String()
	if strings.Contains(out, "\r") {
		t.Errorf("carriage return (\\r) leaked into rendered output, corrupting display: %q", out)
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

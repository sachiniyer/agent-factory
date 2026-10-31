package ui

import (
	"errors"
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// TestErrBoxNarrowWidthDoesNotOverflow is a regression test for issue #337.
// When the container width is less than the width of the "…" tail,
// runewidth.Truncate(..., width, "…") returns "…" which is wider than
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
	const full = "no clipboard tool found (install xclip/wl-clipboard, or pbcopy on macOS); Session URL: https://example.invalid/session/987"
	e := NewErrBox()
	e.SetSize(64, 1)
	e.SetError(errors.New(full))

	out := stripANSI(e.String())
	if !strings.Contains(out, "E details") {
		t.Fatalf("truncated error should advertise the full-details key, got %q", out)
	}
	if strings.Contains(out, "https://example.invalid/session/987") {
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

// TestErrBoxJoinsMultiLineErrorsWithSeparator covers the #1826 voice sweep. A
// multi-line error was flattened onto the status line with "//", which reads as a
// commented-out fragment or a URL remnant rather than a break between two
// sentences. " · " is the repo's separator glyph, already used by the pane header
// and the web term meta.
func TestErrBoxJoinsMultiLineErrorsWithSeparator(t *testing.T) {
	e := NewErrBox()
	// Wide enough that statusLine returns the joined line untruncated.
	e.SetSize(120, 3)
	e.SetError(errors.New("push failed\nremote rejected the branch"))

	line := e.statusLine()
	if !strings.Contains(line, "push failed · remote rejected the branch") {
		t.Errorf("want the two lines joined by ' · ', got %q", line)
	}
	if strings.Contains(line, "//") {
		t.Errorf("the // joiner must be gone, got %q", line)
	}
}

func TestSanitizeErrorControls(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"backspace", "bad\bpath", "badpath"},
		{"bell", "bad\apath", "badpath"},
		{"form feed", "bad\fpath", "badpath"},
		{"vertical tab", "bad\vpath", "badpath"},
		{"delete", "bad\x7fpath", "badpath"},
		{"UTF-8 NEL", "bad\u0085path", "badpath"},
		{"newline", "first\nsecond", "first · second"},
		// Tabs are expanded to four spaces — what lipgloss's default
		// TabWidth renders — so the width budget matches what the
		// terminal actually draws and the status bar's downstream clamp
		// does not silently cut the tail. See sanitizeError's comment.
		{"tab", "first\tsecond", "first    second"},
		{"mixed", "\x1b[31mbad\b\a\f\v\x7f\u0085path\r\nnext\titem\x1b[0m", "badpath · next    item"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeError(tc.raw); got != tc.want {
				t.Errorf("sanitizeError(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			box := NewErrBox()
			box.SetError(errors.New(tc.raw))
			if got := box.FullError(); got != tc.want {
				t.Errorf("FullError() = %q, want %q", got, tc.want)
			}
		})
	}
	for r := rune(0); r <= 0x9f; r++ {
		if r >= 0x20 && r < 0x7f || r == '\n' || r == '\t' {
			continue
		}
		if got := sanitizeError("safe" + string(r)); got != "safe" {
			t.Errorf("control U+%04X survives sanitizing: %q", r, got)
		}
	}
}

// TestErrBoxDetailsHintOverride: a modal text field swaps in the key that
// opens the details from there (#4123); clearing the override restores the
// error_details binding's hint.
func TestErrBoxDetailsHintOverride(t *testing.T) {
	e := NewErrBox()
	e.SetSize(40, 1)
	e.SetNotice(errors.New("a session titled \"todo-core\" conflicts with existing session \"todo-core\""))

	e.SetDetailsHint("ctrl+e details")
	if got := e.String(); !strings.Contains(got, "ctrl+e details") {
		t.Errorf("the override hint must replace E details, got %q", got)
	}

	e.SetDetailsHint("")
	if got := e.String(); !strings.Contains(got, "E details") || strings.Contains(got, "ctrl+e") {
		t.Errorf("clearing the override must restore E details, got %q", got)
	}

	e.HideDetailsHint(true)
	if got := e.String(); strings.Contains(got, "details") {
		t.Errorf("a hidden hint must advertise no details key, got %q", got)
	}
	e.HideDetailsHint(false)
	if got := e.String(); !strings.Contains(got, "E details") {
		t.Errorf("unhiding must restore the hint, got %q", got)
	}
}

// TestErrBoxHintPaintsWithoutRetaining pins SetHint's contract (#5356 review):
// a hint answers a key on the same bar and fades on the same Expire timer, but
// it is never retrievable — acknowledging an empty store must leave it empty,
// or `E details` would reopen the hint itself as "Last notice".
func TestErrBoxHintPaintsWithoutRetaining(t *testing.T) {
	e := NewErrBox()

	e.SetHint(errors.New("No recent error"))
	if got := e.FullError(); got != "No recent error" {
		t.Errorf("the hint must paint on the bar, got %q", got)
	}
	if text, _ := e.RetainedNotice(); text != "" {
		t.Errorf("the hint must not enter the retained slot, got %q", text)
	}

	e.Expire()
	if got := e.FullError(); got != "" {
		t.Errorf("the hint must fade on the same timer, still shows %q", got)
	}
	if text, _ := e.RetainedNotice(); text != "" {
		t.Errorf("expiry must not resurrect the hint into retained, got %q", text)
	}

	// The contrast the contract exists against: a real notice does retain.
	e.SetNotice(errors.New("No preview to keep"))
	if text, _ := e.RetainedNotice(); text != "No preview to keep" {
		t.Errorf("a real notice must still retain for E details, got %q", text)
	}
	e.Expire()
	if text, _ := e.RetainedNotice(); text != "No preview to keep" {
		t.Errorf("a real notice must survive its fade, got %q", text)
	}
}

// The emoji clusters the bug report measures against runewidth's
// under-reporting: heart+VS16, keycap, and regional-indicator flags. The
// table is from ui/layout/cells.go; the values are what tmux 3.4 actually
// advances. The chained ZWJ family is deliberately NOT here: the bug
// report excludes it ("ZWJ-family emoji measures the same 2 cells on both
// runewidth and xansi, so it does not trigger the divergence") and its
// Cells/UpperBound disagreement is a separate concern of the contract
// measure, not the measure the bug fixes.
var errBoxMeasureCorpus = []struct {
	name         string
	s            string
	cells, bound int
}{
	{"heart+vs16", "\u2764\uFE0F", 2, 2},
	{"keycap", "1\uFE0F\u20E3", 2, 2},
	{"regional indicator", "\U0001F1FA\U0001F1F8", 2, 2},
}

// TestErrBoxStatusLineFitsContractMeasure is the regression test for the
// silent-hard-cut bug. statusLine trusts its fit test to ensure the error row
// fits e.width; before this fix the test was on runewidth, which under-reports
// what the terminal advances on the ordinary VS16/keycap/regional-indicator
// emoji classes (runewidth 1 where tmux and x-ansi measure 2). A line the
// fit test accepted therefore rendered wider than the bar, and the status
// bar's downstream layout.ClampToRect silently right-cut it with no ellipsis,
// taking trailing content and the `E details` hint itself into the bargain.
//
// The fix replaces runewidth with layout.Cells (and the truncator with
// xansi.Truncate plus the progressive-loop trim from TruncateToCells /
// truncateToContract); this test pins that on every cluster from the
// cells.go table the report calls out, statusLine's output fits e.width on
// the contract measure the bar's clamp enforces — and the `E details` hint
// survives whole.
func TestErrBoxStatusLineFitsContractMeasure(t *testing.T) {
	for _, tc := range errBoxMeasureCorpus {
		if got := layout.Cells(tc.s); got != tc.cells {
			t.Fatalf("%s: precondition — layout.Cells = %d, want %d (from cells.go table)", tc.name, got, tc.cells)
		}
		if got := layout.CellsUpperBound(tc.s); got != tc.bound {
			t.Fatalf("%s: precondition — layout.CellsUpperBound = %d, want %d (from cells.go table)", tc.name, got, tc.bound)
		}
	}

	// Pick a bar narrow enough each cluster's notice cannot fit untruncated
	// at its real width, and small enough to force the hint-budget truncation
	// branch (prefixWidth > 3). The default `E details` hint claims 11
	// cells, so width=20 leaves 9 cells of prefix: enough to make truncation
	// visible while keeping the hint intact.
	const width = 20
	for _, tc := range errBoxMeasureCorpus {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			notice := strings.Repeat(tc.s, 16) // guaranteed longer than the bar
			e := NewErrBox()
			e.SetSize(width, 1)
			e.SetError(errors.New(notice))
			line := e.statusLine()
			if got := layout.CellsUpperBound(line); got > width {
				t.Errorf("statusLine returned a row the contract measure says is %d "+
					"cells, over the %d-cell bar: %q", got, width, line)
			}
			// And specifically the `E details` hint must survive intact —
			// the symptom that earned this bug its report.
			if !strings.Contains(line, "E details") {
				t.Errorf("the truncated notice must advertise an intact `E details` "+
					"hint so the user can read the rest from the bar; got %q", line)
			}
		})
	}
}

// TestErrBoxStatusLineKeepsHintIntactOnHeartVS16 is the narrow form of the
// bug report's first repro: a heart+VS16 notice where runewidth says the bar
// is wide enough for the full line plus hint, but x-ansi (the contract
// measure) says the row is 3 cells over. Before the fix the clamp's silent
// right-cut produced `❤️❤️❤️…  E deta`; after, the row fits and the hint is
// whole.
func TestErrBoxStatusLineKeepsHintIntactOnHeartVS16(t *testing.T) {
	const heart = "\u2764\uFE0F" // runewidth 1, x-ansi 2, tmux 2
	e := NewErrBox()
	e.SetSize(15, 1)
	e.SetError(errors.New(strings.Repeat(heart, 16)))

	line := e.statusLine()

	// The measure the status bar's clamp uses must agree the row fits.
	if got := layout.CellsUpperBound(line); got > 15 {
		t.Fatalf("statusLine emitted a row the contract measure says is %d cells, "+
			"over the 15-cell bar; the clamp would silently cut it: %q", got, line)
	}

	// And the hint must survive whole — the bug's headline symptom.
	if !strings.Contains(line, "E details") {
		t.Errorf("expected an intact `E details` hint on a clustered notice, got %q", line)
	}
	// The bug's specific truncation was `E deta`; assert no partial hint
	// survives whole-less.
	if strings.Contains(line, "E deta") && !strings.Contains(line, "E details") {
		t.Errorf("the hint was truncated mid-word to `E deta`: %q", line)
	}
}

// TestErrBoxStatusLineMarksSilentCutWithEllipsis is the bug report's second
// repro: 30 hearts + `xxxxx` in a 40-cell bar. runewidth reported the line at
// 35 (≤ 40), so the fit test returned the line untruncated, intending it to
// fit; the real measure is 65 and the clamp silently cut the tail — 10 hearts
// and all five filler characters vanished with no truncation marker. After
// the fix, statusLine sees the real width and marks the cut with `…`.
func TestErrBoxStatusLineMarksSilentCutWithEllipsis(t *testing.T) {
	const heart = "\u2764\uFE0F"
	e := NewErrBox()
	e.SetSize(40, 1)
	e.SetError(errors.New(strings.Repeat(heart, 30) + "xxxxx"))

	line := e.statusLine()
	if got := layout.CellsUpperBound(line); got > 40 {
		t.Fatalf("statusLine emitted a row the contract measure says is %d cells, "+
			"over the 40-cell bar: %q", got, line)
	}
	// The line must mark the cut — that is the whole reason the bug is
	// visible. Hard-cut launches no "I have more" affordance at all.
	if !strings.Contains(line, "…") {
		t.Errorf("a clipped notice must mark its cut with `…`; got %q", line)
	}
	// And the hint must survive whole on exactly this case, which the bug
	// report called out by name.
	if !strings.Contains(line, "E details") {
		t.Errorf("the truncated notice must advertise an intact `E details` hint; got %q", line)
	}
}

// TestErrBoxExpandsTabsForWidthBudget pins the second, independent trigger.
// sanitizeError used to keep tabs ("Tabs remain text"), but lipgloss with
// the default TabWidth renders every tab as four spaces. So a tab passed
// the fit test as 0 cells, statusLine returned the line untruncated, and
// the clamp cut the trailing content silently (e.g. `col1\tcol2` in a 9-cell
// bar rendered as `col1    ` with `ol2` silently gone). Expanding tabs to
// four spaces in sanitizeError — what lipgloss would emit anyway — makes
// the budget match the render.
func TestErrBoxExpandsTabsForWidthBudget(t *testing.T) {
	// First confirm sanitizeError expands the tab.
	if got, want := sanitizeError("col1\tcol2"), "col1    col2"; got != want {
		t.Fatalf("sanitizeError must expand a tab to the four cells lipgloss "+
			"renders; got %q, want %q", got, want)
	}

	e := NewErrBox()
	e.SetSize(9, 1)
	e.SetError(errors.New("col1\tcol2"))

	line := e.statusLine()
	if got := layout.CellsUpperBound(line); got > 9 {
		t.Fatalf("statusLine emitted a row the contract measure says is %d cells, "+
			"over the 9-cell bar: %q", got, line)
	}
	// The cut must be marked — the absence of a marker is the whole bug.
	if !strings.Contains(line, "…") {
		t.Errorf("a tab notice that overruns the bar must mark its cut with `…`; got %q", line)
	}
	// And exactly the col1 prefix survives — the silent cut used to take
	// col2's last runes; the ellipsis version keeps the user's leading
	// content and remembers there is more.
	if !strings.HasPrefix(line, "col1") {
		t.Errorf("the leading col1 text must survive truncation; got %q", line)
	}
}

// TestTruncateStatusTextFitsContractMeasure pins the lower-level helper
// directly. Before the fix, truncateStatusText measured on runewidth and
// truncated with runewidth.Truncate, so a 2-cell (on xansi) heart passed
// its 1-cell budget and was returned whole — the very under-reporting
// statusLine inherits. The fix lets the helper be called from any caller
// (RecoveryContent uses it too) without exporting the bug.
func TestTruncateStatusTextFitsContractMeasure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		width int
	}{
		{"heart+vs16 into 1", "\u2764\uFE0F", 1},
		{"heart+vs16 into 3", "\u2764\uFE0F", 3},
		{"keycap into 1", "1\uFE0F\u20E3", 1},
		{"regional indicator into 1", "\U0001F1FA\U0001F1F8", 1},
		{"plain ascii over width", "a very long error message", 10},
		{"plain ascii at width", "abcde", 5},
		{"plain ascii one over width", "abcde", 4},
		{"zero width", "abcde", 0},
		{"negative width", "abcde", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := truncateStatusText(tc.text, tc.width)
			if tc.width <= 0 {
				if out != "" {
					t.Fatalf("width <= 0 must return empty, got %q", out)
				}
				return
			}
			if got := layout.CellsUpperBound(out); got > tc.width {
				t.Errorf("truncateStatusText returned %d cells, over the %d-cell "+
					"budget: %q (text=%q)", got, tc.width, out, tc.text)
			}
		})
	}
}

// TestErrBoxRenderedStatusBarFitsContractMeasure closes the loop
// end-to-end. The bug's harm lives in the StatusBar output: errBox.String()
// is the error row ui.StatusBar composes and then hands to
// layout.ClampToRect. This test drives that whole chain — NewStatusBar →
// SetRect → String — for every cluster in the cells.go disagreement corpus
// the report calls out AND for the tab path, and asserts each rendered line
// honours the bar's contract (<= r.W on the contract measure, plus the
// `E details` hint survives whole when the prefix budget admits it).
func TestErrBoxRenderedStatusBarFitsContractMeasure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notice string
		width  int
	}{
		// The bug report's two repros are the headline cases. Both
		// narrow the bar enough to land in the hint-budget truncation
		// branch (`prefixWidth > 3`), which is where the hint was being
		// silently hard-cut before the fix.
		{"heart+vs16 narrow bar", strings.Repeat("\u2764\uFE0F", 16), 15},
		{"heart+vs16 wide bar", strings.Repeat("\u2764\uFE0F", 32), 60},
		// And the rest of the cells.go disagreement corpus the fix
		// covers: keycap, regional flags. Same narrow branches as
		// heart+VS16 so the corpus is symmetric under the contract.
		{"keycap over bar", strings.Repeat("1\uFE0F\u20E3", 8), 15},
		{"regional indicator over bar", strings.Repeat("\U0001F1FA\U0001F1F8", 8), 15},
		// Tab path: the bar must see the 4-cell expansion lipgloss will
		// emit, mark the cut, and keep the hint — not silently cut
		// `col2` like the old runewidth math did.
		{"tab over bar", "col1\tcol2 lots more text", 22},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// The status bar is two rows: hints above, error line below.
			// Both must honour the contract — the bug's symptom is on
			// the error row.
			r := layout.Rect{W: tc.width, H: layout.StatusBarRows}
			menu := NewMenu()
			errBox := NewErrBox()
			sb := NewStatusBar(menu, errBox)
			sb.SetRect(r)
			errBox.SetError(errors.New(tc.notice))

			out := sb.String()
			// Every row must honour the rectangle contract — the bar is
			// exactly rect.W cells wide on the contract measure, and a
			// row wider than r.W wraps or pushes the chrome out.
			lines := strings.Split(out, "\n")
			if len(lines) != r.H {
				t.Fatalf("expected %d lines from the status bar, got %d: %q", r.H, len(lines), out)
			}
			for i, line := range lines {
				if got := layout.CellsUpperBound(line); got > tc.width {
					t.Errorf("line %d of the rendered status bar measures %d cells, "+
						"over the %d-cell rect: %q", i, got, tc.width, xansi.Strip(line))
				}
			}
			// And the readable hint survives the whole pipeline, end to
			// end, exactly the surface the bug report's symptom named.
			// Every case above is in the hint-budget branch
			// (prefixWidth > 3), so the hint must report.
			plain := xansi.Strip(out)
			if !strings.Contains(plain, "E details") {
				t.Errorf("the rendered status bar must advertise an intact `E details` "+
					"hint on a clipped clustered notice; got %q", plain)
			}
		})
	}
}

// A standing condition (#6386: the daemon's web listener is unbound) stays on
// the bar until retracted, yields to a notice while one is up, never lands in
// the retained slot `E details` reads, and never overflows the bar.
func TestErrBoxStandingConditionPersistsAndYieldsToNotices(t *testing.T) {
	e := NewErrBox()
	e.SetSize(60, 1)
	e.SetStanding("web UI not served: 127.0.0.1:8443 is not bound")

	if got := stripANSI(e.String()); !strings.Contains(got, "web UI not served") {
		t.Fatalf("standing text missing from the bar: %q", got)
	}
	// Expire is what fades a transient notice; the standing text must outlive it.
	e.Expire()
	if got := stripANSI(e.String()); !strings.Contains(got, "web UI not served") {
		t.Fatalf("standing text must survive Expire: %q", got)
	}
	if text, _ := e.RetainedNotice(); text != "" {
		t.Fatalf("standing text must not be retained for `E details`, got %q", text)
	}

	e.SetNotice(errors.New("session created"))
	if got := stripANSI(e.String()); !strings.Contains(got, "session created") || strings.Contains(got, "web UI") {
		t.Fatalf("a notice must take the bar over the standing text: %q", got)
	}
	e.Expire()
	if got := stripANSI(e.String()); !strings.Contains(got, "web UI not served") {
		t.Fatalf("standing text must return when the notice fades: %q", got)
	}

	e.SetSize(20, 1)
	for _, line := range strings.Split(e.String(), "\n") {
		if w := runewidth.StringWidth(stripANSI(line)); w > 20 {
			t.Fatalf("standing line width %d exceeds the bar (20): %q", w, line)
		}
	}

	e.SetStanding("")
	if got := strings.TrimSpace(stripANSI(e.String())); got != "" {
		t.Fatalf("retracted standing text must leave the bar empty, got %q", got)
	}
}

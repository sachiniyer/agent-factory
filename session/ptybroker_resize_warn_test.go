package session

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #3862. A pane whose tmux session is GONE fails EVERY later resize-window for as
// long as that pane stays dead, so warning per frame turns one drag into a burst of
// identical lines (five inside one second in the daemon log that prompted this, at
// rows 500..504). That case warns ONCE; every other failure stays exactly as noisy
// as it was, because a non-terminal one can clear and each occurrence is genuinely
// news. "Once" is per DEAD PANE rather than per process — a recovery that re-spawns
// tmux re-arms it, which the third test below is about.
//
// What must NOT change, and is asserted here rather than assumed: the authoritative
// echo is still broadcast per resize (resize()'s own comment: a failed apply must
// never swallow the echo clients reflow on), the apply is still attempted every
// frame, and the error is still returned to the caller. The silencing is the log
// line and nothing else.

// goneResizeFrames is how many resize frames each case drives — more than one, so
// "once" and "once per frame" are distinguishable outcomes.
const goneResizeFrames = 5

func TestPTYBrokerWarnsOnceForAGoneSessionsResize(t *testing.T) {
	warnings := captureWarningLog(t)
	ch := &fakeClientlessChannel{resizeErr: fmt.Errorf("%w: resize-window", tmux.ErrSessionGone)}
	br := newPTYBroker(ch)
	sub, err := br.subscribe(0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// One echo read PER resize, deliberately: a subscriber that lags several resizes
	// is collapsed by resizeSeen/resizeGen to a single echo carrying the latest size,
	// so driving N resizes and then draining would assert one event and prove nothing
	// about frames 2..N. Interleaving is what makes this genuinely N echoes.
	for i := range goneResizeFrames {
		rows, cols := uint16(40+i), uint16(100+i)
		if err := br.resize(rows, cols); !errors.Is(err, tmux.ErrSessionGone) {
			t.Fatalf("resize %d returned %v, want the apply error still propagated", i, err)
		}
		ev, err := nextWithin(t, sub, 2*time.Second)
		if err != nil {
			t.Fatalf("resize %d echo: %v", i, err)
		}
		if ev.Kind != PTYResize || ev.Rows != rows || ev.Cols != cols {
			t.Fatalf("resize %d echo = %+v, want the authoritative %dx%d", i, ev, rows, cols)
		}
	}

	if len(ch.resizes) != goneResizeFrames {
		t.Fatalf("applies attempted = %d, want %d: silencing the log must not stop the apply",
			len(ch.resizes), goneResizeFrames)
	}
	lines := warningLinesContaining(warnings.String(), "apply resize")
	if len(lines) != 1 {
		t.Fatalf("warnings = %d, want exactly 1 for a gone session:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	// Anti-vacuity. "Exactly one" is also what a broker that never warned at all
	// would score if the capture seam were broken or the message were reworded, so
	// pin the surviving line to the FIRST frame's geometry.
	if !strings.Contains(lines[0], "40x100") {
		t.Fatalf("the one warning is %q, want the FIRST frame's 40x100", lines[0])
	}
}

func TestPTYBrokerWarnsAgainAfterARecoveryRespawnsTmux(t *testing.T) {
	warnings := captureWarningLog(t)
	ch := &fakeClientlessChannel{resizeErr: fmt.Errorf("%w: resize-window", tmux.ErrSessionGone)}
	br := newPTYBroker(ch)

	// First death: one line, and the burst after it stays silent.
	for i := range goneResizeFrames {
		if err := br.resize(uint16(40+i), uint16(100+i)); !errors.Is(err, tmux.ErrSessionGone) {
			t.Fatalf("resize %d returned %v, want the apply error", i, err)
		}
	}
	if got := len(warningLinesContaining(warnings.String(), "apply resize")); got != 1 {
		t.Fatalf("warnings before recovery = %d, want 1", got)
	}

	// A session recovery re-spawns tmux and PRESERVES this broker (#1682,
	// resetBrokerCaptures), so "gone" stops being terminal here. A second death
	// after it is new information and must not be swallowed by the first one's latch.
	br.resetCapture()
	for i := range goneResizeFrames {
		if err := br.resize(uint16(60+i), uint16(200+i)); !errors.Is(err, tmux.ErrSessionGone) {
			t.Fatalf("post-recovery resize %d returned %v, want the apply error", i, err)
		}
	}
	lines := warningLinesContaining(warnings.String(), "apply resize")
	if len(lines) != 2 {
		t.Fatalf("warnings across both deaths = %d, want 2 (one each):\n%s", len(lines), strings.Join(lines, "\n"))
	}
	// Anti-vacuity: the second line must be the POST-recovery death, not a duplicate
	// of the first — a latch that was never consulted again would also score two.
	if !strings.Contains(lines[1], "60x200") {
		t.Fatalf("second warning is %q, want the post-recovery 60x200", lines[1])
	}
}

func TestPTYBrokerKeepsWarningForANonTerminalResizeFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		// An old tmux (resize-window predates 2.9) or a one-off exec failure: the next
		// frame may well succeed, so every occurrence is news.
		{"plain failure", errors.New("error resizing window: exit status 1")},
		// A TIMEOUT is not a death certificate. ResizeWindow returns ErrTmuxTimeout
		// precisely when the server is WEDGED and the session's state is UNKNOWN, and
		// its own contract (session/tmux/bounded_test.go) forbids reporting that as a
		// confirmed-gone session. Folding it into the once-only arm would hide a
		// wedged server behind a single line.
		{"wedged server", fmt.Errorf("%w: resize-window after 10s", tmux.ErrTmuxTimeout)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warnings := captureWarningLog(t)
			ch := &fakeClientlessChannel{resizeErr: tc.err}
			br := newPTYBroker(ch)
			for i := range goneResizeFrames {
				if err := br.resize(uint16(40+i), uint16(100+i)); err == nil {
					t.Fatalf("resize %d returned nil, want the apply error", i)
				}
			}
			if got := len(warningLinesContaining(warnings.String(), "apply resize")); got != goneResizeFrames {
				t.Fatalf("warnings = %d, want one per frame (%d)", got, goneResizeFrames)
			}
		})
	}
}

// warningLinesContaining returns the captured warning lines that mention needle,
// so a count is over the lines this test is about rather than over everything the
// broker happened to log.
func warningLinesContaining(captured, needle string) []string {
	var out []string
	for _, line := range strings.Split(captured, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return out
}

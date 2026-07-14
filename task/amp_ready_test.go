package task

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestIsAmpPromptFrameRealCapture pins BUG 1 to the ACTUAL bytes amp's ready pane
// produces. The fixtures are real `tmux capture-pane -p -e -J` captures (the same
// path af/WaitForReady uses, escapes preserved) of amp 0.0.1783988119: a truecolor
// escape wraps the mode label ("\x1b[38;2;61;255;166mmedium\x1b[39m") and a dim
// escape wraps the repo/branch on the bottom rule. Those escapes sit between the
// box-drawing glyphs, so the old box regex never matched in the wild — amp creates
// spun the full readiness timeout. The fix strips escapes first, then requires the
// labeled top rule AND the closing bottom rule, so the real ready frame matches and
// the blank loading pane / "Welcome to Amp" banner do not.
func TestIsAmpPromptFrameRealCapture(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    bool
	}{
		{"real amp ready frame (ANSI-colored)", "testdata/amp_ready.ansi", true},
		// Same frame after a turn spent tokens: amp interleaves a "$0.06" cost
		// indicator into the top rule ("╭── $0.06 ─ medium ─╮"). Detection must
		// tolerate that decoration, not regress to the timeout.
		{"amp ready frame with cost decoration", "testdata/amp_ready_cost.ansi", true},
		{"amp blank loading pane", "testdata/amp_loading.ansi", false},
		{"amp welcome banner, no input box", "testdata/amp_banner_only.ansi", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			content := string(raw)
			if got := isAmpPromptFrame(content); got != tc.want {
				t.Errorf("isAmpPromptFrame(%s) = %v, want %v", tc.fixture, got, tc.want)
			}
			// isReadyContent must agree via the amp branch — this is the seam
			// WaitForReady actually calls.
			if got := isReadyContent(content, "amp"); got != tc.want {
				t.Errorf("isReadyContent(%s, amp) = %v, want %v", tc.fixture, got, tc.want)
			}
		})
	}
}

// TestIsAmpPromptFrameRequiresBothRules guards the strictness the old comment
// asked for: a labeled top rule alone (a partial redraw, or the box top scrolled
// into stale scrollback) is not "accepting input" until the closing bottom rule is
// also present. Built from the real capture so the ANSI handling is exercised too.
func TestIsAmpPromptFrameRequiresBothRules(t *testing.T) {
	raw, err := os.ReadFile("testdata/amp_ready.ansi")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	lines := splitLines(string(raw))

	var topOnly, bottomOnly []string
	for _, ln := range lines {
		if !containsRune(ln, '╰') {
			topOnly = append(topOnly, ln)
		}
		if !containsRune(ln, '╭') {
			bottomOnly = append(bottomOnly, ln)
		}
	}

	if isAmpPromptFrame(joinLines(topOnly)) {
		t.Error("labeled top rule without the bottom rule must not count as ready")
	}
	if isAmpPromptFrame(joinLines(bottomOnly)) {
		t.Error("bottom rule without the labeled top rule must not count as ready")
	}
}

// TestWaitForReadyStopsPollingOnContextCancel pins BUG 2: an abandoned/cancelled
// create must tear the readiness poll down at once — never leave a capture-pane
// loop spinning after the caller gave up (the leak that pinned Sachin's daemon at
// ~50% CPU for 30+ minutes). The pane never reaches ready, so only cancellation
// can stop the loop; after WaitForReady returns, no further captures may happen.
func TestWaitForReadyStopsPollingOnContextCancel(t *testing.T) {
	defer setWaitForReadyTimingForTest(10*time.Second, 5*time.Millisecond)()
	// Pin a fast no-op limit detector so the (one-time, cold) config load in the
	// default detector can't delay the first poll and race the cancel below.
	defer setWaitLimitForTest(NewLimitDetector(nil), time.Now)()

	var captures atomic.Int64
	inst := newPreviewInstance(t, func() (string, error) {
		captures.Add(1)
		return "still starting up...\n", nil // never ready
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- WaitForReady(ctx, inst) }()

	// Wait until the poll has actually captured the pane, then abandon the create.
	deadline := time.Now().Add(2 * time.Second)
	for captures.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("poll never captured the pane before cancel")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled after abandon, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForReady did not return after context cancel — poll goroutine leaked")
	}

	// The poll goroutine is gone: snapshot the capture count, wait several poll
	// intervals, and assert it did not grow. A lingering spinner would keep
	// capturing.
	settled := captures.Load()
	time.Sleep(50 * time.Millisecond)
	if grew := captures.Load() - settled; grew != 0 {
		t.Fatalf("pane captured %d more time(s) after cancel — poll goroutine leaked", grew)
	}
}

// splitLines/joinLines/containsRune keep this test dependency-free (no strings
// import churn against the rest of the package's helpers).
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func joinLines(lines []string) string {
	out := ""
	for i, ln := range lines {
		if i > 0 {
			out += "\n"
		}
		out += ln
	}
	return out
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

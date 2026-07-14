package task

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
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

// TestIsAmpPromptFrameRequiresContiguousBox pins Greptile P2: the labeled top rule
// and the closing bottom rule must belong to ONE adjacent box. Separated fragments
// — an old frame's top border left in scrollback, unrelated output, then a bottom
// border — must not read as ready; one contiguous current box must.
func TestIsAmpPromptFrameRequiresContiguousBox(t *testing.T) {
	stale := "╭──────── medium ────────╮\n" +
		"some unrelated output line\n" +
		"more logs scrolled in between\n" +
		"╰──────── /tmp/repo (main) ────────╯\n"
	if isAmpPromptFrame(stale) {
		t.Error("separated top/bottom rules (stale scrollback) must not count as ready")
	}

	live := "╭──────── medium ────────╮\n" +
		"│ >                                        │\n" +
		"│                                          │\n" +
		"╰──────── /tmp/repo (main) ────────╯\n"
	if !isAmpPromptFrame(live) {
		t.Error("a contiguous current amp prompt box must count as ready")
	}
}

// TestWaitForReadyCancelReturnsDuringBlockingCapture pins Greptile P1: a cancel
// must return — and release the per-repo start lock — even when the poll is
// currently blocked inside a slow/wedged `tmux capture-pane`. Without the bounded,
// cancellable capture the create would stall inside the capture, holding the lock.
func TestWaitForReadyCancelReturnsDuringBlockingCapture(t *testing.T) {
	defer setWaitForReadyTimingForTest(10*time.Second, time.Millisecond)()
	defer setWaitLimitForTest(NewLimitDetector(nil), time.Now)()

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	inst := newPreviewInstance(t, func() (string, error) {
		once.Do(func() { close(entered) })
		<-release // simulate a capture that blocks for a long time
		return "still starting up...\n", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- WaitForReady(ctx, inst) }()

	<-entered // the capture is now blocked inside Preview
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled while a capture is blocked, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForReady stayed blocked on the in-flight capture after cancel — the start lock would stay held")
	}
	close(release) // let the abandoned capture goroutine finish and exit
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

	// The poll loop has exited; at most the single in-flight capture may still
	// complete, but no NEW capture may start. Let any in-flight goroutine drain,
	// snapshot, then assert the count is stable — a lingering spinner would keep
	// incrementing.
	time.Sleep(60 * time.Millisecond)
	settled := captures.Load()
	time.Sleep(80 * time.Millisecond)
	if grew := captures.Load() - settled; grew != 0 {
		t.Fatalf("pane captured %d more time(s) after cancel drained — poll goroutine leaked", grew)
	}
}

// ctxCaptureBackend is a fake backend whose agent-tab capture is context-bound
// (like the local tmux runtime). Its PreviewContext blocks until the capture's
// context is cancelled, then records that it observed the cancellation — standing
// in for a `tmux capture-pane` subprocess that gets torn down on cancel.
type ctxCaptureBackend struct {
	*session.FakeBackend
	entered  chan struct{}
	enterOne sync.Once
	ctxFired atomic.Bool
}

func (b *ctxCaptureBackend) PreviewContext(ctx context.Context, _ *session.Instance) (string, error) {
	b.enterOne.Do(func() { close(b.entered) })
	<-ctx.Done() // a real capture would be killed here via exec.CommandContext
	b.ctxFired.Store(true)
	return "", ctx.Err()
}

// TestWaitForReadyThreadsContextIntoCapture pins the last Greptile P1 tail: the
// capture itself must RECEIVE cancellation (so the subprocess is killed), not just
// have the wait race around it. On cancel, WaitForReady returns promptly AND the
// in-flight capture observes its context firing.
func TestWaitForReadyThreadsContextIntoCapture(t *testing.T) {
	defer setWaitForReadyTimingForTest(10*time.Second, time.Millisecond)()
	defer setWaitLimitForTest(NewLimitDetector(nil), time.Now)()

	backend := &ctxCaptureBackend{FakeBackend: session.NewFakeBackend(), entered: make(chan struct{})}
	restore := session.SetBackendFactoryForTest(func(_ session.InstanceOptions, _ string) (session.Backend, error) {
		return backend, nil
	})
	defer restore()
	inst, err := session.NewInstance(session.InstanceOptions{Title: "ctxcap", Path: t.TempDir(), Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- WaitForReady(ctx, inst) }()

	<-backend.entered // a capture is in flight, holding a context
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForReady did not return after cancel")
	}

	// The capture's own context must have fired — i.e. cancellation was threaded
	// into the capture, not just raced around it.
	deadline := time.Now().Add(time.Second)
	for !backend.ctxFired.Load() {
		if time.Now().After(deadline) {
			t.Fatal("capture did not receive context cancellation — ctx not threaded to the capture subprocess")
		}
		time.Sleep(2 * time.Millisecond)
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

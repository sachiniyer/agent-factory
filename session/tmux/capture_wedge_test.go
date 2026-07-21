package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
)

// #2099 / #2105: the CAPTURE and POLL paths kept using bare exec.Command (or
// exec.CommandContext with context.Background()) long after #1787/#1917 bounded
// the WS and teardown paths, so a wedged tmux server hung them forever. The two
// reported symptoms share one root cause:
//
//   - #2105: the daemon's per-second status poll calls HasUpdated →
//     CapturePaneContent → CapturePaneContentContext(context.Background()). The
//     daemon polls instances SEQUENTIALLY, so one wedged session froze the status
//     of every later instance — liveness, trust-prompt dismissal, and the
//     usage-limit detector all stop.
//   - #2099: CapturePaneContentWithOptions and the submit path's
//     capturePaneForDelivery ran capture-pane with no deadline at all.
//
// These tests drive a REAL fake tmux on PATH rather than a MockCmdExec that
// sleeps, and that is load-bearing rather than stylistic: a mock's Func blocks
// the CALLING GOROUTINE directly, which no context deadline can reach, so a
// blocking mock would "fail" identically before and after the fix and would
// prove nothing. The bound only exists because a real subprocess can be
// SIGKILLed on the deadline. stallingTmuxOnPath (bounded_test.go) additionally
// sleeps in a CHILD, so passing also requires boundedTmuxCommand's process-group
// kill and tmuxWaitDelay — a plain exec.CommandContext leaves the child holding
// the inherited stdout pipe and Output() stays blocked on pipe EOF, silently
// defeating the deadline.

// shortPasteDeliveryMaxWait tightens the submit path's own delivery budget so the
// tests below can assert in milliseconds. Returned to its previous value on
// cleanup.
func shortPasteDeliveryMaxWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := pasteDeliveryMaxWait
	pasteDeliveryMaxWait = d
	t.Cleanup(func() { pasteDeliveryMaxWait = prev })
}

// TestCaptureAndPollTmuxCommandsDoNotHang is the #2099/#2105 regression. Every
// capture/poll entry point must RETURN against a wedged server, and must report
// the wedge as ErrTmuxTimeout — never as ErrSessionGone.
func TestCaptureAndPollTmuxCommandsDoNotHang(t *testing.T) {
	cases := []struct {
		name string
		call func(ts *TmuxSession) error
	}{
		// #2105: the daemon status-poll capture. The headline hang.
		{"CapturePaneContent", func(ts *TmuxSession) error { _, err := ts.CapturePaneContent(); return err }},
		// The context form with a caller ctx that never fires: the deadline must
		// come from the package's own bound, not from the caller.
		{"CapturePaneContentContext", func(ts *TmuxSession) error {
			_, err := ts.CapturePaneContentContext(context.Background())
			return err
		}},
		// #2099: the scrollback capture behind Preview/Snapshot.
		{"CapturePaneContentWithOptions", func(ts *TmuxSession) error {
			_, err := ts.CapturePaneContentWithOptions("-", "-")
			return err
		}},
		// #2105 explicitly recommends bounding both taps: trust-prompt dismissal
		// runs on the same unsupervised daemon poll as HasUpdated, so a stall here
		// freezes the poll exactly like the capture.
		{"TapEnter", func(ts *TmuxSession) error { return ts.TapEnter() }},
		{"TapDAndEnter", func(ts *TmuxSession) error { return ts.TapDAndEnter() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stallingTmuxOnPath(t)
			shortTmuxTimeout(t, 200*time.Millisecond)
			ts := NewTmuxSessionWithDeps("wedge-2105", "sh", MakePtyFactory(), cmd.MakeExecutor())
			ts.setMonitor(newStatusMonitor())

			done := make(chan error, 1)
			go func() { done <- tc.call(ts) }()

			// Generous relative to the 200ms deadline, but far below the fake
			// tmux's 300s sleep: only a real bound can land inside it.
			select {
			case err := <-done:
				if !errors.Is(err, ErrTmuxTimeout) {
					t.Fatalf("want ErrTmuxTimeout against a stalled tmux, got %v", err)
				}
				// THE safety invariant. A tripped deadline means the server is
				// wedged and the session's real state is UNKNOWN. Callers act
				// destructively on "gone" (the daemon marks the instance Lost and
				// reaps its process trees), so reporting a merely-wedged server as
				// gone would tear down a live agent.
				if errors.Is(err, ErrSessionGone) {
					t.Fatalf("timeout must not be reported as ErrSessionGone: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("%s hung against a stalled tmux (#2099/#2105): no return within 30s — "+
					"the daemon polls instances sequentially, so this stalls every later instance's status", tc.name)
			}
		})
	}
}

// TestHasUpdatedDoesNotHangOnWedgedServer drives the #2105 symptom through the
// exact call the daemon makes. refreshInstanceStatus has no watchdog, so a hang
// here is unbounded and silent.
func TestHasUpdatedDoesNotHangOnWedgedServer(t *testing.T) {
	stallingTmuxOnPath(t)
	shortTmuxTimeout(t, 200*time.Millisecond)
	ts := NewTmuxSessionWithDeps("wedge-2105-poll", "sh", MakePtyFactory(), cmd.MakeExecutor())
	ts.setMonitor(newStatusMonitor())

	done := make(chan bool, 1)
	go func() {
		_, _, _ = ts.HasUpdated()
		done <- true
	}()

	select {
	case <-done:
		// The monitor must NOT have latched itself dead: that latch is reserved
		// for a CONFIRMED-gone session (ErrSessionGone), and a wedged server
		// confirms nothing. Latching on a timeout would permanently silence the
		// status monitor for a session that is merely slow.
		ts.monitorMu.Lock()
		dead := ts.monitor.dead
		ts.monitorMu.Unlock()
		if dead {
			t.Fatal("a wedged tmux server must not latch the status monitor dead: " +
				"the session is UNKNOWN, not confirmed gone, and the latch is permanent until respawn")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("HasUpdated hung against a stalled tmux (#2105): this runs on the daemon's " +
			"unsupervised per-second poll, which polls instances sequentially")
	}
}

// TestCapturePaneContentContextHonorsCallerCancellation guards the contract the
// #2105 fix must NOT regress while adding its own bound. task.WaitForReady passes
// a cancellable ctx and relies on cancellation tearing the in-flight capture down
// (see task/amp_ready_test.go). Adding an internal deadline must therefore DERIVE
// from the caller's ctx, not replace it — and a caller cancel must still surface
// as ctx.Err(), distinct from the package's own ErrTmuxTimeout, so an abandoned
// create is never misfiled as a wedged server.
func TestCapturePaneContentContextHonorsCallerCancellation(t *testing.T) {
	stallingTmuxOnPath(t)
	// Deliberately LONG relative to the cancel below: if the caller's cancel were
	// not threaded through, this test could only pass by waiting out our bound.
	shortTmuxTimeout(t, 30*time.Second)
	ts := NewTmuxSessionWithDeps("wedge-2105-cancel", "sh", MakePtyFactory(), cmd.MakeExecutor())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ts.CapturePaneContentContext(ctx)
		done <- err
	}()

	time.Sleep(100 * time.Millisecond) // let the capture get in flight
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled when the CALLER cancels, got %v", err)
		}
		// Caller cancellation is not a wedged server. WaitForReady distinguishes
		// them, and so must this.
		if errors.Is(err, ErrTmuxTimeout) {
			t.Fatalf("caller cancellation must not be reported as ErrTmuxTimeout: %v", err)
		}
		if errors.Is(err, ErrSessionGone) {
			t.Fatalf("caller cancellation must not be reported as ErrSessionGone: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("CapturePaneContentContext ignored the caller's cancellation — an abandoned " +
			"create would hold the per-repo start lock across the whole capture")
	}
}

// TestSubmitPathDoesNotHangOnWedgedServer is the #2099 submit half. Every tmux
// command in sendKeysPasteBuffer (the delivery captures, load-buffer,
// paste-buffer, delete-buffer, send-keys Enter) must be bounded, or a wedged
// server hangs the prompt-delivery path — which the daemon drives while holding
// the per-session op lock.
func TestSubmitPathDoesNotHangOnWedgedServer(t *testing.T) {
	stallingTmuxOnPath(t)
	shortTmuxTimeout(t, 200*time.Millisecond)
	shortPasteDeliveryMaxWait(t, 200*time.Millisecond)
	ts := NewTmuxSessionWithDeps("wedge-2099-submit", "sh", MakePtyFactory(), cmd.MakeExecutor())

	done := make(chan error, 1)
	go func() { done <- ts.SendKeysCommand("a prompt long enough to be distinctive") }()

	select {
	case err := <-done:
		// The delivery MUST fail loudly rather than silently claim success: the
		// prompt provably did not land on a wedged server, and #1982 is exactly
		// the bug class where a submit reports success with the prompt stranded.
		if err == nil {
			t.Fatal("SendKeysCommand reported success against a wedged tmux server: " +
				"nothing was delivered, so a nil error is a silent lost prompt (#1982 class)")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("SendKeysCommand hung against a stalled tmux (#2099): the submit path runs " +
			"under the daemon's per-session op lock, so the session becomes unpromptable")
	}
}

// TestPasteDeliveryLoopHonorsItsOwnBudget is subtlety (a) of the fix, and the
// half the issue's suggested patch gets WRONG.
//
// capturePaneForDelivery is called from waitForPasteDelivered's poll loop, whose
// entire budget is pasteDeliveryMaxWait. Bounding the capture at the package's
// flat tmuxCommandTimeout would let ONE capture overshoot that budget many times
// over, leaving the loop's `time.Now().After(deadline)` check unreachable for the
// whole tmuxCommandTimeout — i.e. the reported bug only half fixed. The capture
// must therefore be bounded by the loop's REMAINING budget (clamped to
// tmuxCommandTimeout), so the loop actually honors pasteDeliveryMaxWait.
func TestPasteDeliveryLoopHonorsItsOwnBudget(t *testing.T) {
	stallingTmuxOnPath(t)
	const (
		maxWait = 300 * time.Millisecond
		// An order of magnitude larger than maxWait: a flat tmuxCommandTimeout
		// bound on the capture is the failure this separates out, and it is only
		// visible when the two budgets differ.
		tmuxBound = 3 * time.Second
	)
	shortTmuxTimeout(t, tmuxBound)
	shortPasteDeliveryMaxWait(t, maxWait)
	ts := NewTmuxSessionWithDeps("wedge-2099-budget", "sh", MakePtyFactory(), cmd.MakeExecutor())

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		ts.waitForPasteDelivered(newDeliveryProbe("a-distinctive-tail"))
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		// Slack for process spawn/kill, but far below tmuxBound: a capture bounded
		// at tmuxCommandTimeout instead of the remaining loop budget lands at
		// ~3s and fails here.
		if limit := 4 * maxWait; elapsed >= limit {
			t.Fatalf("waitForPasteDelivered took %v (>= %v) against a stalled tmux: its captures are "+
				"bounded by tmuxCommandTimeout (%v) rather than the loop's own remaining budget (%v), "+
				"so the pasteDeliveryMaxWait deadline check stays unreachable", elapsed, limit, tmuxBound, maxWait)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("waitForPasteDelivered hung against a stalled tmux (#2099): the capture inside " +
			"its poll loop is unbounded, so the loop's deadline check never runs")
	}
}

// TestCleanupSessionsDoesNotHangOnWedgedServer covers the sweep's tmux commands
// (`tmux ls`, `show-environment`, `kill-session`). `af reset` runs this
// synchronously in a short-lived CLI process, so an unbounded stall here is a
// user-visible hang with no way out but ^C.
func TestCleanupSessionsDoesNotHangOnWedgedServer(t *testing.T) {
	stallingTmuxOnPath(t)
	shortTmuxTimeout(t, 200*time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- CleanupSessions(cmd.MakeExecutor()) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrTmuxTimeout) {
			t.Fatalf("want ErrTmuxTimeout against a stalled tmux, got %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("CleanupSessions hung against a stalled tmux: `af reset` runs it synchronously")
	}
}

// TestSessionHomeMarkerDoesNotHangOnWedgedServer covers the ownership probe the
// sweep runs once PER discovered session. It is best-effort (no error to return),
// so the contract is simply that it returns — and returns "no marker", since a
// wedged server proves nothing about ownership and the sweep's safe direction is
// to leave a session it cannot prove it owns (#1122).
func TestSessionHomeMarkerDoesNotHangOnWedgedServer(t *testing.T) {
	stallingTmuxOnPath(t)
	shortTmuxTimeout(t, 200*time.Millisecond)

	type result struct {
		home string
		ok   bool
	}
	done := make(chan result, 1)
	go func() {
		home, ok := sessionHomeMarker(cmd.MakeExecutor(), "af_wedged")
		done <- result{home, ok}
	}()

	select {
	case got := <-done:
		if got.ok {
			t.Fatalf("a wedged server must not be read as proof of ownership, got home %q", got.home)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("sessionHomeMarker hung against a stalled tmux: CleanupSessions calls it once " +
			"per discovered session, so one wedge stalls the whole sweep")
	}
}

// healthyTmuxOnPath installs a fake tmux that answers immediately, standing in
// for a healthy server. `load-buffer` copies its stdin to stdinSink so the tests
// can prove the paste payload survives the bounded path intact.
func healthyTmuxOnPath(t *testing.T, stdinSink string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"load-buffer\" ]; then cat > " + stdinSink + "; exit 0; fi\n" +
		"if [ \"$1\" = \"display-message\" ]; then echo '3 7'; exit 0; fi\n" +
		"echo 'pane line'\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestBoundedCapturesSucceedWhenTmuxIsHealthy guards the other direction: the new
// deadlines must not break the normal path. A regression that always trips the
// timeout, or that mis-reads a fast clean exit as a deadline, fails here.
func TestBoundedCapturesSucceedWhenTmuxIsHealthy(t *testing.T) {
	sink := filepath.Join(t.TempDir(), "stdin.txt")
	healthyTmuxOnPath(t, sink)
	shortTmuxTimeout(t, 10*time.Second)
	ts := NewTmuxSessionWithDeps("healthy-2099", "sh", MakePtyFactory(), cmd.MakeExecutor())
	ts.setMonitor(newStatusMonitor())

	if got, err := ts.CapturePaneContent(); err != nil || got != "pane line\n" {
		t.Fatalf("CapturePaneContent: got %q err %v", got, err)
	}
	if got, err := ts.CapturePaneContentContext(context.Background()); err != nil || got != "pane line\n" {
		t.Fatalf("CapturePaneContentContext: got %q err %v", got, err)
	}
	if got, err := ts.CapturePaneContentWithOptions("-", "-"); err != nil || got != "pane line\n" {
		t.Fatalf("CapturePaneContentWithOptions: got %q err %v", got, err)
	}
	if err := ts.TapEnter(); err != nil {
		t.Fatalf("TapEnter: %v", err)
	}
	if err := ts.TapDAndEnter(); err != nil {
		t.Fatalf("TapDAndEnter: %v", err)
	}
	if content, ok := ts.capturePaneForDelivery(); !ok || content != "pane line\n" {
		t.Fatalf("capturePaneForDelivery: got %q ok %v", content, ok)
	}
}

// TestLoadBufferStdinSurvivesTheBoundedPath is the load-buffer half of the fix.
// It is the package's only tmux command that STREAMS a payload in on stdin, and
// WaitDelay force-closes inherited pipes (#856/#896) — including the one exec
// creates for a non-*os.File stdin. A bound that truncates the payload would
// submit a silently mangled prompt, which is strictly worse than the hang it
// replaces, so assert the whole prompt reaches tmux byte-for-byte.
func TestLoadBufferStdinSurvivesTheBoundedPath(t *testing.T) {
	sink := filepath.Join(t.TempDir(), "stdin.txt")
	healthyTmuxOnPath(t, sink)
	shortTmuxTimeout(t, 10*time.Second)
	shortPasteDeliveryMaxWait(t, 100*time.Millisecond)
	ts := NewTmuxSessionWithDeps("healthy-2099-load", "sh", MakePtyFactory(), cmd.MakeExecutor())

	// Large enough that the copy is a real multi-write stream rather than a
	// single small write that could pass by luck.
	prompt := strings.Repeat("prompt payload that must survive intact. ", 500)
	if err := ts.SendKeysCommand(prompt); err != nil {
		t.Fatalf("SendKeysCommand against a healthy tmux: %v", err)
	}
	got, err := os.ReadFile(sink)
	if err != nil {
		t.Fatalf("read load-buffer stdin sink: %v", err)
	}
	if string(got) != prompt {
		t.Fatalf("load-buffer stdin truncated by the bounded path: got %d bytes, want %d", len(got), len(prompt))
	}
}

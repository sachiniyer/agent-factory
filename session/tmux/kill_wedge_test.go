package tmux

import (
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// #1917: a session kill that wedges forever, leaving the session permanently
// undeletable until the daemon restarts.
//
// daemon.KillSession runs the whole teardown with no deadline of its own while
// holding a per-session kills-in-flight guard, so ANY unbounded step below is
// enough to reproduce the reported symptom. Before the fix every tmux command on
// the teardown path used bare exec.Command: display-message (panePID),
// list-panes (SessionProcessTrees), kill-session and has-session (Close). Each is
// now bounded by tmuxCommandTimeout.
//
// These tests drive a REAL fake tmux on PATH rather than a MockCmdExec that
// sleeps, and that is load-bearing, not a stylistic choice: a mock's RunFunc
// blocks the calling goroutine directly and is not reachable by ANY context
// deadline, so a blocking mock would "fail" identically before and after the fix
// and prove nothing. The bound only exists because a real subprocess can be
// SIGKILLed on the deadline — so the test has to exercise a real subprocess.
// stallingTmuxOnPath (bounded_test.go) additionally sleeps in a CHILD, so
// passing also requires the process-group kill and tmuxWaitDelay.

// TestTeardownTmuxCommandsDoNotWedgeTheKill is the #1917 regression. Every tmux
// command reachable from the kill teardown must return against a wedged server.
func TestTeardownTmuxCommandsDoNotWedgeTheKill(t *testing.T) {
	cases := []struct {
		name string
		// call runs one teardown entry point and returns its error (nil for the
		// best-effort probes, which report through their return value instead).
		call func(ts *TmuxSession) error
	}{
		// The whole teardown call the daemon actually makes (session/teardown.go).
		{"CloseAndWaitForPaneExit", func(ts *TmuxSession) error { _, err := ts.CloseAndWaitForPaneExit(); return err }},
		// Close alone: list-panes + kill-session (+ the has-session probe).
		{"Close", func(ts *TmuxSession) error { _, err := ts.Close(); return err }},
		// The first command on the teardown, and the one whose stall the issue
		// misread as "capped at 3s" (paneExitWait bounds only waitForPIDExit).
		{"panePID", func(ts *TmuxSession) error { _, err := ts.panePID(); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stallingTmuxOnPath(t)
			shortTmuxTimeout(t, 200*time.Millisecond)
			ts := NewTmuxSessionWithDeps("wedge-1917", "sh", MakePtyFactory(), cmd.MakeExecutor())

			done := make(chan error, 1)
			go func() { done <- tc.call(ts) }()

			// Generous relative to the 200ms deadline, but far below the fake
			// tmux's 300s sleep: only a real bound can land inside it.
			select {
			case err := <-done:
				if !errors.Is(err, ErrTmuxTimeout) {
					t.Fatalf("want ErrTmuxTimeout against a stalled tmux, got %v", err)
				}
				// A wedged server proves nothing about the session, so the
				// teardown must never claim it confirmed the session gone —
				// that would let a caller reap a live agent's process tree.
				if errors.Is(err, ErrSessionGone) {
					t.Fatalf("timeout must not be reported as ErrSessionGone: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("%s hung against a stalled tmux (#1917): no return within 30s — "+
					"the daemon's kills-in-flight guard is held across this call, so the session "+
					"would be undeletable until the daemon restarts", tc.name)
			}
		})
	}
}

// TestSessionProcessTreesDoesNotWedgeTheKill covers the teardown's other tmux
// command. It is best-effort (nil on any failure) rather than error-returning, so
// it gets its own test: the contract is that it RETURNS, and returns nil — a
// wedged server has told us nothing about which processes are leaked, and reaping
// on a guess would SIGKILL a live session's tree.
func TestSessionProcessTreesDoesNotWedgeTheKill(t *testing.T) {
	stallingTmuxOnPath(t)
	shortTmuxTimeout(t, 200*time.Millisecond)

	done := make(chan []proctree.Process, 1)
	go func() { done <- SessionProcessTrees(cmd.MakeExecutor(), "wedge-1917") }()

	select {
	case procs := <-done:
		if len(procs) != 0 {
			t.Fatalf("a stalled list-panes must yield no reap set, got %d processes", len(procs))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("SessionProcessTrees hung against a stalled tmux (#1917): it is the FIRST tmux " +
			"command on the kill teardown, so a stall here wedges the kill before kill-session runs")
	}
}

// TestExistsOrUnknownOnWedgedServerReportsPresent pins the deliberate choice in
// sessionExists: the bool cannot express "unknown", so a tripped deadline reports
// TRUE. Every caller that acts destructively acts on FALSE (Close reaps the
// session's process trees; io.go/clientless.go raise ErrSessionGone, which the
// daemon reads as a confirmed death), so a false "gone" against a merely-wedged
// server would tear down a live session. A false "exists" only costs a
// best-effort skip. Returning quickly is the fix; returning TRUE is the safety
// property that keeps the fix from trading a hang for data loss.
func TestExistsOrUnknownOnWedgedServerReportsPresent(t *testing.T) {
	stallingTmuxOnPath(t)
	shortTmuxTimeout(t, 200*time.Millisecond)
	ts := NewTmuxSessionWithDeps("wedge-1917-probe", "sh", MakePtyFactory(), cmd.MakeExecutor())

	done := make(chan bool, 1)
	go func() { done <- ts.ExistsOrUnknown() }()

	select {
	case exists := <-done:
		if !exists {
			t.Fatal("a wedged tmux server must not be reported as a gone session: " +
				"callers reap process trees and raise ErrSessionGone on false")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ExistsOrUnknown hung against a stalled tmux (#1917): it is the fallback probe " +
			"on nearly every tmux error path in this package, including Close's")
	}
}

// TestCloseOnWedgedServerSkipsTheExistenceProbe guards the rule
// tmuxTimeoutContext documents: after kill-session trips its deadline, Close must
// NOT fall back to has-session. The probe would spawn another tmux command
// against the same wedged server and hang identically, so the bound would buy
// nothing. Timing is the only observable: with the probe, teardown costs a second
// full deadline.
func TestCloseOnWedgedServerSkipsTheExistenceProbe(t *testing.T) {
	stallingTmuxOnPath(t)
	const bound = 400 * time.Millisecond
	shortTmuxTimeout(t, bound)
	ts := NewTmuxSessionWithDeps("wedge-1917-probe-skip", "sh", MakePtyFactory(), cmd.MakeExecutor())

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_, _ = ts.Close()
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		// Close spends one deadline on list-panes and one on kill-session. A
		// third would mean the has-session probe ran on the timeout path.
		if max := 3 * bound; elapsed >= max {
			t.Fatalf("Close took %v (>= %v): kill-session's timeout path appears to have fallen "+
				"back to an ExistsOrUnknown probe, which hangs on the same wedged server", elapsed, max)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close hung against a stalled tmux (#1917)")
	}
}

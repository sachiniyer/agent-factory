package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// The #1917 review locks for the two places a tmux TIMEOUT used to be silently
// downgraded to an ordinary failure, letting the caller delete a workspace while
// the session's fate was unknown.

// wedgeOn returns an executor that stalls past the deadline for the tmux verbs in
// wedged and answers every other verb via fast.
func wedgeOn(wedged []string, fast func(args []string) error) cmd_test.MockCmdExec {
	isWedged := func(c *exec.Cmd) bool {
		for _, w := range wedged {
			for _, a := range c.Args {
				if a == w {
					return true
				}
			}
		}
		return false
	}
	run := func(c *exec.Cmd) error {
		if isWedged(c) {
			time.Sleep(2 * time.Second)
			return fmt.Errorf("wedged tmux server never answered %s", strings.Join(c.Args, " "))
		}
		return fast(c.Args)
	}
	return cmd_test.MockCmdExec{
		RunFunc:    run,
		OutputFunc: func(c *exec.Cmd) ([]byte, error) { return nil, run(c) },
	}
}

// TestCloseAndWaitForPaneExit_PanePIDTimeout_KeepsTheUnknown is review finding (2).
//
// display-message times out while the kill-session that follows SUCCEEDS. The old
// code threw pidErr away and returned Close's nil error, so the caller was told
// the teardown was clean — but the #802 pane-exit wait had been skipped, because
// we never learned which process to wait for. The caller then removed the worktree
// while the HUP'd agent was still flushing into it.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: state comes back KNOWN with a nil error.
func TestCloseAndWaitForPaneExit_PanePIDTimeout_KeepsTheUnknown(t *testing.T) {
	// Trip the bound fast: these tests are about WHICH answer a tripped deadline
	// produces, not how long it takes to trip.
	shortTmuxTimeout(t, 150*time.Millisecond)
	// Only display-message stalls; kill-session and list-panes answer cleanly.
	exec := wedgeOn([]string{"display-message"}, func([]string) error { return nil })
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_pidtimeout", "claude", NewMockPtyFactory(t), exec)

	state, err := session.CloseAndWaitForPaneExit()

	if state != PaneStateUnknown {
		t.Fatal("a timed-out panePID reported a KNOWN state: the pane-exit wait was skipped (we never " +
			"learned which process to wait for), so the caller deletes the worktree while the HUP'd " +
			"agent is still flushing into it (#1917 review)")
	}
	if err == nil || !errors.Is(err, ErrTmuxTimeout) {
		t.Fatalf("the timeout must stay reachable through errors.Is for callers that gate on it, got: %v", err)
	}
}

// TestClose_HasSessionProbeTimeout_ReportsUnknown is review finding (4).
//
// kill-session fails FAST (so Close probes has-session to tell "already gone" from
// "refused to die"), and the probe — newly bounded — times out. sessionExists
// collapsed that to `true`, so Close reported only the original ordinary
// kill-session error and the caller proceeded to worktree cleanup with the
// session's liveness unknown.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: state KNOWN, and the error does not wrap
// ErrTmuxTimeout.
func TestClose_HasSessionProbeTimeout_ReportsUnknown(t *testing.T) {
	// Trip the bound fast: these tests are about WHICH answer a tripped deadline
	// produces, not how long it takes to trip.
	shortTmuxTimeout(t, 150*time.Millisecond)
	// kill-session answers with a failure immediately; the has-session probe stalls.
	exec := wedgeOn([]string{"has-session"}, func(args []string) error {
		for _, a := range args {
			if a == "kill-session" {
				return fmt.Errorf("exit status 1")
			}
		}
		return nil
	})
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_probetimeout", "claude", NewMockPtyFactory(t), exec)

	state, err := session.Close()

	if state != PaneStateUnknown {
		t.Fatal("a timed-out has-session probe reported a KNOWN state: Close then hands the caller an " +
			"ordinary kill failure, and the caller cleans up the worktree without ever establishing " +
			"whether the session is still running (#1917 review)")
	}
	if err == nil || !errors.Is(err, ErrTmuxTimeout) {
		t.Fatalf("the probe's timeout must reach the caller as a timeout, got: %v", err)
	}
}

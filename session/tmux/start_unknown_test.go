package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// TestStart_ReadinessTimeoutOnWedgedServer_PropagatesTheUnknown is #1917 round-7
// finding (2): Start dropped its cleanup Close's PaneStateUnknown and flattened the
// error with %v.
//
// The path that matters: has-session answers "no" (so Start proceeds), the spawn
// SUCCEEDS — a detached session now exists and its pane may be RUNNING in the
// worktree — and then the server wedges, so the readiness poll times out and the
// cleanup Close cannot confirm the session's fate. LocalBackend.Launch calls
// gw.Cleanup() the instant Start returns an error, so if this error does not carry
// the unknown, the brand-new worktree is deleted out from under that live pane.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the returned error does not wrap ErrTmuxTimeout
// (the state was dropped, and %v erased the sentinel even where it existed).
func TestStart_ReadinessTimeoutOnWedgedServer_PropagatesTheUnknown(t *testing.T) {
	shortTmuxTimeout(t, 150*time.Millisecond)

	// has-session: answers fast and says "gone", so Start gets past its
	// already-exists gate and spawns. Everything else stalls past the deadline —
	// the readiness poll never sees the session, and the cleanup Close cannot
	// confirm whether the pane it just spawned is running.
	execu := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			for _, a := range c.Args {
				if a == "has-session" {
					return fmt.Errorf("exit status 1") // answered: no such session
				}
			}
			time.Sleep(2 * time.Second)
			return fmt.Errorf("wedged tmux server never answered")
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("wedged tmux server never answered")
		},
	}
	session := NewTmuxSessionFromSanitizedNameWithDeps("af_wedged_start", "claude", NewMockPtyFactory(t), execu)

	err := session.Start(t.TempDir())

	if err == nil {
		t.Fatal("a Start whose readiness poll never sees its session must fail")
	}
	if !errors.Is(err, ErrTmuxTimeout) {
		t.Fatalf("Start dropped the unknown pane state: its cleanup Close could not confirm whether "+
			"the session it just spawned is dead, but the error does not say so — LocalBackend.Launch "+
			"then deletes the brand-new worktree out from under a possibly-live pane (#1917 round 7). "+
			"got: %v", err)
	}
}

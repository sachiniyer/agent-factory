package tmux

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"syscall"
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

// TestClose_HasSessionProbeTimeout_PreservesKillSessionErrno is the lost-restore
// errno-classifier pin for the "kill-session answered with a failure, post-kill
// has-session probe did not answer" branch of close (#5295/#5296/#5418).
//
// Sibling to TestClose_HasSessionProbeTimeout_ReportsUnknown above: the same
// branch, but kill-session fails with a fork/exec errno instead of an ordinary
// "exit status 1". The daemon's lostRestoreEnvironmentalClass classifier resolves
// "environmental" (unbounded wait) vs "structural" (bounded give-up, durable at
// lostRestoreMaxAttempts=6) through errors.Is over the WRAPPED chain — never
// message text (daemon/lostrestore_environmental.go). When kill-session fails
// with a fork/exec errno (EAGAIN under RLIMIT_NPROC / cgroup pids.max, ENOMEM
// under a kernel OOM window, ...), that errno is the environmental evidence the
// classifier reads; flattening it with %v severed the errno from the chain, so
// the classifier saw only the bare ErrTmuxTimeout sentinel (structural by
// design) and the loop gave up on a RECOVERABLE session under the exact
// resource pressure the errno class was built for — the #5295 incident failure
// mode the classifier exists to prevent.
//
// This is the close-path counterpart of session/cleanup_respawned_account_tabs_
// errno_test.go (the #5418 fix): it drives the real TmuxSession.Close through
// the same wedgeOn/cmdExec seam the existing close-branch tests use, fails
// kill-session with the real os/exec *fs.PathError shape os.StartProcess
// surfaces from a refused clone, wedges the has-session probe past the bound,
// and asserts the errno stays reachable through errors.Is after close wraps it.
func TestClose_HasSessionProbeTimeout_PreservesKillSessionErrno(t *testing.T) {
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"EAGAIN", syscall.EAGAIN},
		{"ENOMEM", syscall.ENOMEM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Trip the bound fast: this test is about WHICH chain a tripped
			// deadline's branch preserves, not how long it takes to trip.
			shortTmuxTimeout(t, 150*time.Millisecond)
			// *fs.PathError is what os/exec surfaces from a failed fork/exec, and
			// its Unwrap returns the wrapped errno; the daemon's classifier walks
			// the chain via errors.Is, so the errno must stay reachable through
			// that unwrap rather than as text in a formatted message.
			killErrno := &fs.PathError{Op: "fork/exec", Path: "/usr/bin/tmux", Err: tc.errno}
			// kill-session fails FAST with the errno (the deadline does not trip,
			// so r.tmux returns it verbatim); the has-session probe then stalls
			// past the bound, so close takes the "!ok" branch that wraps the
			// kill-session error into the ErrTmuxTimeout carrier.
			exec := wedgeOn([]string{"has-session"}, func(args []string) error {
				for _, a := range args {
					if a == "kill-session" {
						return killErrno
					}
				}
				return nil
			})
			session := NewTmuxSessionFromSanitizedNameWithDeps(
				"af_killerrnoprobe", "claude", NewMockPtyFactory(t), exec)

			state, err := session.Close()

			// The branch's own contract (#1917), unchanged by the fix: a timed-out
			// has-session probe leaves the session's fate UNKNOWN, and the
			// ErrTmuxTimeout sentinel must stay reachable for callers that gate
			// on it.
			if state != PaneStateUnknown {
				t.Fatalf("state = %v, want PaneStateUnknown: a timed-out has-session probe after a "+
					"fast kill-session failure must not report the fate as established (#1917)", state)
			}
			if err == nil || !errors.Is(err, ErrTmuxTimeout) {
				t.Fatalf("error = %v, want the ErrTmuxTimeout sentinel to survive the wrap so callers "+
					"that gate on it still classify a timeout (#1917)", err)
			}
			// The lost-restore classifier pin: the fork/exec errno the kill-session
			// path produced must stay reachable through errors.Is after close wraps
			// it, so lostRestoreEnvironmentalClass resolves lostRestoreEnvErrno and
			// the loop waits on the environment instead of charging the structural
			// give-up budget. With the %v flatten the errno survived only as prose
			// and errors.Is could not match it; the negative control below pins
			// that the classifier genuinely walks the unwrap chain rather than text.
			if !errors.Is(err, tc.errno) {
				t.Errorf("error = %v, want errors.Is(err, %v) = true: the kill-session fork/exec errno must "+
					"stay reachable through the wrap so lostRestoreEnvironmentalClass classifies "+
					"environmental (unbounded wait) rather than structural (durable give-up at "+
					"lostRestoreMaxAttempts=6) — the #5295 property the classifier exists to enforce",
					err, tc.errno)
			}
			// The branch's diagnostic shape must stay present so observers read the
			// failure origin.
			if !strings.Contains(err.Error(), "has-session probe after kill-session failed") {
				t.Errorf("error = %v, want the branch's diagnostic to name the has-session probe after "+
					"the kill-session failure", err)
			}

			// Negative control: the errno's text present inside a plain string
			// error must NOT classify via errors.Is — pinning that the classifier
			// matches the unwrap chain, never prose. Without this property the
			// original %v wrap would have read as environmental from text, masking
			// the bug.
			plainText := fmt.Errorf("tmux command timed out: has-session probe after kill-session "+
				"failed (fork/exec /usr/bin/tmux: %s)", tc.errno.Error())
			if errors.Is(plainText, tc.errno) {
				t.Errorf("the classifier must not match the errno from message text alone — the "+
					"negative control for the unwrap-chain contract (plainText=%v)", plainText)
			}
		})
	}
}

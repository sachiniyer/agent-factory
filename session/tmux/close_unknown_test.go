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

// fastProbeFailureExec models the conjunction no TestClose* coverage ever staged:
// kill-session fails FAST, AND the post-kill has-session probe fails FAST with an
// error that is NOT positive evidence of absence. Both are the real *fs.PathError
// os.StartProcess surfaces from a refused clone under pids.max / RLIMIT_NPROC —
// the #7269 class — so the tmux server and session are untouched and alive. The
// lossy probe this path used to run collapsed that probe failure into "answered:
// absent" (probeSessionContext: "any other error ... conflated with absence"),
// so no switch arm touched gone, and Close returned (PaneStateKnown, nil) while
// firing revokeLaunchCapabilities on a still-alive session (#4731). The strict
// probe returns (false, false, err) for a fast failure, routing it to the !ok
// arm exactly like the timed-out probe.
//
// countLets tests assert both fast failures actually happened (kill-session
// invocations and has-session probe invocations).
func fastProbeFailureExec(errno syscall.Errno, killCalls, probeCalls *int) cmd_test.MockCmdExec {
	probeErr := &fs.PathError{Op: "fork/exec", Path: "/usr/bin/tmux", Err: errno}
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			s := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(s, "kill-session"):
				if killCalls != nil {
					*killCalls++
				}
				return probeErr
			case strings.Contains(s, "has-session"):
				if probeCalls != nil {
					*probeCalls++
				}
				return probeErr
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			// probeSessionStrict runs the post-kill has-session through the Output
			// seam (outputTmuxBoundedWith), so the FAST non-absence failure must
			// surface here — not as the plain error the lossy Run probe swallowed.
			if strings.Contains(strings.Join(c.Args, " "), "has-session") {
				if probeCalls != nil {
					*probeCalls++
				}
				return nil, probeErr
			}
			return []byte(""), nil
		},
	}
}

// TestClose_HasSessionProbeFastFailure_ReportsUnknown is the core regression for
// the bug: when kill-session fails fast and the post-kill has-session probe ALSO
// fails fast with a non-absence error, the lossy probe collapsed that probe
// failure into "answered: absent", so gone kept its initialized true: Close
// swallowed the kill error entirely and returned (PaneStateKnown, nil) — a clean
// false success — and revocation fired on a still-alive session.
//
// Sibling to TestClose_HasSessionProbeTimeout_PreservesKillSessionErrno above: the
// SAME branch, but the probe FAILS FAST instead of wedging past the bound. With
// the strict probe the fast failure takes the !ok -> PaneStateUnknown refusal
// branch (error surfaced with the kill error intact, no revocation), the #4731
// keep-rule ("a close that could not tell keeps it").
func TestClose_HasSessionProbeFastFailure_ReportsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"EAGAIN", syscall.EAGAIN},
		{"ENOMEM", syscall.ENOMEM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var killCalls, probeCalls int
			exec := fastProbeFailureExec(tc.errno, &killCalls, &probeCalls)
			session := NewTmuxSessionFromSanitizedNameWithDeps(
				"af_fast_probe_fail", "claude", NewMockPtyFactory(t), exec)

			state, err := session.Close()

			requireEqual(t, int(1), killCalls,
				"Close must still attempt the kill-session")
			requireEqual(t, int(1), probeCalls,
				"the post-kill has-session probe must run exactly once (via the strict Output seam)")
			if state != PaneStateUnknown {
				t.Fatalf("state = %v, want PaneStateUnknown: a fast probe failure that is not positive "+
					"evidence of absence must not let Close report the fate as established — tmux never "+
					"answered that the session is gone (#4731)", state)
			}
			if err == nil {
				t.Fatalf("error = nil, want a non-nil error surfacing the kill failure: pre-fix the lossy " +
					"probe swallowed the kill error and returned (PaneStateKnown, nil) for a session that " +
					"was never proved gone")
			}
			if !errors.Is(err, tc.errno) {
				t.Errorf("error = %v, want errors.Is(err, %v) = true: the kill-session fork/exec errno must "+
					"stay reachable through the wrap (the lost-restore classifier reads it via errors.Is)",
					err, tc.errno)
			}
			if errors.Is(err, ErrTmuxTimeout) {
				t.Errorf("error = %v, the strict probe's fast failure must NOT be wrapped as ErrTmuxTimeout: "+
					"only a probe that actually timed out carries that sentinel (#1917); a fast failure is "+
					"named for what it is", err)
			}
			if !strings.Contains(err.Error(), "has-session probe after kill-session failed") {
				t.Errorf("error = %v, want the branch's diagnostic to name the has-session probe after the "+
					"kill-session failure", err)
			}
		})
	}
}

// TestClose_HasSessionProbeFastFailure_DoesNotReapCapturedPaneProcess pins the
// destructive reuse of the bug's false success: in the fork/exec EAGAIN class the
// pre-teardown capture SUCCEEDS (the capture is a tmux client command that
// fork/exec'd before the pressure window), so the captured pane tree of the
// still-alive session lands in `leaked`. Pre-fix, no arm matched (false, true) so
// `leaked` was never nil'ed and the on-request reaper signalled the live captured
// tree of a session whose teardown officially never took effect (#4731). The
// strict probe routes the fast failure to the !ok arm, which nil's `leaked`, so
// the reaper never touches the survivor.
//
// Two properties of the fixture are load-bearing (#7321):
//
//   - The child's exit is observed through a WAIT. A killed child nobody has
//     waited on is a zombie, and a zombie still answers signal 0, so a signal-0
//     liveness check passes against the very reap it is there to catch.
//   - The child leads its OWN kernel session. This test binary is named
//     tmux.test, so its direct children pass the capture's "child of a tmux
//     server" check, and the capture then adds every member of the root's
//     kernel session. Without Setsid that session is the one `go test` runs in,
//     and a regression has the reaper TERM/KILL the test runner and the shell
//     that launched it instead of failing this assertion.
func TestClose_HasSessionProbeFastFailure_DoesNotReapCapturedPaneProcess(t *testing.T) {
	shrinkReapWaits(t)

	// A live child of this test stands in for the captured pane root of an agent
	// whose tmux session is still running. list-panes reports it so the
	// pre-teardown capture puts it in `leaked`; kill-session and the post-kill
	// has-session probe both fail fast with EAGAIN, so the session is untouched.
	child := exec.Command("sleep", "300")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		t.Fatalf("start live pane stand-in: %v", err)
	}
	// The only wait on the child, so its result is the one place an exit shows up.
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		<-exited
	})

	probeErr := &fs.PathError{Op: "fork/exec", Path: "/usr/bin/tmux", Err: syscall.EAGAIN}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			s := strings.Join(c.Args, " ")
			if strings.Contains(s, "kill-session") {
				return probeErr
			}
			if strings.Contains(s, "has-session") {
				return probeErr
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			s := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(s, "has-session"):
				return nil, probeErr
			case strings.Contains(s, "list-panes"):
				// The pre-teardown capture succeeds and names the live child, so
				// `leaked` is non-empty going into the kill/probe branch.
				return []byte(fmt.Sprintf("%d\n", child.Process.Pid)), nil
			}
			return []byte(""), nil
		},
	}

	// Not vacuous: the capture Close is about to run must actually admit the
	// child. If it did not, `leaked` would be empty and no reaper could ever
	// start, whatever the probe arm did with it.
	captured, err := CaptureSessionProcessTrees(cmdExec, "af_fast_probe_reap")
	if err != nil {
		t.Fatalf("the pre-teardown capture must succeed for this fixture: %v", err)
	}
	admitted := false
	for _, p := range captured {
		if p.PID == child.Process.Pid {
			admitted = true
		}
	}
	if !admitted {
		t.Fatalf("the pre-teardown capture did not admit the stand-in pane process %d (captured %v): "+
			"the reap assertion below would pass without testing anything", child.Process.Pid, captured)
	}

	session := newTmuxSession("af_fast_probe_reap", "sh", NewMockPtyFactory(t), cmdExec)
	state, err := session.Close()
	if state != PaneStateUnknown {
		t.Fatalf("state = %v, want PaneStateUnknown for a teardown whose probe could not answer", state)
	}
	if err == nil {
		t.Fatalf("error = nil, want the kill failure surfaced")
	}

	// The async on-request reaper (Close is the interactive path) TERMs the
	// captured tree after reapGraceWait and is finished within reapGraceWait +
	// reapTermWait + one final second. `sleep` dies on the TERM, so a reap shows
	// up as the wait returning inside that window; outliving it is the pass.
	window := reapTestSlack*(reapGraceWait+reapTermWait) + time.Second
	select {
	case waitErr := <-exited:
		// Hand the result back so the cleanup's receive does not block.
		exited <- waitErr
		t.Fatalf("the live captured pane process was reaped by a teardown that never took effect "+
			"(wait returned: %v): a close that could not tell whether the session was gone must not "+
			"signal its captured pane tree (#4731)", waitErr)
	case <-time.After(window):
	}
}

// requireEqual is a testify-free equality assertion for the int comparisons in
// these tests; close_unknown_test.go deliberately stays on the bare t.Fatal style
// of its siblings rather than pulling in testify.
func requireEqual(t *testing.T, want, got int, msg string) {
	t.Helper()
	if want != got {
		t.Fatalf("%s: got %d, want %d", msg, got, want)
	}
}

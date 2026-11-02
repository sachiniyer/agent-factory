package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// delayedLaunchExitPtyFactory is a trackedPtyFactory whose commandDone
// delivers a nonzero launch exit after a short delay. The delay lets the
// readiness poll's synchronous has-session probe block past the 2s
// sessionReadyDeadline so that, by the time the probe returns and the
// select runs, BOTH commandDone and the deadline channel are ready — the
// race the fix addresses. A pre-populated channel (delay 0) works equally
// well: the select does not run until the synchronous probe returns, so the
// pending exit is still in the channel at the select regardless.
type delayedLaunchExitPtyFactory struct {
	t       *testing.T
	waitErr error
	delay   time.Duration
}

func (f delayedLaunchExitPtyFactory) Start(*exec.Cmd) (*os.File, error) {
	f.t.Fatal("Start called instead of StartTracked")
	return nil, nil
}

func (f delayedLaunchExitPtyFactory) StartTracked(*exec.Cmd) (*os.File, <-chan error, error) {
	ptmx, err := os.CreateTemp(f.t.TempDir(), "deadline-race-pty")
	if err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() {
		time.Sleep(f.delay)
		done <- f.waitErr
		close(done)
	}()
	return ptmx, done, nil
}

// raceReadinessPollExec is a cmd_test.MockCmdExec whose has-session call
// counting reproduces the both-ready race: the FIRST has-session (Start's
// pre-spawn gate) answers "not found" fast so the create proceeds; the
// SECOND (the readiness poll's first and only probe) sleeps past the 2s
// sessionReadyDeadline so the deadline fires while the probe is still
// blocking; every later has-session (the post-timeout cleanup) answers
// "not found" fast. Other tmux commands (kill-session, set-option, etc.)
// succeed fast so the cleanup path does not confound the timing.
type raceReadinessPollExec struct {
	hasSessionCalls atomic.Int32
}

func (e *raceReadinessPollExec) RunFunc(c *exec.Cmd) error {
	for _, arg := range c.Args {
		if arg == "has-session" {
			n := e.hasSessionCalls.Add(1)
			if n == 2 {
				// Readiness poll: block past the 2s deadline so both
				// commandDone and the deadline channel are ready at the
				// select. The mock does not respect the context deadline
				// (it is not a real subprocess), so the probe returns at
				// 2.1s with ctx.Err() != nil — probeSessionContext
				// reports (false, false), i.e. "not known", and the loop
				// falls through to the select.
				time.Sleep(2100 * time.Millisecond)
			}
			return errors.New("session not found")
		}
	}
	return nil
}

func (e *raceReadinessPollExec) OutputFunc(c *exec.Cmd) ([]byte, error) {
	// show-options: "no server running" so importClientEnvironmentArgs
	// takes the no-server fast path. display-message: empty output (no
	// pane — the determinate-empty branch). list-panes: empty output (no
	// processes). All return (nil, nil) so the cleanup path completes
	// without confounding the race.
	return nil, nil
}

// TestStartDeadlineRaceDrainsLateNonzeroLaunchExit is the regression for the
// race the report describes: in the readiness poll's select, the
// commandDone launch-exit channel and the <-timeout deadline channel can
// both be ready at the same select (the exit lands while the synchronous
// has-session probe is still blocking past the 2s deadline). Before the fix
// Go's pseudo-random select could pick <-timeout, silently discarding the
// pending nonzero launch exit and classifying identical facts (launch
// answered nonzero, session absent) as the ErrSessionNotStarted
// worktree-deletion marker — the nondeterministic deletion the commandErr
// case's invariant exists to prevent.
//
// With the fix the <-timeout arm drains commandDone non-blockingly and
// reuses the same launch-error classification, so the both-ready race
// resolves deterministically in favor of the actionable cause regardless of
// which arm Go picks.
//
// The harness runs the race repeatedly: with the fix every iteration
// classifies the nonzero exit as the actionable launch error (outside
// ErrSessionNotStarted). Without the fix ~50% of iterations pick <-timeout
// and misclassify as ErrSessionNotStarted, so the loop catches the
// regression reliably.
func TestStartDeadlineRaceDrainsLateNonzeroLaunchExit(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)

	const iterations = 5
	for i := 0; i < iterations; i++ {
		t.Run(fmt.Sprintf("race_%d", i), func(t *testing.T) {
			exec := &raceReadinessPollExec{}
			cmdExec := cmd_test.MockCmdExec{
				RunFunc:    exec.RunFunc,
				OutputFunc: exec.OutputFunc,
			}
			ptyFactory := delayedLaunchExitPtyFactory{
				t:       t,
				waitErr: errors.New("new-session exited with status 7"),
				delay:   100 * time.Millisecond,
			}
			ts := NewTmuxSessionWithDeps("af_deadline_race", "sh", ptyFactory, cmdExec)

			err := ts.Start(t.TempDir())
			if err == nil {
				t.Fatal("Start succeeded after its launch command exited non-zero")
			}
			if errors.Is(err, ErrSessionNotStarted) {
				t.Fatalf("a nonzero launch exit that straddled the readiness deadline was misclassified as "+
					"ErrSessionNotStarted (the worktree-deletion marker) instead of the actionable launch "+
					"error — the both-ready race discarded the pending exit: %v", err)
			}
			if !strings.Contains(err.Error(), "new-session exited with status 7") {
				t.Fatalf("the actionable launch exit status was lost from the error chain: expected the "+
					"launch error, got: %v", err)
			}
		})
	}
}

// TestStartDeadlineRaceCarriesSocketFSProbeEvidence verifies the drain in the
// timeout arm reuses the full launch-error classification — including the
// socketFSProbe errno join the commandErr case performs — so a real resource
// verdict still rides the chain on the race path instead of being lost as
// the timeout-only classification skips it (#5295 review).
func TestStartDeadlineRaceCarriesSocketFSProbeEvidence(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	old := socketFSProbe
	socketFSProbe = func(string) error {
		return &os.PathError{Op: "write", Path: "/tmp", Err: syscall.ENOSPC}
	}
	defer func() { socketFSProbe = old }()

	exec := &raceReadinessPollExec{}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    exec.RunFunc,
		OutputFunc: exec.OutputFunc,
	}
	ptyFactory := delayedLaunchExitPtyFactory{
		t:       t,
		waitErr: errors.New("tmux new-session exited with status 1"),
		delay:   100 * time.Millisecond,
	}
	ts := NewTmuxSessionWithDeps("af_deadline_race_enospc", "sh", ptyFactory, cmdExec)

	err := ts.Start(t.TempDir())
	if err == nil {
		t.Fatal("Start succeeded after its launch command exited non-zero")
	}
	if errors.Is(err, ErrSessionNotStarted) {
		t.Fatalf("the deadline race misclassified a nonzero launch exit as ErrSessionNotStarted: %v", err)
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("a launch failure on a full socket filesystem must carry ENOSPC in the chain even on the "+
			"deadline-race path — the drain must reuse the full launch-error classification including the "+
			"socketFSProbe join: %v", err)
	}
}

// TestStartDeadlineRacePreservesWorktreeInvariant is the deletion-flip
// regression: the commandErr case's invariant comment requires that every
// answered post-spawn failure is returned OUTSIDE ErrSessionNotStarted "so
// LocalBackend preserves the worktree". The timeout arm's drain must honor
// the same invariant — identical facts (launch answered nonzero, session
// absent) must not flip between "worktree preserved" and "worktree deleted"
// on goroutine scheduling. This test asserts the invariant directly: the
// error from a raced-away nonzero exit is NOT ErrSessionNotStarted, which
// is the marker LocalBackend.launch gates worktree deletion on
// (session/backend_local.go).
func TestStartDeadlineRacePreservesWorktreeInvariant(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)

	exec := &raceReadinessPollExec{}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    exec.RunFunc,
		OutputFunc: exec.OutputFunc,
	}
	ptyFactory := delayedLaunchExitPtyFactory{
		t:       t,
		waitErr: errors.New("systemd-run --user --scope failed: exit status 1"),
		delay:   100 * time.Millisecond,
	}
	ts := NewTmuxSessionWithDeps("af_deadline_invariant", "sh", ptyFactory, cmdExec)

	err := ts.Start(t.TempDir())
	if err == nil {
		t.Fatal("Start succeeded after its launch command exited non-zero")
	}
	if errors.Is(err, ErrSessionNotStarted) {
		t.Fatalf("the deadline race flipped the worktree-deletion invariant: an answered nonzero launch "+
			"exit paired with session absence must stay outside ErrSessionNotStarted so LocalBackend "+
			"preserves the worktree, but the timeout arm returned the deletion marker: %v", err)
	}
}

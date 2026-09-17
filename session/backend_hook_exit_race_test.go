package session

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hookDeadlineContext is a context.Context whose deadline fires on demand:
// calling trigger() closes Done() and makes Err() return
// context.DeadlineExceeded. os/exec's watchCtx reads c.ctx.Err() to build the
// error it splices, so this is the only way to produce context.DeadlineExceeded
// without relying on a wall-clock timeout that can fire before the script even
// starts. The archive-hook sibling (daemon/archive_hook_outcome_test.go) uses
// the same device; the type is unexported there, so this package carries its
// own copy rather than exporting test machinery across the boundary.
type hookDeadlineContext struct {
	done chan struct{}
	once sync.Once
}

func newHookDeadlineContext() (context.Context, func()) {
	dc := &hookDeadlineContext{done: make(chan struct{})}
	return dc, func() { dc.once.Do(func() { close(dc.done) }) }
}

func (dc *hookDeadlineContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (dc *hookDeadlineContext) Done() <-chan struct{}       { return dc.done }
func (dc *hookDeadlineContext) Value(_ any) any             { return nil }
func (dc *hookDeadlineContext) Err() error {
	select {
	case <-dc.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// The defect #4411 shares with #4284's archive hook: a hook script that exits 0
// in the same instant its timeout fires can be reported as
// context.DeadlineExceeded — os/exec's watchCtx splices ctx.Err() over the nil
// result when its <-ctx.Done() arm wins and Cancel (Process.Kill) answers nil
// on the zombie. ProcessState is the kernel's record of what the script did,
// and a clean exit must outrank whatever the context did afterwards.
//
// The race is arranged deterministically, not by timing:
//
//  1. hookScriptMakeContext injects the hookDeadlineContext, so the spliced
//     error is context.DeadlineExceeded at a precise instant rather than a
//     wall-clock accident.
//  2. hookScriptBeforeStart hands the script fd 3 via cmd.ExtraFiles. The
//     script never touches it; the kernel closes it only when the process
//     image is destroyed — after the exit code is already fixed — so EOF on
//     the read side is proof of exit that a descheduled shell cannot fake the
//     way an EXIT-trap marker could.
//  3. hookScriptAfterStart observes that EOF, fires the deadline, then waits
//     for the production Cancel to run. watchCtx calls Cancel only from its
//     ctx.Done() arm, so observing it proves the arm was taken while it was
//     the only selectable one — the spliced ctxResult is queued before Wait
//     can offer the competing receive. If that arm is never observed the test
//     fails rather than passing vacuously.
//  4. The assertion is positive: the run must report success. "Does not
//     contain deadline text" would pass whether the guard fired or the splice
//     window never opened; asserting nil forces the test to fail when the
//     guard is absent.
//
// The default Cancel is Process.Kill, which answers nil on a zombie — no
// straggler is needed to keep the kill non-ESRCH, unlike the archive hook's
// group-wide kill that needed a survivor in the group.
func TestHookScript_ExitZeroAtDeadlineReportsSuccess(t *testing.T) {
	script := writeHookScript(t, filepath.Join(t.TempDir(), "hook.sh"), "exit 0")

	exitR, exitW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = exitR.Close() })

	dc, triggerDeadline := newHookDeadlineContext()
	origMakeCtx := hookScriptMakeContext
	t.Cleanup(func() { hookScriptMakeContext = origMakeCtx })
	hookScriptMakeContext = func(time.Duration) (context.Context, context.CancelFunc) {
		return dc, triggerDeadline
	}

	origBeforeStart := hookScriptBeforeStart
	t.Cleanup(func() { hookScriptBeforeStart = origBeforeStart })
	hookScriptBeforeStart = func(cmd *exec.Cmd) {
		require.Empty(t, cmd.ExtraFiles,
			"this test wires its exit pipe to fd 3 — the production command grew ExtraFiles")
		cmd.ExtraFiles = []*os.File{exitW}
	}

	cancelRan := make(chan struct{})
	var cancelOnce sync.Once
	guardFired := false
	origAfterStart := hookScriptAfterStart
	t.Cleanup(func() { hookScriptAfterStart = origAfterStart })
	hookScriptAfterStart = func(cmd *exec.Cmd, _ context.CancelFunc) {
		// cmd.Cancel exists only after Start — the CommandContext default is
		// installed inside it — so the wrap lives in the afterStart seam.
		prodCancel := cmd.Cancel
		require.NotNil(t, prodCancel)
		cmd.Cancel = func() error {
			err := prodCancel()
			cancelOnce.Do(func() { close(cancelRan) })
			return err
		}
		// The parent holds its own copy of the write end; EOF can never arrive
		// while it stays open, so close it before reading.
		_ = exitW.Close()
		exited := make(chan error, 1)
		go func() {
			_, readErr := exitR.Read(make([]byte, 1))
			exited <- readErr
		}()
		select {
		case readErr := <-exited:
			if !errors.Is(readErr, io.EOF) {
				return
			}
		case <-time.After(10 * time.Second):
			// The script provably never died; leave guardFired false so the
			// test fails rather than hanging or passing vacuously.
			return
		}
		// The script is dead and its exit status is already fixed. Fire the
		// deadline, then wait for the production Cancel to run — proof that
		// watchCtx took its ctx.Done() arm before Wait could offer the
		// competing receive, so the spliced ctxResult is queued ahead of a
		// clean result.
		triggerDeadline()
		select {
		case <-cancelRan:
			guardFired = true
		case <-time.After(10 * time.Second):
			// watchCtx never ran Cancel; the splice window never opened.
		}
	}

	out, cmd, err := runHookScriptWithResolvedEnvironment(time.Minute, script, "", nil, nil)
	require.True(t, guardFired,
		"seam did not observe script-exit followed by Cancel — the test exercised nothing")
	require.NotNil(t, cmd)
	require.NoError(t, err,
		"a script that exited 0 must report success even when the deadline fired in the same instant")
	assert.NotNil(t, cmd.ProcessState)
	require.True(t, cmd.ProcessState.Exited() && cmd.ProcessState.ExitCode() == 0,
		"the kernel's record must say the script really did exit 0")
	_ = out
}

// The other half of the rule: a nonzero exit the script reached on its own is
// still a failure, never a clean exit the guard could drop. The splice cannot
// apply — os/exec replaces only a NIL result — so the *ExitError surfaces and
// the fired deadline still wraps it in DeadlineExceeded: the conservative
// classification for a teardown caller that cannot prove the workspace
// absorbed the answer.
func TestHookScript_NonzeroExitAtDeadlineStillFails(t *testing.T) {
	script := writeHookScript(t, filepath.Join(t.TempDir(), "hook.sh"), "exit 23")

	exitR, exitW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = exitR.Close() })

	dc, triggerDeadline := newHookDeadlineContext()
	origMakeCtx := hookScriptMakeContext
	t.Cleanup(func() { hookScriptMakeContext = origMakeCtx })
	hookScriptMakeContext = func(time.Duration) (context.Context, context.CancelFunc) {
		return dc, triggerDeadline
	}

	origBeforeStart := hookScriptBeforeStart
	t.Cleanup(func() { hookScriptBeforeStart = origBeforeStart })
	hookScriptBeforeStart = func(cmd *exec.Cmd) {
		require.Empty(t, cmd.ExtraFiles)
		cmd.ExtraFiles = []*os.File{exitW}
	}

	cancelRan := make(chan struct{})
	var cancelOnce sync.Once
	windowOpened := false
	origAfterStart := hookScriptAfterStart
	t.Cleanup(func() { hookScriptAfterStart = origAfterStart })
	hookScriptAfterStart = func(cmd *exec.Cmd, _ context.CancelFunc) {
		prodCancel := cmd.Cancel
		require.NotNil(t, prodCancel)
		cmd.Cancel = func() error {
			err := prodCancel()
			cancelOnce.Do(func() { close(cancelRan) })
			return err
		}
		_ = exitW.Close()
		exited := make(chan error, 1)
		go func() {
			_, readErr := exitR.Read(make([]byte, 1))
			exited <- readErr
		}()
		select {
		case readErr := <-exited:
			if !errors.Is(readErr, io.EOF) {
				return
			}
		case <-time.After(10 * time.Second):
			return
		}
		triggerDeadline()
		select {
		case <-cancelRan:
			windowOpened = true
		case <-time.After(10 * time.Second):
		}
	}

	_, _, err = runHookScriptWithResolvedEnvironment(time.Minute, script, "", nil, nil)
	require.True(t, windowOpened,
		"seam did not observe script-exit followed by Cancel — the test exercised nothing")
	require.Error(t, err, "a script that exited 23 failed, whatever the clock did")
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"the conservative classification for an answer the workspace may not have absorbed")
	assert.Contains(t, err.Error(), "exit status 23",
		"the script's real exit status must still surface inside the report")
}

// The failure direction the guard must never take: a script still running when
// the deadline fires IS a timeout — SIGKILLed, ProcessState.Exited() false, and
// the wrapped DeadlineExceeded is what lets callers like reap classify the
// workspace state as unknown rather than trusting a success that never
// happened (#2529).
func TestHookScript_StillRunningAtDeadlineReportsDeadline(t *testing.T) {
	script := writeHookScript(t, filepath.Join(t.TempDir(), "hook.sh"), "sleep 30")

	start := time.Now()
	_, _, err := runHookScriptWithResolvedEnvironment(200*time.Millisecond, script, "", nil, nil)

	require.Error(t, err, "a script that never finished must be reported")
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"a script killed at the deadline is exactly what the timeout wrap is for")
	assert.Less(t, time.Since(start), 10*time.Second,
		"the SIGKILL must land promptly — no post-deadline wait on a dead script")
}

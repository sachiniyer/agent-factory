package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/hooklog"
)

// The outcome an archive hook reports must be the outcome it HAD (#3407). The
// hook runs at the teardown chokepoint of a committed operation, and this single
// error string is the whole of what an operator learns about it — surfaced as the
// `warning` on a successful archive, and logged. A wrong one sends them looking
// for a hook that hung when what actually happened was a hook that finished, or a
// hook that failed for a nameable reason.
//
// The original misreport came from a capture pipe held by a backgrounded child:
// cmd.Run waited on that pipe after the hook shell exited, and a deadline that
// elapsed in the wait replaced the hook's real outcome. #4010's direct output
// file removes that wait altogether. The tests below retain the straggler and
// pin both halves of the replacement contract: return promptly with the shell's
// real outcome, but still kill the straggler before the worktree moves.

// withArchiveHookTimeout shortens the hook deadline for one test.
func withArchiveHookTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	original := onArchiveHookTimeout
	onArchiveHookTimeout = d
	t.Cleanup(func() { onArchiveHookTimeout = original })
}

func archiveHookContext(t *testing.T) onArchiveHookContext {
	t.Helper()
	return onArchiveHookContext{
		sessionID:   "test-id",
		title:       "test-session",
		repoRoot:    t.TempDir(),
		worktree:    t.TempDir(),
		archivePath: t.TempDir(),
	}
}

// A successful hook is done when its shell exits. A backgrounded child may keep
// the output file open, but unlike a capture pipe that descriptor cannot delay
// cmd.Run until the deadline.
func TestArchiveHook_OutputFileStragglerDoesNotDelaySuccess(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeOnArchiveCommand(t, fmt.Sprintf("printf output; touch %q; sleep 30 >&1 2>&1 &", marker))
	withArchiveHookTimeout(t, 500*time.Millisecond)

	start := time.Now()
	err := runOnArchiveHook(archiveHookContext(t))
	elapsed := time.Since(start)

	require.NoError(t, err, "the hook exited successfully; its output-file straggler is cleanup, not failure")
	assert.FileExists(t, marker, "the hook must actually have run, or this test proves nothing")
	assert.Less(t, elapsed, onArchiveHookWaitDelay,
		"a descendant holding the direct output file must not recreate the old capture-pipe wait")
}

// deadlineContext is a context.Context whose deadline fires on demand: calling
// triggerDeadline() closes Done() and makes Err() return context.DeadlineExceeded.
// os/exec's watchCtx reads c.ctx.Err() to build the error it splices, so this
// is the only way to produce context.DeadlineExceeded without relying on a
// wall-clock timeout that can fire before the shell even starts.
// Calling triggerDeadline() more than once is safe and idempotent.
type deadlineContext struct {
	done chan struct{}
	once sync.Once
}

func newDeadlineContext() (context.Context, func()) {
	dc := &deadlineContext{
		done: make(chan struct{}),
	}
	trigger := func() {
		dc.once.Do(func() { close(dc.done) })
	}
	return dc, trigger
}

func (dc *deadlineContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (dc *deadlineContext) Done() <-chan struct{}       { return dc.done }
func (dc *deadlineContext) Value(_ any) any             { return nil }
func (dc *deadlineContext) Err() error {
	select {
	case <-dc.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// The straggler test above uses a generous 500ms deadline so the shell's
// microsecond exit lands far inside it, but the os/exec race this guards
// lives where the shell's natural exit and the deadline coincide. When the
// deadline fires the same instant the shell exits 0, os/exec's watchCtx can
// splice context.DeadlineExceeded over the nil exit-0 error (its `<-ctx.Done()`
// arm reaching `if err == nil && watch.err != nil { err = watch.err }`). The
// backgrounded `sleep 30` keeps the process group alive, so Cancel()'s
// group-wide SIGKILL returns 0 instead of ESRCH — pushing it into the
// `return nil` arm that lets the splice through, rather than os.ErrProcessDone
// which suppresses it.
//
// This test arranges the race deterministically:
//
//  1. A custom context (deadlineContext) is injected via onArchiveHookMakeContext.
//     Its Err() returns context.DeadlineExceeded when triggered — not
//     context.Canceled — so watchCtx splices the exact error the production
//     guard suppresses.
//
//  2. The shell's death is observed through a pipe, not inferred from a clock
//     or a marker. onArchiveHookBeforeStart hands the shell a pipe write end
//     as fd 3 (cmd.ExtraFiles); the kernel closes it only when the process
//     image is destroyed, after the exit code is already fixed — so EOF is
//     proof of exit that a descheduled shell cannot fake the way an EXIT-trap
//     marker followed by a sleep could. The straggler's `3>&-` keeps it from
//     inheriting fd 3, or its survival would hold the pipe open for thirty
//     seconds past the shell's exit.
//
//  3. watchCtx must take its ctx.Done() arm while it is the ONLY selectable
//     one. Once cmd.Wait blocks on `<-c.ctxResult`, the `resultc <- ctxResult{}`
//     arm becomes selectable too and Go picks randomly — a deadline triggered
//     but not yet consumed at that point splices only half the time. The seam
//     therefore also wraps cmd.Cancel and waits for it to run: the production
//     group-kill executes inside the Done arm, so observing it proves the arm
//     was taken and ctxResult{DeadlineExceeded} is queued before Wait reads.
//
//  4. The assertion is POSITIVE: runOnArchiveHook must return nil. "Does not
//     contain the string" is not used, because that assertion passes whether the
//     guard fired or the window never opened. Asserting nil forces the test to
//     fail when the guard is absent (the spliced error becomes a non-nil return).
//
// Verification: removing the guard at archive_hook.go:245 makes this test fail
// with `context deadline exceeded` for a hook that exited 0 — the original bug.
func TestArchiveHook_ExitZeroWithStragglerNeverReportsContextDeadline(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// The straggler keeps the process group — and the output file — alive after
	// the shell exits, so the group-wide SIGKILL in Cancel returns 0 rather
	// than ESRCH. `3>&-` keeps it off the exit pipe: fd 3 must report the
	// SHELL's death alone.
	writeOnArchiveCommand(t, "sleep 30 >&1 2>&1 3>&- &")

	// exitW reaches the hook shell as fd 3 via cmd.ExtraFiles. The shell never
	// writes to it and never closes it, so the kernel closes it during process
	// teardown — after exit_code is already fixed, moments before the process
	// becomes a zombie. EOF on exitR is therefore a kernel-level "the shell is
	// dead" signal, not a guess that it has had time to die.
	exitR, exitW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = exitR.Close() })

	// Inject a context whose deadline fires on command, not on a wall-clock
	// budget. The standard WithTimeout context would give context.Canceled on
	// cancel() and context.DeadlineExceeded only on natural expiry; we need the
	// latter at a precise moment relative to the shell's exit.
	dc, triggerDeadline := newDeadlineContext()

	origMakeCtx := onArchiveHookMakeContext
	t.Cleanup(func() { onArchiveHookMakeContext = origMakeCtx })
	onArchiveHookMakeContext = func() (context.Context, context.CancelFunc) {
		return dc, triggerDeadline
	}

	cancelRan := make(chan struct{})
	var cancelOnce sync.Once
	origBeforeStart := onArchiveHookBeforeStart
	t.Cleanup(func() { onArchiveHookBeforeStart = origBeforeStart })
	onArchiveHookBeforeStart = func(cmd *exec.Cmd) {
		require.Empty(t, cmd.ExtraFiles,
			"this test wires its exit pipe to fd 3 — the production command grew ExtraFiles")
		cmd.ExtraFiles = []*os.File{exitW}
		prodCancel := cmd.Cancel
		require.NotNil(t, prodCancel)
		cmd.Cancel = func() error {
			err := prodCancel()
			cancelOnce.Do(func() { close(cancelRan) })
			return err
		}
	}

	guardFired := false
	origAfterStart := onArchiveHookAfterStart
	t.Cleanup(func() { onArchiveHookAfterStart = origAfterStart })
	onArchiveHookAfterStart = func(_ *exec.Cmd, _ context.CancelFunc) {
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
			// The shell provably never died; leave guardFired false so the test
			// fails rather than hanging or passing vacuously.
			return
		}
		// The shell is dead and its exit status is already fixed. Fire the
		// deadline, then wait for the production Cancel to run — that proves
		// watchCtx took its ctx.Done() arm before Wait could offer the
		// competing receive, so the spliced ctxResult{DeadlineExceeded} is
		// queued ahead of a clean result.
		triggerDeadline()
		select {
		case <-cancelRan:
			guardFired = true
		case <-time.After(10 * time.Second):
			// watchCtx never ran Cancel; the splice window never opened.
		}
	}

	err = runOnArchiveHook(archiveHookContext(t))
	require.True(t, guardFired, "seam did not observe shell-exit followed by Cancel — the test exercised nothing")
	require.NoError(t, err, "guard must suppress context.DeadlineExceeded for an exit-0 hook")
}

// The same shape with a failing shell: the exit status and output are the
// outcome, and an output-file straggler cannot hold that answer past the clock.
func TestArchiveHook_OutputFileStragglerDoesNotHideExitStatus(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	writeOnArchiveCommand(t, "printf 'prune failed loudly'; sleep 30 >&1 2>&1 & exit 23")
	withArchiveHookTimeout(t, 500*time.Millisecond)

	start := time.Now()
	err := runOnArchiveHook(archiveHookContext(t))
	elapsed := time.Since(start)

	require.Error(t, err, "a hook that exits 23 failed, whatever the clock did")
	assert.Contains(t, err.Error(), "exit status 23",
		"the operator must be told what the hook actually reported")
	assert.Contains(t, err.Error(), "prune failed loudly",
		"the hook's own output is the diagnosis and must not be replaced by a timeout")
	assert.NotContains(t, err.Error(), "timed out",
		"the hook exited on its own terms; nothing timed out")
	assert.Less(t, elapsed, onArchiveHookWaitDelay,
		"a descendant holding the direct output file must not recreate the old capture-pipe wait")
}

// The guard against fixing the misreport by never reporting a timeout: a hook
// that is genuinely still running when the deadline fires is killed, and that IS
// a timeout. ProcessState.Exited() is what separates the two — false here,
// because the deadline's SIGKILL ended the shell rather than the shell exiting.
func TestArchiveHook_HookRunningAtTheDeadlineIsStillATimeout(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	writeOnArchiveCommand(t, "printf 'still working'; sleep 30")
	withArchiveHookTimeout(t, 500*time.Millisecond)

	err := runOnArchiveHook(archiveHookContext(t))

	require.Error(t, err, "a hook that never finished must be reported")
	assert.Contains(t, err.Error(), "timed out after 500ms",
		"a hook still running at the deadline is exactly what the timeout message is for")
	assert.Contains(t, err.Error(), "still working",
		"the timeout must still carry whatever the hook managed to say")
}

func TestArchiveHook_FailureSurfacesBoundedTailAndLogPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	payloadPath := filepath.Join(t.TempDir(), "hook-output")
	payload := "discarded beginning\n" + strings.Repeat("x", hooklog.TailLimit) + "\nkept ending\n"
	require.NoError(t, os.WriteFile(payloadPath, []byte(payload), 0o600))
	writeOnArchiveCommand(t, fmt.Sprintf("cat %q; exit 23", payloadPath))

	err := runOnArchiveHook(archiveHookContext(t))

	require.Error(t, err)
	logs, globErr := filepath.Glob(filepath.Join(home, "logs", "hooks", "on-archive-*.log"))
	require.NoError(t, globErr)
	require.Len(t, logs, 1)
	for _, want := range []string{"exit status 23", logs[0], hooklog.ExcerptPrefix + "kept ending"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.NotContains(t, err.Error(), "discarded beginning",
		"the surfaced diagnostic must stay bounded")
	// The report reaches the daemon log and every archive caller (#4853): one
	// line, then at most ExcerptLines quoted lines, each prefixed.
	lines := strings.Split(err.Error(), "\n")
	assert.LessOrEqual(t, len(lines)-1, hooklog.ExcerptLines, "the report quotes a bounded excerpt, not the output")
	for _, line := range lines[1:] {
		assert.True(t, strings.HasPrefix(line, hooklog.ExcerptPrefix), "unprefixed quoted line %q", line)
	}
	full, readErr := os.ReadFile(logs[0])
	require.NoError(t, readErr)
	assert.Contains(t, string(full), "discarded beginning")
	assert.Contains(t, string(full), "kept ending")
}

func TestArchiveHook_SuccessRemovesOutputLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeOnArchiveCommand(t, "printf success")

	require.NoError(t, runOnArchiveHook(archiveHookContext(t)))

	logs, err := filepath.Glob(filepath.Join(home, "logs", "hooks", "on-archive-*.log"))
	require.NoError(t, err)
	assert.Empty(t, logs, "successful archive hook retained an output log")
}

// End to end through the surface an operator actually reads. A committed archive
// carries its hook's outcome as a warning, so a false timeout is not an internal
// detail — it is what `af sessions archive` prints, and what the daemon log keeps.
func TestArchiveSession_OutputFileStragglerCommitsWithoutAFalseTimeoutWarning(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, srcPath := registerArchivable(t, manager, repoID, repoPath, "worker")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	writeOnArchiveCommand(t, fmt.Sprintf("printf pruned; touch %q; sleep 30 >&1 2>&1 &", marker))
	withArchiveHookTimeout(t, 500*time.Millisecond)

	archivedPath, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})

	require.NoError(t, err, "the hook completed; the archive must report a clean outcome")
	assert.FileExists(t, marker, "the hook must actually have run")
	assert.Equal(t, inst.ID, archived.ID)
	assert.False(t, exists(srcPath), "the archive itself must still have committed")
	assert.True(t, exists(archivedPath))
}

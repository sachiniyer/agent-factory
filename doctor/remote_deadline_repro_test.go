package doctor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestRemoteCoderWhoami_NonZeroExitNearDeadlineNotReportedAsTimeout
// reproduces the classification bug in checkCoderStatus: when `coder whoami`
// self-exits non-zero (e.g. a 401) and a pipe-holding descendant pushes
// CombinedOutput past the context deadline, the old code checked
// ctx.Err() == DeadlineExceeded and labelled it "timed out" even though the
// process had already completed with a real error message. The fix uses
// ExitError.ExitCode() < 0 (signal-killed) combined with the ctxKilled flag
// (set only by cmd.Cancel when Kill succeeds) as the timeout discriminator,
// so a non-zero self-exit (ExitCode >= 0) is reported as "failed" with its
// output regardless of whether the caller's timer also fired. This also
// handles the race where Kill returns nil for a zombie that already exited
// with a non-zero code: ExitCode >= 0 takes precedence over ctxKilled, so
// the diagnostics are preserved rather than mislabeled as a timeout.
//
// The probe timeout and wait delay are injected so the test does not depend on
// the 3s production deadline: a 1s timeout with a 2s wait delay gives the
// process a wide margin to self-exit before the deadline while still
// guaranteeing the deadline fires before WaitDelay closes the pipe.
func TestRemoteCoderWhoami_NonZeroExitNearDeadlineNotReportedAsTimeout(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	binDir := t.TempDir()
	// The fake coder exits non-zero after 100ms. A pipe-holding descendant
	// (sleep 30 &) keeps CombinedOutput's pipe readers alive past the 1s
	// deadline, making ctx.Err() return DeadlineExceeded even though the
	// process itself self-exited. The 2s wait delay guarantees the deadline
	// fires before WaitDelay closes the pipe.
	//
	// The backgrounded sleep survives the test as an orphan (WaitDelay closes
	// the inherited pipes but does not terminate descendants), so the script
	// records the child PID and t.Cleanup reaps it.
	pidFile := filepath.Join(dir, "sleep.pid")
	writeExecutable(t, binDir, "coder",
		"#!/bin/sh\n"+
			"sleep 0.1\n"+
			"echo 'Error: 401 Unauthorized' >&2\n"+
			"sleep 30 &\n"+
			"echo $! > \""+pidFile+"\"\n"+
			"exit 1\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return
		}
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGKILL)
		}
	})

	hook := writeHookScript(t, dir, "coder-hook.sh", "#!/bin/sh\necho '[]'\n")
	hooks := &config.RemoteHooks{LaunchCmd: hook, DeleteCmd: hook}

	opts := withRemote(testOptions(t, false), hooks)
	opts.coderProbeTimeout = time.Second
	opts.coderProbeWaitDelay = 2 * time.Second

	report, err := Run(opts)
	require.NoError(t, err)

	checks := findCheckRows(report, "coder")
	require.Len(t, checks, 1)
	require.Equal(t, StatusWarn, checks[0].Status)
	require.NotContains(t, checks[0].Detail, "timed out",
		"coder exited non-zero with an error; it did not time out")
	require.Contains(t, checks[0].Detail, "failed")
	require.Contains(t, checks[0].Detail, "Error: 401 Unauthorized")
	require.Equal(t, "run `coder login`", checks[0].Remediation)
	require.False(t, checks[0].Problem, "coder auth warnings must not fail doctor")
	require.Zero(t, report.UnresolvedCount(), "coder auth warnings must not fail doctor")
}

// TestRemoteCoderWhoami_GenuineTimeoutStillReportedAsTimeout is the no-regression
// guard for the classification-fix above: a `coder whoami` that genuinely hangs
// past the deadline (no descendant, SIGKILLed by the context) must still be
// reported as "timed out". ExitError.ExitCode() returns -1 for a signal-terminated
// process, so the fix's check (< 0) must keep classifying this as a timeout.
func TestRemoteCoderWhoami_GenuineTimeoutStillReportedAsTimeout(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	binDir := t.TempDir()
	// exec replaces the shell with sleep so CommandContext's SIGKILL reaches the
	// sleep process directly (rather than killing the shell and orphaning sleep
	// for 5 minutes).
	writeExecutable(t, binDir, "coder", "#!/bin/sh\nexec sleep 300\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	hook := writeHookScript(t, dir, "coder-hook.sh", "#!/bin/sh\necho '[]'\n")
	hooks := &config.RemoteHooks{LaunchCmd: hook, DeleteCmd: hook}

	report, err := Run(withRemote(testOptions(t, false), hooks))
	require.NoError(t, err)

	checks := findCheckRows(report, "coder")
	require.Len(t, checks, 1)
	require.Equal(t, StatusWarn, checks[0].Status)
	require.Contains(t, checks[0].Detail, "timed out",
		"a coder that passes the deadline without answering must be reported as a timeout")
	require.Equal(t, "run `coder login`", checks[0].Remediation)
	require.False(t, checks[0].Problem)
	require.Zero(t, report.UnresolvedCount())
}

// TestRemoteCoderWhoami_SignalDeathNearDeadlineNotReportedAsTimeout verifies
// that a coder process signal-killed before the deadline — while a pipe-holding
// descendant keeps CombinedOutput blocked past it — is reported as "failed"
// with its captured output, not "timed out". The signal death has ExitCode < 0,
// but the context did not kill the process (cmd.Cancel is only invoked while the
// process is still alive), so ctxKilled stays false and the output is preserved.
//
// The fake coder sends itself SIGKILL after 100ms, producing a negative
// ExitCode that would satisfy the old ExitCode < 0 check. A pipe-holding
// descendant (sleep 30 &) keeps CombinedOutput's pipe readers alive past the
// 1s deadline, making ctx.Err() return DeadlineExceeded even though the
// process self-terminated before the context fired — reproducing both
// predicates the old code checked. The ctxKilled flag (set only by cmd.Cancel)
// distinguishes this from a genuine context-killed timeout.
func TestRemoteCoderWhoami_SignalDeathNearDeadlineNotReportedAsTimeout(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	binDir := t.TempDir()
	// The fake coder self-exits with SIGKILL after 100ms. A pipe-holding
	// descendant (sleep 30 &) keeps CombinedOutput's pipe readers alive past
	// the 1s deadline, making ctx.Err() return DeadlineExceeded even though
	// the process self-terminated before the context fired. The 2s wait delay
	// guarantees the deadline fires before WaitDelay closes the pipe.
	//
	// The backgrounded sleep survives the test as an orphan (WaitDelay closes
	// the inherited pipes but does not terminate descendants), so the script
	// records the child PID and t.Cleanup reaps it.
	pidFile := filepath.Join(dir, "sleep.pid")
	writeExecutable(t, binDir, "coder",
		"#!/bin/sh\n"+
			"sleep 0.1\n"+
			"echo 'Error: signal death' >&2\n"+
			"sleep 30 &\n"+
			"echo $! > \""+pidFile+"\"\n"+
			"kill -KILL $$\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return
		}
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGKILL)
		}
	})

	hook := writeHookScript(t, dir, "coder-hook.sh", "#!/bin/sh\necho '[]'\n")
	hooks := &config.RemoteHooks{LaunchCmd: hook, DeleteCmd: hook}

	opts := withRemote(testOptions(t, false), hooks)
	opts.coderProbeTimeout = time.Second
	opts.coderProbeWaitDelay = 2 * time.Second

	report, err := Run(opts)
	require.NoError(t, err)

	checks := findCheckRows(report, "coder")
	require.Len(t, checks, 1)
	require.Equal(t, StatusWarn, checks[0].Status)
	require.NotContains(t, checks[0].Detail, "timed out",
		"coder was signal-killed before the deadline; it did not time out")
	require.Contains(t, checks[0].Detail, "failed")
	require.Contains(t, checks[0].Detail, "Error: signal death")
	require.Equal(t, "run `coder login`", checks[0].Remediation)
	require.False(t, checks[0].Problem, "coder auth warnings must not fail doctor")
	require.Zero(t, report.UnresolvedCount(), "coder auth warnings must not fail doctor")
}

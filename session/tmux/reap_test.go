package tmux

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
)

// shrinkReapWaits lowers the grace periods so reap tests finish in
// milliseconds instead of seconds.
func shrinkReapWaits(t *testing.T) {
	t.Helper()
	oldGrace, oldTerm := reapGraceWait, reapTermWait
	reapGraceWait, reapTermWait = 200*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { reapGraceWait, reapTermWait = oldGrace, oldTerm })
}

// spawnSessionWithEscapee creates a real tmux session (on the test's private
// server) whose pane backgrounds a SIGHUP-immune sleeper — the exact shape of
// the leaked `yes` processes from the 2026-07-03 outage — and returns the
// session name and the escapee's process identity.
func spawnSessionWithEscapee(t *testing.T, name string) proctree.Process {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "escapee.pid")
	// nohup makes the sleeper ignore the SIGHUP that `tmux kill-session`
	// delivers, so without reaping it would outlive the session forever.
	script := fmt.Sprintf("nohup sleep 300 >/dev/null 2>&1 & echo $! > %s; exec sleep 300", pidFile)
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", dir, script).CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)

	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 1
	}, 5*time.Second, 20*time.Millisecond, "escapee pid file never appeared")

	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	escapee, ok := snap[pid]
	require.True(t, ok, "escapee %d not in process snapshot", pid)
	t.Cleanup(func() {
		// Belt-and-suspenders: never leak the sleeper past the test even if
		// the assertion fails.
		_ = proctree.Signal(escapee, syscall.SIGKILL)
	})
	return escapee
}

// TestReapLogsSessionNameLiterally is the #1211 regression: a session name is
// a runtime value that deliberately preserves `%`, so it must be passed as a
// `%s` argument to the reap logger — never spliced into the format string,
// where its `%d`/`%s`/`%n` sequences would be interpreted and corrupt the log
// with `%!s(MISSING)` / `%!d(...)` garbage.
func TestReapLogsSessionNameLiterally(t *testing.T) {
	// Redirect the WARNING logger to a buffer for the duration of this test.
	var buf bytes.Buffer
	oldOut, oldFlags := log.WarningLog.Writer(), log.WarningLog.Flags()
	log.WarningLog.SetOutput(&buf)
	log.WarningLog.SetFlags(0)
	t.Cleanup(func() {
		log.WarningLog.SetOutput(oldOut)
		log.WarningLog.SetFlags(oldFlags)
	})

	// A real, live child that survives the (near-zero) grace period, so the
	// reaper actually SIGTERMs it and emits a log line about it.
	child := exec.Command("sleep", "300")
	require.NoError(t, child.Start())
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})
	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	proc, ok := snap[child.Process.Pid]
	require.True(t, ok, "child %d not in snapshot", child.Process.Pid)

	// A session name packed with format specifiers — the exact hazard #1211
	// describes (tmux sanitization preserves `%`).
	const name = "af_fmt%d%s%n_evil"
	reapLeakedProcesses(name, []proctree.Process{proc}, time.Millisecond, 300*time.Millisecond)

	out := buf.String()
	require.Contains(t, out, "tmux "+name+":",
		"session name must be logged literally, not interpreted as a format string")
	require.NotContains(t, out, "%!",
		"format-string corruption markers (%%!s(MISSING), %%!d(...)) must not appear")
}

// TestCloseReapsEscapedPaneProcesses is the end-to-end #1104 regression
// test: a pane child that ignores SIGHUP must not survive Close().
func TestCloseReapsEscapedPaneProcesses(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_reap-close-test"
	escapee := spawnSessionWithEscapee(t, name)
	require.True(t, proctree.AliveSame(escapee), "escapee must be alive before Close")

	session := NewTmuxSessionFromSanitizedName(name, "sh")
	_, closeErr := session.Close()
	require.NoError(t, closeErr)
	require.False(t, session.ExistsOrUnknown(), "session must be gone after Close")

	// Close reaps asynchronously; the escapee ignores SIGHUP, so only the
	// reaper's SIGTERM/SIGKILL can end it.
	require.Eventually(t, func() bool { return !proctree.AliveSame(escapee) },
		5*time.Second, 25*time.Millisecond,
		"SIGHUP-immune pane child survived Close — process tree was not reaped")
}

// TestCleanupSessionsReapsEscapedProcesses covers the `af reset` sweep: it
// must reap synchronously (the CLI process exits right after).
func TestCleanupSessionsReapsEscapedProcesses(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	escapee := spawnSessionWithEscapee(t, "af_reap-reset-test")

	// Stamp this home's ownership marker: the sweep only kills sessions it
	// can prove it owns (#1122), and the raw `tmux new-session` above does
	// not go through the af creation path that stamps it.
	home, err := afHomeDir()
	require.NoError(t, err)
	out, err := exec.Command("tmux", "set-environment", "-t", "=af_reap-reset-test", EnvMarkerHome, home).CombinedOutput()
	require.NoError(t, err, "set-environment: %s", out)

	require.NoError(t, CleanupSessions(cmd.MakeExecutor()))

	// Synchronous contract: by the time CleanupSessions returns, the sweep
	// has run to completion.
	require.False(t, proctree.AliveSame(escapee),
		"SIGHUP-immune pane child survived CleanupSessions")
}

// TestCaptureRejectsNonTmuxPaneRoots is the safety property that keeps
// mock-backed tests (and any confused tmux output) from ever capturing a
// process tree that is not rooted in a real tmux pane: a claimed pane PID
// whose parent is not a tmux server must be ignored.
func TestCaptureRejectsNonTmuxPaneRoots(t *testing.T) {
	// Our own PID is alive but its parent is the go test runner, not tmux.
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(fmt.Sprintf("%d\n", os.Getpid())), nil
		},
	}
	procs := SessionProcessTrees(cmdExec, "af_bogus")
	require.Empty(t, procs, "a pane root whose parent is not tmux must never be captured")
}

// TestCaptureIgnoresGarbageOutput: mock executors routinely return
// non-numeric canned output; capture must degrade to a no-op.
func TestCaptureIgnoresGarbageOutput(t *testing.T) {
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}
	require.Empty(t, SessionProcessTrees(cmdExec, "af_bogus"))
}

// TestCloseDoesNotReapWhenSessionSurvives: if kill-session fails and the
// session is still alive, its processes are not leaks and must be left
// alone.
func TestCloseDoesNotReapWhenSessionSurvives(t *testing.T) {
	shrinkReapWaits(t)

	// A live child of this test stands in for a pane process. The mock
	// reports it as a pane root — but with a non-tmux parent it would be
	// rejected anyway, so this test asserts at the Close level: kill-session
	// fails, has-session says alive, and the child must remain untouched.
	child := exec.Command("sleep", "300")
	require.NoError(t, child.Start())
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				return fmt.Errorf("server wedged")
			}
			return nil // has-session succeeds -> session still exists
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte(fmt.Sprintf("%d\n", child.Process.Pid)), nil
		},
	}
	session := newTmuxSession("af_survivor", "sh", NewMockPtyFactory(t), cmdExec)
	_, survErr := session.Close()
	require.Error(t, survErr, "a surviving session must surface the kill failure")

	time.Sleep(3 * reapGraceWait)
	require.NoError(t, child.Process.Signal(syscall.Signal(0)),
		"processes of a session that survived kill-session must not be reaped")
}

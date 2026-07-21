package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
)

// #2028: the cleanup kill-session on Start's pty-start-failure path used a bare,
// unbounded t.cmdExec.Run, so a wedged tmux server would hang it forever. Start
// runs on the daemon's create/launch path, so that stall wedges the whole handler
// — the same class as #1917's teardown wedge and #1967's unbounded exec.
//
// Like the other wedge tests in this package (kill_wedge_test.go / start_wedge_
// test.go) this drives a REAL fake tmux on PATH that sleeps in a CHILD, not a
// blocking MockCmdExec: a blocking mock is unreachable by any context deadline and
// would fail identically before and after the fix, proving nothing. Only a real
// subprocess can be SIGKILLed on the deadline, and only the process-group kill +
// tmuxWaitDelay collect the pipe-holding child.

// failingPtyFactory is a PtyFactory whose Start always fails, driving Start
// straight into its pty-start-failure cleanup branch (the #2028 site) without a
// real PTY.
type failingPtyFactory struct{}

func (failingPtyFactory) Start(*exec.Cmd) (*os.File, error) {
	return nil, fmt.Errorf("simulated pty start failure")
}
func (failingPtyFactory) Close() {}

// wedgeKillSessionAfterCreateOnPath installs a fake `tmux` on PATH that:
//   - answers the FIRST has-session "no such session" (exit 1), so Start's up-front
//     existence gate passes and the create proceeds to ptyFactory.Start;
//   - answers every LATER has-session "exists" (exit 0), so the post-failure
//     ExistsOrUnknown reports the session present and the cleanup kill-session runs;
//   - answers list-panes (SessionProcessTrees) cleanly with no panes;
//   - WEDGES kill-session by sleeping in a child, standing in for a wedged server —
//     the command whose unbounded Run this test pins.
func wedgeKillSessionAfterCreateOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "has_session_calls")
	pidFile := filepath.Join(dir, "wedge.pid")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"has-session)\n" +
		"  n=$(cat " + state + " 2>/dev/null || echo 0)\n" +
		"  n=$((n + 1))\n" +
		"  echo \"$n\" > " + state + "\n" +
		"  if [ \"$n\" -eq 1 ]; then exit 1; fi\n" +
		"  exit 0\n" +
		"  ;;\n" +
		"list-panes)\n" +
		"  exit 0\n" +
		"  ;;\n" +
		"kill-session)\n" +
		"  sleep 300 &\n" +
		"  echo $! > " + pidFile + "\n" +
		"  wait\n" +
		"  ;;\n" +
		"*)\n" +
		"  exit 0\n" +
		"  ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Backstop: if a regression leaves the cleanup blocked (the test then times
	// out), kill the wedged child so nothing is orphaned past the test.
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			_ = exec.Command("kill", "-9", strconv.Itoa(pid)).Run()
		}
	})
}

// TestStartPtyFailureCleanupKillSessionIsBounded is the #2028 regression: when
// ptyFactory.Start fails and the server is wedged, Start's cleanup kill-session
// must return on its own deadline rather than hang. Before the fix the cleanup ran
// on a bare, unbounded exec.Command and Start never returned.
func TestStartPtyFailureCleanupKillSessionIsBounded(t *testing.T) {
	wedgeKillSessionAfterCreateOnPath(t)
	shortTmuxTimeout(t, 200*time.Millisecond)
	// Pin the `-e` support probe so sessionEnvFlags never runs an unbounded
	// `tmux -V` against the wedging fake (envmarker.go) — not what this exercises.
	forceNewSessionEnvMarkers(t, false)
	ts := NewTmuxSessionWithDeps("wedge-2028-cleanup", "sh", failingPtyFactory{}, cmd.MakeExecutor())

	done := make(chan error, 1)
	go func() { done <- ts.Start(t.TempDir()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start must fail when ptyFactory.Start fails")
		}
		// The pty-start failure must survive; the bounded cleanup only appends its
		// own (timeout) cause to it — it does not replace or swallow the real error.
		if !strings.Contains(err.Error(), "error starting tmux session") {
			t.Fatalf("want the pty-start failure surfaced, got %v", err)
		}
		if !errors.Is(err, ErrSessionNotStarted) {
			t.Fatalf("a PtyFactory.Start failure happens before the new-session process begins; got %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Start hung on the pty-start-failure cleanup kill-session against a wedged tmux " +
			"server (#2028): the cleanup kill-session was an unbounded t.cmdExec.Run, and Start runs " +
			"on the daemon's create/launch path, so the whole handler wedges")
	}
}

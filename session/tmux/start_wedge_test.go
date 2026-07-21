package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
)

// #1962 regressions for the two known DoesSessionExist misreads in Start. Both
// reuse the wedged-server harness from bounded_test.go (stallingTmuxOnPath /
// shortTmuxTimeout): a fake `tmux` on PATH that sleeps in a child, so a real
// deadline can SIGKILL it — the only faithful reproduction of a wedged server
// (a blocking mock is unreachable by any context deadline; see kill_wedge_test.go).

// TestStartOnWedgedServerReportsTimeoutNotAlreadyExists covers start.go's up-front
// existence check. It is a POSITIVE existence gate, so a wedged/timed-out
// has-session must NOT be laundered into "tmux session already exists": that
// misreads UNKNOWN as a confirmed name collision (#1962). With stallingTmuxOnPath
// every tmux command wedges, so the line-16 probe is the one under test — Start
// must surface the timeout and return fast.
func TestStartOnWedgedServerReportsTimeoutNotAlreadyExists(t *testing.T) {
	stallingTmuxOnPath(t)
	shortTmuxTimeout(t, 200*time.Millisecond)
	ts := NewTmuxSessionWithDeps("wedge-1962-start", "sh", NewMockPtyFactory(t), cmd.MakeExecutor())

	done := make(chan error, 1)
	go func() { done <- ts.Start(t.TempDir()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start against a wedged tmux server must fail, got nil")
		}
		// The wedged server never answered has-session, so the name is NOT known
		// to be taken. Claiming "already exists" is the exact misread #1962 fixes.
		if strings.Contains(err.Error(), "already exists") {
			t.Fatalf("wedged has-session laundered into a false name collision (#1962): %v", err)
		}
		if !errors.Is(err, ErrTmuxTimeout) {
			t.Fatalf("want ErrTmuxTimeout from Start's existence check against a wedged server, got %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Start hung against a stalled tmux (#1962): the line-16 existence check must be bounded")
	}
}

// TestStartReadinessPollDoesNotExitOnWedgedServer covers start.go's readiness
// poll. Reading the lossy bool let a mid-poll wedge exit the loop as if the
// session had come up, so Start reported SUCCESS for a session tmux never
// confirmed (#1962). The poll must instead keep waiting on a !known probe until
// its 2s deadline, then take the existing timeout path (ErrTmuxTimeout).
//
// stallingTmuxOnPath alone can't reproduce this — it wedges the line-16 check
// too, so Start never reaches the poll. wedgeHasSessionAfterFirstOnPath answers
// the first has-session ("not found", so line 16 passes and the create proceeds)
// and wedges every command after it, standing in for a server that goes wedged
// DURING the poll.
func TestStartReadinessPollDoesNotExitOnWedgedServer(t *testing.T) {
	wedgeHasSessionAfterFirstOnPath(t)
	shortTmuxTimeout(t, 200*time.Millisecond)
	// Pin the `-e` support probe so Start's sessionEnvFlags never runs an
	// unbounded `tmux -V` against the wedging fake (envmarker.go:78) — that probe
	// is not what this test exercises, and it has no deadline of its own.
	forceNewSessionEnvMarkers(t, false)
	ts := NewTmuxSessionWithDeps("wedge-1962-poll", "sh", NewMockPtyFactory(t), cmd.MakeExecutor())

	done := make(chan error, 1)
	go func() { done <- ts.Start(t.TempDir()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start reported success against a server that wedged during the readiness " +
				"poll — the poll exited on the lie that the session came up (#1962)")
		}
		// The poll could not confirm the session, so the failure must be the
		// timeout path (which threads pane-state / ErrTmuxTimeout), never a false
		// success and never a confirmed-gone classification.
		if !errors.Is(err, ErrTmuxTimeout) {
			t.Fatalf("want ErrTmuxTimeout when the readiness poll cannot confirm the session, got %v", err)
		}
		if errors.Is(err, ErrSessionGone) {
			t.Fatalf("a wedged server proves nothing about the session; must not report ErrSessionGone: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Start hung against a stalled tmux (#1962)")
	}
}

// wedgeHasSessionAfterFirstOnPath installs a fake `tmux` on PATH that answers the
// FIRST has-session with "no such session" (exit 1) — so Start's existence check
// passes and the create proceeds — then WEDGES every subsequent tmux command
// (has-session, list-panes, kill-session, display-message) by sleeping in a child.
// set-option answers fast so the non-wedged control path can still complete. It
// reproduces a server that goes wedged DURING the readiness poll, which
// stallingTmuxOnPath cannot (that fake wedges the line-16 check too).
func wedgeHasSessionAfterFirstOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "has_session_calls")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"has-session)\n" +
		"  n=$(cat " + state + " 2>/dev/null || echo 0)\n" +
		"  n=$((n + 1))\n" +
		"  echo \"$n\" > " + state + "\n" +
		"  if [ \"$n\" -eq 1 ]; then exit 1; fi\n" +
		"  sleep 300 & wait\n" +
		"  ;;\n" +
		"show-options)\n" +
		"  echo 'no server running' >&2\n" +
		"  exit 1\n" +
		"  ;;\n" +
		"set-option|new-session)\n" +
		"  exit 0\n" +
		"  ;;\n" +
		"*)\n" +
		"  sleep 300 & wait\n" +
		"  ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

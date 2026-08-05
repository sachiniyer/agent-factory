package tmux

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests pin the contract CloseAndWaitForPaneExit exists to provide, which
// is narrower than Close's: not "tmux answered", but "the worktree may now be
// deleted or moved". A session that SURVIVED its kill can never satisfy that,
// however much else was established — it may still be writing (#2962).
//
// The distinction matters because of how the destructive caller gates. From
// session/teardown.go's closeTabForDestructiveTeardown:
//
//	state, err := ts.CloseAndWaitForPaneExit()
//	if state != tmux.PaneStateKnown { ...refuse... }
//	if err != nil { log.WarningLog.Printf(...) }   // logs and PROCEEDS
//	return stateKnown, nil
//
// The STATE is the gate; the error is only logged. So returning
// PaneStateKnown alongside "error killing tmux session" is not a mixed signal
// the caller can weigh — it is a licence to delete.

// scriptedTmuxOnPath installs a fake tmux whose behaviour per subcommand is
// given by the caller, so a test can stage the exact combination of answers that
// a real wedged/permission-refusing server would produce.
func scriptedTmuxOnPath(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n" + body + "\n*) exit 97 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestCloseAndWaitSurvivedKillIsNeverKnown is the #2962 regression.
//
// Staged exactly as the report describes: the pane PID query fails for a
// NON-timeout reason (so the existing ErrTmuxTimeout guard does not fire),
// list-panes succeeds (so the capture-error guard does not fire), kill-session
// fails, and has-session confirms the session is STILL THERE.
//
// On master this returns PaneStateKnown, and the destructive caller deletes the
// worktree of a live session.
func TestCloseAndWaitSurvivedKillIsNeverKnown(t *testing.T) {
	scriptedTmuxOnPath(t, `
display-message) echo "can't find pane" >&2; exit 1 ;;
list-panes)      exit 0 ;;
kill-session)    echo "cannot kill session" >&2; exit 1 ;;
has-session)     exit 0 ;;`)

	ts := NewTmuxSessionFromSanitizedName("af_survivor", "")
	state, err := ts.CloseAndWaitForPaneExit()

	if state == PaneStateKnown {
		t.Fatalf("CloseAndWaitForPaneExit = PaneStateKnown with err=%v, but kill-session FAILED and "+
			"has-session confirmed the session is still alive. The destructive caller gates on the "+
			"state and only logs the error, so this deletes the worktree of a live session (#2962)", err)
	}
	if err == nil {
		t.Error("a surviving session must also report why, or the caller has nothing to act on")
	}
}

// TestCloseAndWaitSurvivedKillIsNeverKnownWithAQueryablePane covers the SAME
// defect on the other branch, which the report does not name: when panePID
// SUCCEEDS, the function falls through to `return state, closeErr` at the end
// and inherits Close's KNOWN just the same.
//
// Reaching it needs the pane leader to be already gone (so the pane-exit wait is
// skipped) while the session survives — an agent that exited under a session
// tmux keeps alive, e.g. remain-on-exit or a second pane. The surviving session's
// OTHER panes may still be writing.
func TestCloseAndWaitSurvivedKillIsNeverKnownWithAQueryablePane(t *testing.T) {
	deadPID := exitedPID(t)
	scriptedTmuxOnPath(t, `
display-message) echo "`+deadPID+`" ;;
list-panes)      exit 0 ;;
kill-session)    echo "cannot kill session" >&2; exit 1 ;;
has-session)     exit 0 ;;`)

	ts := NewTmuxSessionFromSanitizedName("af_survivor_two", "")
	state, err := ts.CloseAndWaitForPaneExit()

	if state == PaneStateKnown {
		t.Fatalf("CloseAndWaitForPaneExit = PaneStateKnown with err=%v: the pane leader had already "+
			"exited, so the pane-exit wait was skipped, and the final return handed back Close's "+
			"KNOWN for a session that survived its kill (#2962)", err)
	}
	if err == nil {
		t.Error("a surviving session must also report why")
	}
}

// TestCloseAndWaitUnqueryablePaneStillChecksTheProcessSet is the third hole on
// this path: when panePID fails, the old code returned EARLY, skipping the
// capture-error and surviving-process gates entirely. A read that failed
// (list-panes) was therefore never consulted on that branch — the same class one
// level down.
func TestCloseAndWaitUnqueryablePaneStillChecksTheProcessSet(t *testing.T) {
	scriptedTmuxOnPath(t, `
display-message) echo "can't find pane" >&2; exit 1 ;;
list-panes)      echo "cannot list panes" >&2; exit 1 ;;
kill-session)    exit 0 ;;
has-session)     exit 1 ;;`)

	ts := NewTmuxSessionFromSanitizedName("af_unqueryable", "")
	state, err := ts.CloseAndWaitForPaneExit()

	if state == PaneStateKnown {
		t.Fatalf("CloseAndWaitForPaneExit = PaneStateKnown with err=%v: the pane PID was unqueryable "+
			"AND the pane process set could not be listed, so nothing establishes that anything "+
			"stopped writing — yet the early return skipped the capture-error gate (#2962)", err)
	}
	if err == nil || !strings.Contains(err.Error(), "cannot list panes") {
		t.Errorf("error = %v, want it to name the read that failed", err)
	}
}

// TestCloseAndWaitCleanTeardownStaysKnown is the other half of the contract, so
// the guards above are a signal rather than a state that is always Unknown: an
// unqueryable pane whose session IS confirmed gone and whose process set was
// read cleanly still authorizes cleanup. Without this, the fix would strand
// every caller whose agent had already exited.
func TestCloseAndWaitCleanTeardownStaysKnown(t *testing.T) {
	scriptedTmuxOnPath(t, `
display-message) echo "can't find pane" >&2; exit 1 ;;
list-panes)      exit 0 ;;
kill-session)    exit 0 ;;
has-session)     exit 1 ;;`)

	ts := NewTmuxSessionFromSanitizedName("af_clean", "")
	state, err := ts.CloseAndWaitForPaneExit()

	if state != PaneStateKnown {
		t.Fatalf("CloseAndWaitForPaneExit = %v (err=%v), want PaneStateKnown: kill-session succeeded, "+
			"the session is confirmed gone, and the pane process set was read with nothing left — "+
			"refusing here would block every ordinary teardown of an already-exited agent", state, err)
	}
	if err != nil {
		t.Errorf("error = %v, want nil for a clean teardown", err)
	}
}

// exitedPID returns a PID that is definitively gone: a real child, run to
// completion and reaped, whose number the kernel has not yet reused.
//
// Not a large constant. That would be a guess about the host's pid space, and a
// guess that lands on a LIVE process inverts the test it is used in — the pane
// would look alive and the assertion would pass for the wrong reason.
func exitedPID(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn a throwaway child: %v", err)
	}
	pid := cmd.Process.Pid
	// Reaped by Run, so the number is free. Confirm the kernel agrees before a
	// test leans on it.
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Skipf("pid %d was reused between exit and check (kill(0) = %v); this fixture needs a "+
			"definitively-gone pid", pid, err)
	}
	return strconv.Itoa(pid)
}

// TestSessionGoneRaceAfterAPaneWasObservedStaysUnknown is the Codex finding on
// #2966: the determinate-empty is only sound when NO pane was ever observed.
//
// Here panePID names a live pane, and list-panes THEN reports the session gone.
// That is a race, not an empty session: the ancestry list-panes would have
// returned is lost, so descendants and SID members that outlive the leader are
// unaccounted for. Leader death cannot prove they stopped writing (#1104/#802).
func TestSessionGoneRaceAfterAPaneWasObservedStaysUnknown(t *testing.T) {
	// The pane leader must be ALREADY GONE, so the pane-exit wait is skipped and
	// this test isolates the capture gate. With a live pid it passed either way —
	// the wait caught it — which made it assert the claim while checking something
	// weaker.
	deadPID := exitedPID(t)
	scriptedTmuxOnPath(t, `
display-message) echo "`+deadPID+`" ;;
list-panes)      echo "can't find session: af_raced" >&2; exit 1 ;;
kill-session)    exit 0 ;;
has-session)     exit 1 ;;`)

	ts := NewTmuxSessionFromSanitizedName("af_raced", "")
	state, err := ts.CloseAndWaitForPaneExit()

	if state == PaneStateKnown {
		t.Fatalf("CloseAndWaitForPaneExit = PaneStateKnown with err=%v: a pane WAS observed and the "+
			"session then vanished before list-panes, so its descendants are unaccounted for — that is "+
			"a lost ancestry, not an empty session", err)
	}
	if err == nil {
		t.Error("the race must be reported, not silently downgraded")
	}
}

// TestPaneQueryTimeoutKeepsTheSentinelThroughACloseFailure is the other Codex
// finding: when the pane query times out AND close also fails, the combined
// return must still carry ErrTmuxTimeout. #1917 keeps that sentinel reachable
// through errors.Is for callers that classify on it, and returning closeErr
// alone erased it.
func TestPaneQueryTimeoutKeepsTheSentinelThroughACloseFailure(t *testing.T) {
	shortTmuxTimeout(t, 200*time.Millisecond)
	scriptedTmuxOnPath(t, `
display-message) sleep 300 & wait ;;
list-panes)      exit 0 ;;
kill-session)    echo "cannot kill session" >&2; exit 1 ;;
has-session)     exit 0 ;;`)

	ts := NewTmuxSessionFromSanitizedName("af_timeout_and_survivor", "")
	state, err := ts.CloseAndWaitForPaneExit()

	if state != PaneStateUnknown {
		t.Fatalf("state = %v, want PaneStateUnknown", state)
	}
	if !errors.Is(err, ErrTmuxTimeout) {
		t.Errorf("error = %v, want the ErrTmuxTimeout sentinel to survive the combined failure so "+
			"callers can still classify the tmux command as timed out (#1917)", err)
	}
}

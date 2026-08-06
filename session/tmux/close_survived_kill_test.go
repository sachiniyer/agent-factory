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

	"github.com/sachiniyer/agent-factory/internal/proctree"
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

// TestCaptureTimeoutKeepsTheSentinelThroughACloseFailure is the second sentinel
// leak (Codex on #2966, round 2). The first fix joined pidErr into the
// survived-kill branch; list-panes can time out too, and its ErrTmuxTimeout
// lives in processes.captureErr, which the same branch was still dropping.
//
// Here the pane query SUCCEEDS (so pidErr is nil and cannot carry the sentinel),
// list-panes times out, and the kill then fails with the session still alive.
func TestCaptureTimeoutKeepsTheSentinelThroughACloseFailure(t *testing.T) {
	shortTmuxTimeout(t, 200*time.Millisecond)
	livePID := strconv.Itoa(os.Getpid())
	scriptedTmuxOnPath(t, `
display-message) echo "`+livePID+`" ;;
list-panes)      sleep 300 & wait ;;
kill-session)    echo "cannot kill session" >&2; exit 1 ;;
has-session)     exit 0 ;;`)

	ts := NewTmuxSessionFromSanitizedName("af_capture_timeout", "")
	state, err := ts.CloseAndWaitForPaneExit()

	if state != PaneStateUnknown {
		t.Fatalf("state = %v, want PaneStateUnknown", state)
	}
	if !errors.Is(err, ErrTmuxTimeout) {
		t.Errorf("error = %v, want the ErrTmuxTimeout from the timed-out list-panes to survive the "+
			"combined failure — pidErr is nil here, so the capture error is the only carrier (#1917)", err)
	}
}

// TestUnparseablePaneOutputIsNotAbsence is the third-round Codex finding: the
// determinate-empty predicate accepted ANY pane-query failure, including one
// from a session that plainly existed.
//
// Here display-message returns garbage (tmux answered, we could not read it) and
// the session then vanishes before list-panes. That pairing is a LOST ANCESTRY —
// detached descendants and SID members were never captured — not an empty
// session, so cleanup must refuse.
//
// The sibling below pins the case that MUST still pass, so the fix is a
// discrimination rather than a blanket refusal.
func TestUnparseablePaneOutputIsNotAbsence(t *testing.T) {
	scriptedTmuxOnPath(t, `
display-message) echo "not-a-pid" ;;
list-panes)      echo "can't find session: af_garbled" >&2; exit 1 ;;
kill-session)    exit 0 ;;
has-session)     exit 1 ;;`)

	ts := NewTmuxSessionFromSanitizedName("af_garbled", "")
	state, err := ts.CloseAndWaitForPaneExit()

	if state == PaneStateKnown {
		t.Fatalf("CloseAndWaitForPaneExit = PaneStateKnown with err=%v: display-message ANSWERED with "+
			"unreadable output, which is not evidence the session was absent — pairing it with a "+
			"vanished session treats a lost ancestry as an empty one (#2962 round 3)", err)
	}
	if err == nil {
		t.Error("the refusal must say why")
	}
}

// TestEmptyPaneOutputIsAbsence is the other half: tmux answering with NO pane is
// what a missing session actually produces (measured: exit 0, empty output), and
// that paired with a vanished session is a real empty set. Refusing here would
// block every ordinary teardown of an already-exited agent.
func TestEmptyPaneOutputIsAbsence(t *testing.T) {
	scriptedTmuxOnPath(t, `
display-message) exit 0 ;;
list-panes)      echo "can't find session: af_absent" >&2; exit 1 ;;
kill-session)    exit 0 ;;
has-session)     exit 1 ;;`)

	ts := NewTmuxSessionFromSanitizedName("af_absent", "")
	state, err := ts.CloseAndWaitForPaneExit()

	if state != PaneStateKnown {
		t.Fatalf("CloseAndWaitForPaneExit = %v (err=%v), want PaneStateKnown: tmux answered that there "+
			"is no pane AND no session, which is a determinate empty", state, err)
	}
	if err != nil {
		t.Errorf("error = %v, want nil", err)
	}
}

// The vanished-session sweep, round 5 (Codex on #2966). Round 4 answered "did
// anything outlive this session?" with a marker scan, but matched on AF_SESSION
// ALONE and only REPORTED what it found. Both were wrong, in opposite
// directions, and these three cases pin the corrected split.

// TestVanishedSessionReapsAnOwnedSurvivor: a descendant carrying THIS home's
// markers is a real escaped process. Reporting it is not enough — the tombstone
// is retried by finishUserKill, and a scan that never signals leaves the leak and
// the stuck worktree forever. It must be reaped, and cleanup may then proceed.
func TestVanishedSessionReapsAnOwnedSurvivor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	const name = "af_vanished_owned"
	survivor := spawnMarkedProcess(t, name, home)

	scriptedTmuxOnPath(t, `
display-message) exit 0 ;;
list-panes)      echo "can't find session: `+name+`" >&2; exit 1 ;;
kill-session)    exit 0 ;;
has-session)     exit 1 ;;`)

	state, err := NewTmuxSessionFromSanitizedName(name, "").CloseAndWaitForPaneExit()

	if state != PaneStateKnown || err != nil {
		t.Fatalf("state=%v err=%v: the survivor was this home's, so the bounded reap should have "+
			"eliminated it and authorized cleanup — refusing instead leaves the tombstone retrying "+
			"forever without ever signalling the process", state, err)
	}
	if proctree.AliveSame(survivor) {
		t.Errorf("pid %d survived the sweep; reporting a leak without reaping it is what round 4 got wrong", survivor.PID)
	}
}

// TestVanishedSessionIgnoresAnotherHomesProcess: the same sanitized name can
// exist under a DIFFERENT agent-factory home — a leftover from a temp or dev
// install. Matching on AF_SESSION alone counted it as ours, refusing cleanup
// forever over a process we must not touch.
func TestVanishedSessionIgnoresAnotherHomesProcess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	const name = "af_vanished_foreign"
	foreign := spawnMarkedProcess(t, name, filepath.Join(t.TempDir(), "some-other-home"))

	scriptedTmuxOnPath(t, `
display-message) exit 0 ;;
list-panes)      echo "can't find session: `+name+`" >&2; exit 1 ;;
kill-session)    exit 0 ;;
has-session)     exit 1 ;;`)

	state, err := NewTmuxSessionFromSanitizedName(name, "").CloseAndWaitForPaneExit()

	if state != PaneStateKnown || err != nil {
		t.Fatalf("state=%v err=%v: the marked process belongs to ANOTHER af home, so it is neither our "+
			"survivor nor ours to reap — blocking on it strands this teardown indefinitely", state, err)
	}
	if !proctree.AliveSame(foreign) {
		t.Errorf("another home's process (pid %d) was killed by this teardown", foreign.PID)
	}
}

// TestVanishedSessionRefusesAnUnattributableProcess: a process carrying this
// session's name but NO home marker cannot be shown to be ours OR foreign. That
// is the one case that must still block — "I could not tell" is not "not mine".
func TestVanishedSessionRefusesAnUnattributableProcess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	const name = "af_vanished_unattributable"
	spawnMarkedProcess(t, name, "")

	scriptedTmuxOnPath(t, `
display-message) exit 0 ;;
list-panes)      echo "can't find session: `+name+`" >&2; exit 1 ;;
kill-session)    exit 0 ;;
has-session)     exit 1 ;;`)

	state, err := NewTmuxSessionFromSanitizedName(name, "").CloseAndWaitForPaneExit()

	if state == PaneStateKnown {
		t.Fatalf("state=Known err=%v: a process marking this session with no %s marker cannot be "+
			"attributed, and an unattributable process is not an absent one", err, EnvMarkerHome)
	}
}

// spawnMarkedProcess starts a child carrying AF_SESSION=<name> and returns its
// pid, cleaned up by the test.
func spawnMarkedProcess(t *testing.T, sessionName, afHome string) proctree.Process {
	t.Helper()
	c := exec.Command("sleep", "300")
	// A CLEAN env, not os.Environ(). processEnvValue returns the FIRST match, and
	// this test binary runs inside a real af session — so inheriting the ambient
	// environment puts THAT session's AF_SESSION ahead of the one appended here,
	// and the child reads as belonging to the developer's session instead of the
	// fixture's. Measured: the sweep then reported live pids from the running
	// session rather than the spawned child.
	c.Env = []string{"PATH=" + os.Getenv("PATH"), EnvMarkerSession + "=" + sessionName}
	if afHome != "" {
		c.Env = append(c.Env, EnvMarkerHome+"="+afHome)
	}
	// Its OWN kernel session, because the sweep expands a matched process to its
	// SID members — which is scoped in production precisely because tmux makes a
	// pane root a session leader (see captureSessionProcessTrees). A child that
	// merely inherits the test runner's SID is not that shape: measured, the
	// expansion then pulled in the developer's live af session processes and the
	// fixture indicted them instead of its own child.
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		t.Fatalf("spawn marked process: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
	})
	// The scan reads /proc/<pid>/environ; wait until it is readable, then take the
	// full process IDENTITY. A bare pid is not enough for later liveness checks:
	// AliveSame compares (PID, StartID), so a fabricated Process{PID: n} reports
	// "not alive" for a perfectly live process — which made an earlier version of
	// these assertions pass vacuously.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if env, err := proctree.Environ(c.Process.Pid); err == nil {
			if v, ok := processEnvValue(env, EnvMarkerSession); ok && v == sessionName {
				p, lookupErr := proctree.Lookup(c.Process.Pid)
				if lookupErr != nil {
					t.Fatalf("look up spawned pid %d: %v", c.Process.Pid, lookupErr)
				}
				return p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Skipf("child %d environ never became readable; this fixture needs a marker-visible process", c.Process.Pid)
	return proctree.Process{}
}

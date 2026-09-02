package tmux

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	script := "nohup sleep 300 >/dev/null 2>&1 & " + recordPIDShell("$!", pidFile) + "; exec sleep 300"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", dir, script).CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	testguard.KeepTmuxServerOnEmpty(t)

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
	//
	// reapEscaped, because that reason keeps a plain SIGTERM on WARNING (#2765)
	// and this test is about the WARNING logger's format-string safety. The
	// on-request reason routes the same line to INFO and is covered separately in
	// reap_severity_test.go — including its own #1211 assertion, so neither
	// severity can drift into splicing the name into the format string.
	const name = "af_fmt%d%s%n_evil"
	reapSessionProcesses(reapEscaped, name, []proctree.Process{proc}, time.Millisecond, 300*time.Millisecond)

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

// TestCleanupSessionsReapsMarkedProcessWhenSessionVanishesDuringOwnership
// covers the ls-to-marker race's process half. A detached helper can outlive
// the tmux session that stamped its AF_SESSION/AF_HOME ancestry markers; reset
// must not read "session absent" as "no process remains" and delete the
// helper's worktree underneath it.
func TestCleanupSessionsReapsMarkedProcessWhenSessionVanishesDuringOwnership(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_vanished_with_helper"
	home := t.TempDir()
	marked := spawnMarkedSessionWithEscapee(t, name, home)
	t.Setenv("AGENT_FACTORY_HOME", home)

	require.NoError(t, CleanupSessions(vanishOnMarkerExecutor(t, name)))
	require.False(t, proctree.AliveSame(marked),
		"AF_SESSION/AF_HOME-marked helper survived after its tmux session vanished")
}

// TestCleanupSessionsReapsMarkedProcessWhenSessionVanishesDuringCapture
// covers the marker-to-capture race. Ownership was proved while the session
// existed, but losing the session before the destructive capture must not turn
// the already captured process evidence into an empty set.
func TestCleanupSessionsReapsMarkedProcessWhenSessionVanishesDuringCapture(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_vanished_during_capture"
	home := t.TempDir()
	marked := spawnMarkedSessionWithEscapee(t, name, home)
	t.Setenv("AGENT_FACTORY_HOME", home)

	require.NoError(t, CleanupSessions(vanishOnSecondPaneCaptureExecutor(t, name)))
	require.False(t, proctree.AliveSame(marked),
		"AF_SESSION/AF_HOME-marked helper survived after tmux vanished during process capture")
}

// A generic post-marker capture failure is not an empty process set. Reset must
// refuse before kill-session, preserving the live session as the retry fence;
// otherwise the next reset would see no session and launder the failed read.
func TestCleanupSessionsGenericCaptureFailureRemainsRefusedAcrossRetry(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_failed_second_capture"
	home := t.TempDir()
	marked := spawnMarkedSessionWithEscapee(t, name, home)
	t.Setenv("AGENT_FACTORY_HOME", home)

	cmdExec := failPostMarkerPaneCaptureExecutor(t)
	err := CleanupSessions(cmdExec)
	require.ErrorContains(t, err, "injected generic list-panes failure",
		"a failed capture must stop cleanup before it destroys the retry fence")
	exists, known, probeErr := probeSessionStrict(cmd.MakeExecutor(), name)
	require.NoError(t, probeErr)
	require.True(t, known && exists, "generic capture failure must leave the session alive")
	require.True(t, proctree.AliveSame(marked),
		"a process still owned by the retained live session must not be reaped")

	err = CleanupSessions(cmdExec)
	require.ErrorContains(t, err, "injected generic list-panes failure",
		"retry must not launder the prior incomplete capture into an empty session set")
	exists, known, probeErr = probeSessionStrict(cmd.MakeExecutor(), name)
	require.NoError(t, probeErr)
	require.True(t, known && exists, "retry must retain the session until capture succeeds")
}

// A generic capture refusal for one live session must not strand another
// session that already vanished during the same capture batch. The latter has
// no live tmux fence to preserve, so consume its saved marker evidence before
// returning the batch refusal.
func TestCleanupSessionsRecoversVanishedSessionBeforeGenericCaptureRefusal(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const vanishedName = "af_mixed_vanished"
	const failedName = "af_mixed_failed"
	home := t.TempDir()
	vanished := spawnMarkedSessionWithEscapee(t, vanishedName, home)
	retained := spawnMarkedSessionWithEscapee(t, failedName, home)
	t.Setenv("AGENT_FACTORY_HOME", home)

	err := CleanupSessions(mixedCaptureFailureExecutor(t, vanishedName, failedName))
	require.ErrorContains(t, err, "injected generic list-panes failure")
	require.False(t, proctree.AliveSame(vanished),
		"marked helper from the already-vanished session was stranded by another session's refusal")
	exists, known, probeErr := probeSessionStrict(cmd.MakeExecutor(), failedName)
	require.NoError(t, probeErr)
	require.True(t, known && exists, "the generically unreadable session must remain live")
	require.True(t, proctree.AliveSame(retained),
		"the retained live session must keep ownership of its process")
}

// list-panes can answer with a pane PID and the pane can disappear before the
// process snapshot. That is reported as a generic capture error, so cleanup
// must probe the session rather than assuming the generic class is still live.
func TestCleanupSessionsRecoversGenericCaptureFailureAfterConfirmedAbsence(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_generic_then_absent"
	home := t.TempDir()
	marked := spawnMarkedSessionWithEscapee(t, name, home)
	t.Setenv("AGENT_FACTORY_HOME", home)

	require.NoError(t, CleanupSessions(vanishAfterPaneListExecutor(t, name)))
	require.False(t, proctree.AliveSame(marked),
		"marked helper survived after the pane vanished between list-panes and the process snapshot")
}

// A later incomplete capture must not strand an earlier complete capture. The
// earlier session can disappear while the refusal is being established, and a
// retry cannot rediscover its process tree once its tmux fence is gone.
func TestCleanupSessionsReapsCompleteCaptureBeforeLaterRefusal(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const completeName = "af_000_complete_capture"
	const failedName = "af_999_failed_capture"
	home := t.TempDir()
	complete := spawnMarkedSessionWithEscapee(t, completeName, home)
	failed := spawnMarkedSessionWithEscapee(t, failedName, home)
	t.Setenv("AGENT_FACTORY_HOME", home)

	err := CleanupSessions(vanishCompleteBeforeLaterFailureExecutor(t, completeName, failedName))
	require.ErrorContains(t, err, "injected later capture failure")
	require.False(t, proctree.AliveSame(complete),
		"complete capture was stranded after its tmux fence disappeared")
	require.True(t, proctree.AliveSame(failed),
		"the incompletely captured session must retain ownership of its process")
}

// A generic capture can return ancestry before a strict probe proves the
// session absent, but the pathname-like tmux name does not bind that tree to
// the earlier ownership read. An unmarked partial candidate must therefore
// refuse rather than being signalled or treated as absent.
func TestCleanupSessionsRefusesUnmarkedPartialCaptureAfterConfirmedAbsence(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("exact detached-session fixture requires setsid")
	}

	const name = "af_partial_then_absent"
	home := t.TempDir()
	trigger, pidFile := spawnSessionWaitingToStartUnmarkedHelper(t, name, home)
	t.Setenv("AGENT_FACTORY_HOME", home)
	var helper proctree.Process

	err := CleanupSessions(partialCaptureThenAbsentExecutor(t, name, trigger, pidFile, &helper))
	require.ErrorContains(t, err, "has no AF_SESSION marker")
	require.NotZero(t, helper.PID, "partial-capture helper identity was not recorded")
	require.True(t, proctree.AliveSame(helper),
		"an unmarked partial candidate must not be signalled")
}

// The same fixture with its race REMOVED (#3469). Every Refuses* fixture above
// kills the only session on its private tmux server, so the has-session that
// follows races the server's own shutdown. Land after it and tmux answers `no
// server running on <socket>`, which classifies as absence; land before it and
// tmux answers `no current target` — exit 1, naming neither session nor socket —
// which did not, so the partial-capture refusal surfaced instead of the marker
// refusal these assertions are about. Measured on this box at load ~60: 2
// failures in 40 runs of the sibling above, and CI hit two DIFFERENT siblings
// with the identical signature (the 2026-08-25 preview preflight, and PR #3495).
//
// `exit-empty off` removes the race by removing the shutdown: the server stays
// up holding nothing, so the answer that used to be the unlucky one is now the
// only one. The assertions are deliberately identical to the sibling's — the
// property is that a given fixture state produces the SAME refusal reason
// whichever way the server's exit falls.
func TestCleanupSessionsRefusesUnmarkedPartialCaptureOnSessionlessServer(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("exact detached-session fixture requires setsid")
	}

	const name = "af_partial_then_sessionless"
	home := t.TempDir()
	trigger, pidFile := spawnSessionWaitingToStartUnmarkedHelper(t, name, home)
	// Server option, so it outlives the session the executor is about to kill.
	out, setErr := exec.Command("tmux", "set", "-s", "exit-empty", "off").CombinedOutput()
	require.NoError(t, setErr, "tmux set -s exit-empty off: %s", out)
	t.Setenv("AGENT_FACTORY_HOME", home)
	var helper proctree.Process

	err := CleanupSessions(partialCaptureThenAbsentExecutor(t, name, trigger, pidFile, &helper))
	require.ErrorContains(t, err, "has no AF_SESSION marker")
	require.NotZero(t, helper.PID, "partial-capture helper identity was not recorded")
	require.True(t, proctree.AliveSame(helper),
		"an unmarked partial candidate must not be signalled")
}

// The owned session can exit after its AF_HOME marker is read and another home
// can reuse the same tmux name before capture. Even if that replacement also
// vanishes, its partial tree must not inherit the old session's authorization.
func TestCleanupSessionsDoesNotReapForeignReplacementPartialCapture(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_reused_partial_capture"
	home := t.TempDir()
	foreignHome := t.TempDir()
	original := spawnMarkedSessionWithEscapee(t, name, home)
	t.Setenv("AGENT_FACTORY_HOME", home)
	var foreign proctree.Process

	require.NoError(t, CleanupSessions(replaceWithForeignPartialThenAbsentExecutor(t, name, foreignHome, &foreign)))
	require.False(t, proctree.AliveSame(original), "owned original helper was not recovered")
	require.NotZero(t, foreign.PID, "foreign replacement identity was not recorded")
	require.True(t, proctree.AliveSame(foreign),
		"a reused tmux name authorized signaling another home's process")
}

// A verified marked process can fork an unmarked detached child during its
// grace period. Recovery must refresh the verified ancestry while it is still
// connected; once the parent exits, neither markers nor SID membership can
// rediscover that child and a successful empty result would be false.
func TestCleanupSessionsRefusesChildForkedDuringPartialRecoveryGrace(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("exact detached-session fixture requires setsid")
	}

	const name = "af_partial_forks_during_grace"
	home := t.TempDir()
	trigger, parentPIDFile, childPIDFile := spawnSessionWaitingToForkUnmarkedHelper(t, name, home)
	t.Setenv("AGENT_FACTORY_HOME", home)

	err := CleanupSessions(partialCaptureThenAbsentAfterTriggerExecutor(t, name, trigger, parentPIDFile))
	require.ErrorContains(t, err, "has no AF_SESSION marker")
	// The assertion above already proves the child existed and was captured —
	// nothing else in this fixture is unmarked — so this rendezvous only waits
	// out the child's own write, and cannot mask a child that never appeared.
	waitForPIDFile(t, childPIDFile)
	child := processFromPIDFile(t, childPIDFile)
	require.True(t, proctree.AliveSame(child),
		"an unmarked child discovered during recovery must be retained, not signalled")
}

// Recovery waits are bounded per session, but a batch must overlap them. A
// serial loop makes reset latency grow by a full grace period per vanished
// session and leaves unrelated live sessions mutable for the entire delay.
func TestCleanupSessionsRecoversVanishedSessionsConcurrently(t *testing.T) {
	testguard.IsolateTmux(t)
	oldGrace, oldTerm := reapGraceWait, reapTermWait
	reapGraceWait, reapTermWait = 500*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { reapGraceWait, reapTermWait = oldGrace, oldTerm })

	home := t.TempDir()
	names := []string{"af_concurrent_vanished_1", "af_concurrent_vanished_2", "af_concurrent_vanished_3"}
	for _, name := range names {
		spawnMarkedSessionWithEscapee(t, name, home)
	}
	t.Setenv("AGENT_FACTORY_HOME", home)

	// Observe that the recovery windows OVERLAP instead of timing the whole
	// sweep. The elapsed-time form inferred the property, and the inference put
	// machine speed in charge of the verdict: the concurrent floor is already
	// two grace waits, and three sessions' real tmux and /proc work has to fit
	// in what the bound leaves over. On the dev box at load 75 a correct
	// concurrent implementation measured 2.7s against a 2.5s bound and failed 8
	// runs out of 8 (#3439). The bound was also too loose to do its job in the
	// other direction — a serial sweep costs ~3s of waits, which the same 2.5s
	// bound only caught because overhead pushed it over.
	//
	// A barrier makes the observation exact rather than probable: each recovery
	// holds at the top of its bounded grace wait until every one has arrived.
	// Concurrent recoveries release each other at once; a serial sweep can never
	// raise the count, falls through the bound, and the peak below reports one
	// window instead of three. No machine speed enters the verdict.
	//
	// The observer brackets the WAIT, not the worker goroutine, and the
	// difference is the whole test: a recovery that launched all three workers
	// and then serialized their waits behind a shared lock would satisfy a
	// worker-level observer while reset latency grew per session again.
	// Mutation-tested both ways.
	var mu sync.Mutex
	inFlight, peak := 0, 0
	allOverlapped := make(chan struct{})
	var once sync.Once
	recoveryWindowObserver = func(_ string, entered bool) {
		mu.Lock()
		if entered {
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
		} else {
			inFlight--
		}
		reached := inFlight == len(names)
		mu.Unlock()
		if !entered {
			return
		}
		if reached {
			once.Do(func() { close(allOverlapped) })
		}
		select {
		case <-allOverlapped:
		case <-time.After(overlapBarrierBound):
		}
	}
	t.Cleanup(func() { recoveryWindowObserver = nil })

	require.NoError(t, CleanupSessions(vanishDuringCaptureExecutor(t, names...)))

	mu.Lock()
	got := peak
	mu.Unlock()
	require.Equal(t, len(names), got,
		"vanished-session recovery waits ran serially: only %d of %d windows were ever open at once", got, len(names))
}

// overlapBarrierBound releases a recovery that is waiting for siblings which
// are never going to arrive. It is reached only when the implementation has
// already failed the assertion, so it costs nothing on a passing run and only
// keeps a failing one from hanging until the package deadline.
const overlapBarrierBound = 5 * time.Second

// A generic capture error can accompany a verified partial process tree. The
// session may also own a process that sanitized away its diagnostic markers.
// The partial tree cannot prove what capture missed, so it must remain live
// with the session until a later complete capture authorizes teardown.
func TestCleanupSessionsPreservesPartialCaptureAfterGenericFailure(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_partial_failed_capture"
	escapee := spawnSessionWithEscapee(t, name)
	home, err := afHomeDir()
	require.NoError(t, err)
	out, err := exec.Command("tmux", "set-environment", "-t", exactTarget(name), EnvMarkerHome, home).CombinedOutput()
	require.NoError(t, err, "set ownership marker: %s", out)

	var warnings bytes.Buffer
	oldWarningOut := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(oldWarningOut) })

	err = CleanupSessions(partiallyFailSecondPaneCaptureExecutor(t))
	require.ErrorContains(t, err, "not-a-pane-pid",
		"a partial capture must remain a refusal before session teardown")
	require.True(t, proctree.AliveSame(escapee),
		"a partial capture must not reap processes while their session remains live")
	exists, known, probeErr := probeSessionStrict(cmd.MakeExecutor(), name)
	require.NoError(t, probeErr)
	require.True(t, known && exists, "partial capture failure must leave the session alive")
	require.NotContains(t, warnings.String(), "leaked past its pane tree",
		"a process in a retained live session must not be classified as an escaped leak")
}

// A pane can launch a helper after the pre-marker process snapshot and before
// marker lookup loses the session. The post-absence refresh must find that
// newly marked process rather than treating the older snapshot as final.
func TestCleanupSessionsReapsHelperStartedAfterPreMarkerCapture(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_vanished_late_helper"
	home := t.TempDir()
	trigger, pidFile := spawnSessionWaitingToStartHelper(t, name, home)
	t.Setenv("AGENT_FACTORY_HOME", home)

	var marked proctree.Process
	beforeKill := func() {
		require.NoError(t, os.WriteFile(trigger, []byte("go"), 0o600))
		var pid int
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(pidFile)
			if err != nil {
				return false
			}
			pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || pid <= 1 {
				return false
			}
			snap, err := proctree.Snapshot()
			if err != nil {
				return false
			}
			var ok bool
			marked, ok = snap[pid]
			return ok
		}, 5*time.Second, 20*time.Millisecond, "late helper never appeared")
	}

	require.NoError(t, CleanupSessions(vanishOnMarkerExecutorWith(t, name, beforeKill)))
	require.False(t, proctree.AliveSame(marked),
		"helper started after pre-marker capture survived vanished-session cleanup")
}

// The process sweep's AF_HOME check is an authorization boundary, not just a
// filter: a same-named helper from another install must never be signalled.
func TestCleanupSessionsDoesNotReapForeignHomeProcessForVanishedSession(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_vanished_foreign_helper"
	marked := spawnMarkedSessionWithEscapee(t, name, t.TempDir())
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	require.NoError(t, CleanupSessions(vanishOnMarkerExecutor(t, name)))
	require.True(t, proctree.AliveSame(marked),
		"a foreign-home helper with the same session name must not be reaped")
}

// A matching AF_SESSION without readable home ownership is not foreign and is
// not ours: it is unknown. Leave the process alive and stop reset before it can
// delete a worktree the process may still be using.
func TestCleanupSessionsFailsClosedOnUnownedProcessForVanishedSession(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_vanished_unowned_helper"
	marked := spawnMarkedSessionWithEscapee(t, name, "")
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	err := CleanupSessions(vanishOnMarkerExecutor(t, name))
	require.ErrorContains(t, err, "has no AF_HOME ownership marker")
	require.True(t, proctree.AliveSame(marked),
		"a process with unknown home ownership must be left alone")
}

func TestAddOrReplaceOrphanCandidateReplacesRecycledPID(t *testing.T) {
	stale := proctree.Process{PID: 42, StartID: 100}
	current := proctree.Process{PID: 42, StartID: 200}
	candidates := []proctree.Process{stale}
	byPID := map[int]int{stale.PID: 0}

	candidates = addOrReplaceOrphanCandidate(candidates, byPID, current)

	require.Equal(t, []proctree.Process{current}, candidates,
		"a current marked process must replace a stale identity that reused its PID")
}

func spawnMarkedSessionWithEscapee(t *testing.T, name, home string, generation ...string) proctree.Process {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "escapee.pid")
	args := []string{"new-session", "-d", "-s", name, "-c", dir, "-e", EnvMarkerSession + "=" + name}
	if home != "" {
		args = append(args, "-e", EnvMarkerHome+"="+home)
	}
	if len(generation) > 0 {
		args = append(args, "-e", EnvMarkerGeneration+"="+generation[0])
	}
	args = append(args, "nohup sleep 300 >/dev/null 2>&1 & "+recordPIDShell("$!", pidFile)+"; exec sleep 300")
	out, err := exec.Command("tmux", args...).CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	testguard.KeepTmuxServerOnEmpty(t)

	var pid int
	require.Eventually(t, func() bool {
		data, readErr := os.ReadFile(pidFile)
		if readErr != nil {
			return false
		}
		pid, readErr = strconv.Atoi(strings.TrimSpace(string(data)))
		return readErr == nil && pid > 1
	}, 5*time.Second, 20*time.Millisecond, "helper pid file never appeared")
	t.Cleanup(func() {
		if snap, snapErr := proctree.Snapshot(); snapErr == nil {
			if process, ok := snap[pid]; ok {
				_ = proctree.Signal(process, syscall.SIGKILL)
			}
		}
	})

	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	marked, ok := snap[pid]
	require.True(t, ok, "helper %d not in process snapshot", pid)
	return marked
}

func spawnSessionWaitingToStartHelper(t *testing.T, name, home string) (trigger, pidFile string) {
	t.Helper()
	dir := t.TempDir()
	trigger = filepath.Join(dir, "start-helper")
	pidFile = filepath.Join(dir, "late-helper.pid")
	script := fmt.Sprintf("while [ ! -f %s ]; do sleep 0.01; done; "+
		"nohup sleep 300 >/dev/null 2>&1 & %s; exec sleep 300", trigger, recordPIDShell("$!", pidFile))
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", dir,
		"-e", EnvMarkerSession+"="+name, "-e", EnvMarkerHome+"="+home, script).CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	testguard.KeepTmuxServerOnEmpty(t)
	t.Cleanup(func() {
		if data, readErr := os.ReadFile(pidFile); readErr == nil {
			if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil {
				if snap, snapErr := proctree.Snapshot(); snapErr == nil {
					if process, ok := snap[pid]; ok {
						_ = proctree.Signal(process, syscall.SIGKILL)
					}
				}
			}
		}
	})
	return trigger, pidFile
}

func vanishOnMarkerExecutor(t *testing.T, name string) cmd.Executor {
	return vanishOnMarkerExecutorWith(t, name, nil)
}

func vanishOnMarkerExecutorWith(t *testing.T, name string, beforeKill func()) cmd.Executor {
	t.Helper()
	realExec := cmd.MakeExecutor()
	vanished := false
	return cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if len(command.Args) > 1 && command.Args[1] == "show-environment" && !vanished {
				vanished = true
				if beforeKill != nil {
					beforeKill()
				}
				out, err := exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
				require.NoError(t, err, "kill session before marker lookup: %s", out)
			}
			return realExec.Output(command)
		},
	}
}

func vanishOnSecondPaneCaptureExecutor(t *testing.T, name string) cmd.Executor {
	t.Helper()
	realExec := cmd.MakeExecutor()
	paneCaptures := 0
	return cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if len(command.Args) > 1 && command.Args[1] == "list-panes" {
				paneCaptures++
				if paneCaptures == 2 {
					out, err := exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
					require.NoError(t, err, "kill session before second pane capture: %s", out)
				}
			}
			return realExec.Output(command)
		},
	}
}

func failPostMarkerPaneCaptureExecutor(t *testing.T) cmd.Executor {
	t.Helper()
	realExec := cmd.MakeExecutor()
	paneCaptures := 0
	return cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if len(command.Args) > 1 && command.Args[1] == "list-panes" {
				paneCaptures++
				if paneCaptures%2 == 0 {
					return nil, fmt.Errorf("injected generic list-panes failure")
				}
			}
			return realExec.Output(command)
		},
	}
}

func partiallyFailSecondPaneCaptureExecutor(t *testing.T) cmd.Executor {
	t.Helper()
	realExec := cmd.MakeExecutor()
	paneCaptures := 0
	return cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if len(command.Args) > 1 && command.Args[1] == "list-panes" {
				paneCaptures++
				if paneCaptures == 2 {
					out, err := realExec.Output(command)
					return append(out, []byte("not-a-pane-pid\n")...), err
				}
			}
			return realExec.Output(command)
		},
	}
}

func mixedCaptureFailureExecutor(t *testing.T, vanishedName, failedName string) cmd.Executor {
	t.Helper()
	realExec := cmd.MakeExecutor()
	paneCaptures := make(map[string]int)
	return cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if len(command.Args) <= 1 || command.Args[1] != "list-panes" {
				return realExec.Output(command)
			}
			joined := strings.Join(command.Args, " ")
			for _, name := range []string{vanishedName, failedName} {
				if strings.Contains(joined, name) {
					paneCaptures[name]++
				}
			}
			switch {
			case paneCaptures[vanishedName] == 2 && strings.Contains(joined, vanishedName):
				out, err := exec.Command("tmux", "kill-session", "-t", exactTarget(vanishedName)).CombinedOutput()
				require.NoError(t, err, "kill vanished fixture session: %s", out)
				return realExec.Output(command)
			case paneCaptures[failedName] == 2 && strings.Contains(joined, failedName):
				return nil, fmt.Errorf("injected generic list-panes failure")
			default:
				return realExec.Output(command)
			}
		},
	}
}

func vanishAfterPaneListExecutor(t *testing.T, name string) cmd.Executor {
	t.Helper()
	realExec := cmd.MakeExecutor()
	paneCaptures := 0
	return cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if len(command.Args) > 1 && command.Args[1] == "list-panes" {
				paneCaptures++
				if paneCaptures == 2 {
					out, err := exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
					require.NoError(t, err, "kill session after pane list: %s", out)
					// A PID that cannot exist makes the post-list snapshot report the
					// concrete generic "pane process disappeared" capture error.
					return []byte("99999999\n"), nil
				}
			}
			return realExec.Output(command)
		},
	}
}

func vanishCompleteBeforeLaterFailureExecutor(t *testing.T, completeName, failedName string) cmd.Executor {
	t.Helper()
	realExec := cmd.MakeExecutor()
	paneCaptures := make(map[string]int)
	return cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if len(command.Args) <= 1 || command.Args[1] != "list-panes" {
				return realExec.Output(command)
			}
			joined := strings.Join(command.Args, " ")
			for _, name := range []string{completeName, failedName} {
				if strings.Contains(joined, name) {
					paneCaptures[name]++
				}
			}
			if paneCaptures[failedName] == 2 && strings.Contains(joined, failedName) {
				_, _ = exec.Command("tmux", "kill-session", "-t", exactTarget(completeName)).CombinedOutput()
				return nil, fmt.Errorf("injected later capture failure")
			}
			return realExec.Output(command)
		},
	}
}

func partialCaptureThenAbsentExecutor(t *testing.T, name, trigger, pidFile string, helper *proctree.Process) cmd.Executor {
	t.Helper()
	realExec := cmd.MakeExecutor()
	paneCaptures := 0
	helperStarted := false
	return cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if len(command.Args) > 1 && command.Args[1] == "show-environment" && !helperStarted {
				helperStarted = true
				require.NoError(t, os.WriteFile(trigger, []byte("go"), 0o600))
				waitForPIDFile(t, pidFile)
				*helper = processFromPIDFile(t, pidFile)
			}
			if len(command.Args) > 1 && command.Args[1] == "list-panes" {
				paneCaptures++
				if paneCaptures == 2 {
					out, err := realExec.Output(command)
					return append(out, []byte("not-a-pane-pid\n")...), err
				}
			}
			if len(command.Args) > 1 && command.Args[1] == "has-session" {
				_, _ = exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
			}
			return realExec.Output(command)
		},
	}
}

func replaceWithForeignPartialThenAbsentExecutor(t *testing.T, name, foreignHome string, foreign *proctree.Process) cmd.Executor {
	t.Helper()
	realExec := cmd.MakeExecutor()
	paneCaptures := 0
	return cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if len(command.Args) > 1 && command.Args[1] == "list-panes" {
				paneCaptures++
				if paneCaptures == 2 {
					_, _ = exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
					*foreign = spawnMarkedSessionWithEscapee(t, name, foreignHome)
					out, err := realExec.Output(command)
					return append(out, []byte("not-a-pane-pid\n")...), err
				}
			}
			if len(command.Args) > 1 && command.Args[1] == "has-session" {
				_, _ = exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
			}
			return realExec.Output(command)
		},
	}
}

func partialCaptureThenAbsentAfterTriggerExecutor(t *testing.T, name, trigger, parentPIDFile string) cmd.Executor {
	t.Helper()
	realExec := cmd.MakeExecutor()
	paneCaptures := 0
	triggered := false
	return cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if len(command.Args) > 1 && command.Args[1] == "show-environment" && !triggered {
				triggered = true
				require.NoError(t, os.WriteFile(trigger, []byte("go"), 0o600))
				waitForPIDFile(t, parentPIDFile)
			}
			if len(command.Args) > 1 && command.Args[1] == "list-panes" {
				paneCaptures++
				if paneCaptures == 2 {
					out, err := realExec.Output(command)
					return append(out, []byte("not-a-pane-pid\n")...), err
				}
			}
			if len(command.Args) > 1 && command.Args[1] == "has-session" {
				_, _ = exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
			}
			return realExec.Output(command)
		},
	}
}

func vanishDuringCaptureExecutor(t *testing.T, names ...string) cmd.Executor {
	t.Helper()
	realExec := cmd.MakeExecutor()
	paneCaptures := make(map[string]int)
	return cmd_test.MockCmdExec{
		RunFunc: realExec.Run,
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if len(command.Args) <= 1 || command.Args[1] != "list-panes" {
				return realExec.Output(command)
			}
			joined := strings.Join(command.Args, " ")
			for _, name := range names {
				if !strings.Contains(joined, name) {
					continue
				}
				paneCaptures[name]++
				if paneCaptures[name] == 2 {
					_, _ = exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
				}
			}
			return realExec.Output(command)
		},
	}
}

func spawnSessionWaitingToStartUnmarkedHelper(t *testing.T, name, home string) (trigger, pidFile string) {
	t.Helper()
	dir := t.TempDir()
	trigger = filepath.Join(dir, "start-helper")
	pidFile = filepath.Join(dir, "unmarked-helper.pid")
	script := fmt.Sprintf("while [ ! -f %s ]; do sleep 0.01; done; "+
		"nohup env -u AF_SESSION -u AF_HOME setsid sleep 300 >/dev/null 2>&1 & %s; exec sleep 300",
		trigger, recordPIDShell("$!", pidFile))
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", dir,
		"-e", EnvMarkerSession+"="+name, "-e", EnvMarkerHome+"="+home, script).CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	testguard.KeepTmuxServerOnEmpty(t)
	t.Cleanup(func() {
		if data, readErr := os.ReadFile(pidFile); readErr == nil {
			if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil {
				if snap, snapErr := proctree.Snapshot(); snapErr == nil {
					if process, ok := snap[pid]; ok {
						_ = proctree.Signal(process, syscall.SIGKILL)
					}
				}
			}
		}
	})
	return trigger, pidFile
}

func spawnSessionWaitingToForkUnmarkedHelper(t *testing.T, name, home string) (trigger, parentPIDFile, childPIDFile string) {
	t.Helper()
	dir := t.TempDir()
	trigger = filepath.Join(dir, "start-parent")
	parentPIDFile = filepath.Join(dir, "marked-parent.pid")
	childPIDFile = filepath.Join(dir, "unmarked-child.pid")
	// The CHILD records its own pid, from a script file rather than a second
	// level of nested single quotes. Two reasons, both about who may be killed:
	//
	//   - The marked parent is SIGTERMed by the very sweep this test observes.
	//     Recording the child from the parent puts that write inside a window its
	//     own killer can interrupt, and no rename can help a process that never
	//     reaches it. The child is the one process this test asserts is NEVER
	//     signalled, so it is the safe writer.
	//   - `$!` is the pid of `nohup`, which is the sleeper's pid only because
	//     setsid happens not to fork here (it forks only when already a process
	//     group leader). `$$` inside the child is right either way.
	recorder := filepath.Join(dir, "record-child-pid.sh")
	require.NoError(t, os.WriteFile(recorder, []byte("#!/bin/sh\n"+
		recordPIDShell("$$", "$1")+"\nexec sleep 300\n"), 0o700))
	script := fmt.Sprintf("while [ ! -f %s ]; do sleep 0.01; done; "+
		"nohup sh -c '%s; sleep 0.1; "+
		"nohup env -u AF_SESSION -u AF_HOME setsid sh %s %s >/dev/null 2>&1 & exec sleep 300' "+
		">/dev/null 2>&1 & exec sleep 300",
		trigger, recordPIDShell("$$", parentPIDFile), recorder, childPIDFile)
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", dir,
		"-e", EnvMarkerSession+"="+name, "-e", EnvMarkerHome+"="+home, script).CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	testguard.KeepTmuxServerOnEmpty(t)
	t.Cleanup(func() {
		for _, pidFile := range []string{parentPIDFile, childPIDFile} {
			if data, readErr := os.ReadFile(pidFile); readErr == nil {
				if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil {
					if snap, snapErr := proctree.Snapshot(); snapErr == nil {
						if process, ok := snap[pid]; ok {
							_ = proctree.Signal(process, syscall.SIGKILL)
						}
					}
				}
			}
		}
	})
	return trigger, parentPIDFile, childPIDFile
}

func waitForPIDFile(t *testing.T, pidFile string) {
	t.Helper()
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 1
	}, 5*time.Second, 20*time.Millisecond, "helper pid file never appeared")
}

func processFromPIDFile(t *testing.T, pidFile string) proctree.Process {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	process, ok := snap[pid]
	require.True(t, ok, "helper %d not in process snapshot", pid)
	return process
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

// recordPIDShell renders a shell fragment that records a pid into pidFile
// ATOMICALLY: write a sibling temp file, then rename over the target.
//
// `echo $! > f` is TWO steps — open(O_CREAT|O_TRUNC), then write — so between
// them the file EXISTS and is EMPTY. A reader that lands there gets "" and
// fails with `strconv.Atoi: parsing "": invalid syntax`, a very long way from
// the cause; a writer killed there leaves the empty file behind permanently.
// Neither is hypothetical: a tight reader loop against the un-renamed form
// caught the empty window in 119 of 300 runs of this fixture, and 0 of 300 with
// the rename. rename(2) is atomic within a directory, so a reader can only ever
// observe "not there yet" or the complete pid (#3469 CI, run 33149836815).
func recordPIDShell(pidExpr, pidFile string) string {
	return fmt.Sprintf("echo %s > \"%s.tmp\" && mv \"%s.tmp\" \"%s\"", pidExpr, pidFile, pidFile, pidFile)
}

// spawnLiveProcess starts a real child that outlives the test body and returns
// its process identity. Used where a test needs a genuinely live PID rather
// than a fabricated one, so an assertion about a reported process is about
// something that actually exists.
func spawnLiveProcess(t *testing.T) proctree.Process {
	t.Helper()
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
	return proc
}

// TestCleanupSessionsReportsProcessesThatSurvivedSIGKILL is the #3414
// regression, and it is about a DROPPED RETURN VALUE.
//
// CleanupSessions is what `af reset` runs immediately before it deletes every
// worktree, prunes branches, and erases the records that name them. The reaper
// it calls returns the processes that were still alive after SIGKILL — the
// whole point of a bounded escalation is that its verdict can be "I did not
// win" — and that value was discarded. A process still writing into a worktree
// therefore left no trace in the returned error, reset saw a clean sweep, and
// the directory was deleted out from under it.
//
// This is the rule the rest of the codebase already applies: an unconfirmed
// teardown is not a confirmed one. CloseAndWaitForPaneExit refuses on exactly
// this signal ("pane processes %s are still alive after bounded teardown"),
// reapUnusableSandbox (#3478) and TeardownStateUnknown refuse to turn "could
// not confirm" into a verdict, and #3510 reaps worktree writers before
// relocating an archive for the same reason. CleanupSessions was the one
// teardown path that looked away.
//
// The reap itself stays REAL — the stub delegates to it, so the session's
// actual escapee is still reaped and the sibling test's contract still holds.
// Only the verdict is forced, because the states that genuinely survive SIGKILL
// (uninterruptible sleep, a process this uid may not signal) are not something
// a test can conjure on demand: proctree counts a zombie as gone, so the usual
// trick does not produce one either.
func TestCleanupSessionsReportsProcessesThatSurvivedSIGKILL(t *testing.T) {
	testguard.IsolateTmux(t)
	shrinkReapWaits(t)

	const name = "af_survivor-report-test"
	spawnSessionWithEscapee(t, name)

	// The sweep only kills sessions it can prove this home owns (#1122); the
	// raw `tmux new-session` above does not go through the af creation path.
	home, err := afHomeDir()
	require.NoError(t, err)
	out, err := exec.Command("tmux", "set-environment", "-t", "="+name, EnvMarkerHome, home).CombinedOutput()
	require.NoError(t, err, "set-environment: %s", out)

	survivor := spawnLiveProcess(t)
	realReap := reapOnRequestProcesses
	reapOnRequestProcesses = func(reason reapReason, sanitizedName string, procs []proctree.Process,
		grace, termWait time.Duration,
	) []proctree.Process {
		realReap(reason, sanitizedName, procs, grace, termWait)
		return []proctree.Process{survivor}
	}
	t.Cleanup(func() { reapOnRequestProcesses = realReap })

	err = CleanupSessions(cmd.MakeExecutor())
	require.Error(t, err, "CleanupSessions returned nil while a process survived SIGKILL — "+
		"`af reset` goes on to delete this session's worktree with that process still writing to it (#3414)")
	require.Contains(t, err.Error(), strconv.Itoa(survivor.PID),
		"the error must name the surviving PID: aborting the reset is only actionable if the user "+
			"is told what to go look at")
	require.Contains(t, err.Error(), name, "the error must name the session the survivor came from")
}

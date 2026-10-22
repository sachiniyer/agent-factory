package daemon

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

	"github.com/sachiniyer/agent-factory/internal/watchlifeline"
	"github.com/sachiniyer/agent-factory/task"
)

// parentArgs is the command line of pid's parent, through ps so it reads the
// same on Linux and macOS.
func parentArgs(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps ppid of %d: %v", pid, err)
	}
	ppid := strings.TrimSpace(string(out))
	args, err := exec.Command("ps", "-o", "args=", "-p", ppid).Output()
	if err != nil {
		t.Fatalf("ps args of %s: %v", ppid, err)
	}
	return strings.TrimSpace(string(args))
}

// TestWatchCommandRunsUnderLifeline: the supervisor's default spawn is the
// lifeline, not a bare shell — the property every other watcher test in this
// package now exercises implicitly.
func TestWatchCommandRunsUnderLifeline(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "shell.pid")
	s, _ := newTestSupervisor(t, staticTasks(watchTask("lifeline1", "echo $$ > "+pidFile+"; sleep 300", dir)))
	if s.lifelineExe == "" {
		t.Fatal("no lifeline executable resolved for this test binary")
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	var shell int
	waitUntil(t, 5*time.Second, "watch command to start", func() bool {
		var ok bool
		shell, ok = readPIDFile(pidFile)
		return ok
	})
	if args := parentArgs(t, shell); !strings.Contains(args, watchlifeline.Arg+" "+watchlifeline.Protocol) {
		t.Fatalf("watch shell's parent is %q, want the watch lifeline", args)
	}
}

// TestWatchTaskRemoveAndUpdateEndPromptly: the lifeline catches the group
// SIGTERM so it can keep supervising, so removing or editing a task must still
// end a TERM-ignoring watch command through the SIGKILL escalation, within the
// stop grace — not leave it running under a lifeline that outlived the stop.
func TestWatchTaskRemoveAndUpdateEndPromptly(t *testing.T) {
	for _, change := range []string{"remove", "update"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "shell.pid")
			script := `trap "" TERM; echo $$ > ` + pidFile + `; while :; do sleep 0.05; done`
			current := []task.Task{watchTask("stubborn", script, dir)}
			s, _ := newTestSupervisor(t, func() ([]task.Task, error) { return current, nil })
			if err := s.Reload(); err != nil {
				t.Fatal(err)
			}
			var shell int
			waitUntil(t, 5*time.Second, "watch command to start", func() bool {
				var ok bool
				shell, ok = readPIDFile(pidFile)
				return ok
			})
			time.Sleep(100 * time.Millisecond)
			if args := parentArgs(t, shell); !strings.Contains(args, watchlifeline.Arg) {
				t.Fatalf("watch shell's parent is %q, want the watch lifeline", args)
			}

			switch change {
			case "remove":
				current = nil
			case "update":
				current = []task.Task{watchTask("stubborn", "sleep 300", dir)}
			}
			start := time.Now()
			if err := s.Reload(); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, s.stopGrace+3*time.Second, "the TERM-ignoring watch command to be killed", func() bool {
				return processGone(shell)
			})
			if elapsed := time.Since(start); elapsed > s.stopGrace+3*time.Second {
				t.Fatalf("task %s took %s to end its watch command", change, elapsed)
			}
		})
	}
}

// TestGroupKillPrecedesLeaderReap pins the order reapWatchGroupLeader exists
// for: the cleanup SIGKILL is sent while the leader is still unreaped, so its
// pid — the group's number — cannot have been reused. Waiting first and killing
// after, as the watcher did before the lifeline, would fail here.
func TestGroupKillPrecedesLeaderReap(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid

	var kills []string
	kill := watchGroupKill
	t.Cleanup(func() { watchGroupKill = kill })
	watchGroupKill = func(target int, sig syscall.Signal) error {
		// Signal 0 to the leader's own pid succeeds for a zombie and fails
		// with ESRCH once it has been reaped.
		unreaped := syscall.Kill(pid, 0) == nil
		kills = append(kills, strconv.Itoa(target)+"/"+sig.String()+"/unreaped="+strconv.FormatBool(unreaped))
		return kill(target, sig)
	}

	if _, err := reapWatchGroupLeader(cmd, nil); err != nil {
		t.Fatal(err)
	}
	want := strconv.Itoa(-pid) + "/" + syscall.SIGKILL.String() + "/unreaped=true"
	if len(kills) != 1 || kills[0] != want {
		t.Fatalf("group kills %v, want exactly [%s]: the cleanup kill must land before the leader is reaped", kills, want)
	}
	if !cmd.ProcessState.Exited() {
		t.Fatalf("leader not reaped after reapWatchGroupLeader: %v", cmd.ProcessState)
	}
}

// TestLifelineFallsBackForBinaryWithoutProtocol is the version-skew pair the
// ack exists for: after `af upgrade` on macOS the binary at the daemon's path
// can be an af that predates (or postdates) this protocol. The real pre-lifeline
// af answers the hook's argv with Cobra's unknown-command error and exit 1,
// writing nothing on stdout; this stand-in does exactly that. The watch command
// must still run, started directly, and the supervisor must stop retrying.
func TestLifelineFallsBackForBinaryWithoutProtocol(t *testing.T) {
	dir := t.TempDir()
	oldAF := filepath.Join(dir, "af-old")
	stub := "#!/bin/sh\necho 'Error: unknown command \"" + watchlifeline.Arg + "\" for \"af\"' >&2\necho \"Run 'af --help' for usage.\" >&2\nexit 1\n"
	if err := os.WriteFile(oldAF, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "shell.pid")
	warnings := captureWarnings(t)
	s, _ := newTestSupervisor(t, staticTasks(watchTask("skew1", "echo $$ > "+pidFile+"; sleep 300", dir)))
	s.lifelineExe = oldAF
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	var shell int
	waitUntil(t, 5*time.Second, "watch command to start without a lifeline", func() bool {
		var ok bool
		shell, ok = readPIDFile(pidFile)
		return ok
	})
	if args := parentArgs(t, shell); strings.Contains(args, watchlifeline.Arg) {
		t.Fatalf("watch shell's parent is %q; the fallback should have started it directly", args)
	}
	if !s.lifelineUnsupported.Load() {
		t.Fatal("supervisor did not latch the binary as not speaking the lifeline protocol")
	}
	// A silent fallback would quietly reopen #5329 on exactly this skew, so
	// the warning must name the binary and say what the fallback costs.
	got := warnings.String()
	t.Logf("fallback warning: %s", strings.TrimSpace(got))
	for _, want := range []string{oldAF, "does not speak the watch lifeline protocol",
		"a watch child can outlive this daemon", "af daemon restart"} {
		if !strings.Contains(got, want) {
			t.Fatalf("fallback warning %q does not contain %q", got, want)
		}
	}
	if n := strings.Count(got, "does not speak the watch lifeline protocol"); n != 1 {
		t.Fatalf("fallback warned %d times, want once per daemon", n)
	}
}

// TestLifelineShellStartFailureIsAStartError: a shell the lifeline cannot
// start is the same start error a direct spawn reports, not a lifeline
// problem — the supervisor keeps using the lifeline.
func TestLifelineShellStartFailureIsAStartError(t *testing.T) {
	s, _ := newTestSupervisor(t, staticTasks())
	s.shell = "/nonexistent/af-test-shell"
	w := s.newTaskWatcher(watchTask("noshell", "true", t.TempDir()))
	_, err := w.runOnce()
	if err == nil || !strings.Contains(err.Error(), "start /nonexistent/af-test-shell") {
		t.Fatalf("runOnce error %v, want the lifeline's start failure", err)
	}
	if s.lifelineUnsupported.Load() {
		t.Fatal("a shell start failure latched the lifeline as unsupported")
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("start failure surfaced as an exit (%v), not a start error", err)
	}
}

// TestLifelineRunReportsTheShellsStatus: the lifeline always dies of its own
// group SIGKILL once the shell has run, so the run's result must come from the
// status it reported — a clean exit stays a clean exit, a failure keeps its
// code, and a SIGTERM death still reads as stop/reload.
func TestLifelineRunReportsTheShellsStatus(t *testing.T) {
	s, _ := newTestSupervisor(t, staticTasks())
	run := func(script string) error {
		t.Helper()
		_, err := s.newTaskWatcher(watchTask("status", script, t.TempDir())).runOnce()
		return err
	}
	if err := run("exit 0"); err != nil {
		t.Fatalf("exit 0 under the lifeline returned %v, want a clean exit", err)
	}
	if err := run("exit 3"); err == nil || err.Error() != "exit status 3" {
		t.Fatalf("exit 3 under the lifeline returned %v, want exit status 3", err)
	}
	if err := run("kill -TERM $$; sleep 5"); !exitedFromSignal(err, syscall.SIGTERM) {
		t.Fatalf("a SIGTERM death under the lifeline returned %v, want it to read as SIGTERM", err)
	}
	if s.lifelineUnsupported.Load() {
		t.Fatal("these runs fell back from the lifeline")
	}
}

// TestTaskStartFailureKeepsTheLifeline: a task whose project directory is gone
// cannot start under the lifeline or without it. That is the task's failure,
// and every other task must still get a lifeline.
func TestTaskStartFailureKeepsTheLifeline(t *testing.T) {
	s, _ := newTestSupervisor(t, staticTasks())
	missing := filepath.Join(t.TempDir(), "gone")
	if _, err := s.newTaskWatcher(watchTask("nodir", "true", missing)).runOnce(); err == nil {
		t.Fatal("a watch task in a missing directory started")
	}
	if s.lifelineUnsupported.Load() {
		t.Fatal("one task's missing directory turned the lifeline off for the whole daemon")
	}
}

// TestUnexecutableLifelineFallsBack is the other half: when the lifeline
// binary itself cannot be exec'd but the shell can, the binary is the problem,
// and the supervisor falls back once and says so.
func TestUnexecutableLifelineFallsBack(t *testing.T) {
	s, _ := newTestSupervisor(t, staticTasks())
	s.lifelineExe = filepath.Join(t.TempDir(), "no-such-af")
	warnings := captureWarnings(t)
	if _, err := s.newTaskWatcher(watchTask("noexe", "exit 0", t.TempDir())).runOnce(); err != nil {
		t.Fatalf("the shell did not run without its lifeline: %v", err)
	}
	if !s.lifelineUnsupported.Load() {
		t.Fatal("an unexecutable lifeline binary was not latched")
	}
	if got := warnings.String(); !strings.Contains(got, s.lifelineExe) || !strings.Contains(got, "cannot exec it") {
		t.Fatalf("fallback warning %q does not name the binary and the exec failure", got)
	}
}

// TestReapFallbackKeepsTheWatchdogUntilExit: when the leader's exit cannot be
// awaited without reaping it, the stop watchdog must stay in charge until the
// leader has actually exited — ending it first would leave a later stop with
// nothing to signal the group, and the stop would hang on the reap.
func TestReapFallbackKeepsTheWatchdogUntilExit(t *testing.T) {
	exited := watchLeaderExited
	t.Cleanup(func() { watchLeaderExited = exited })
	watchLeaderExited = func(int) error { return errors.New("stubbed: no unreaped wait") }

	cmd := exec.Command("sleep", "0.3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	var leaderRunning bool
	if _, err := reapWatchGroupLeader(cmd, nil, func() { leaderRunning = !processGone(pid) }); err != nil {
		t.Fatal(err)
	}
	if leaderRunning {
		t.Fatal("the stop watchdog was ended while the watch command was still running")
	}
}

// TestSkippedGroupKillDoesNotWedgeTheRun: when the leader's exit cannot be
// awaited without reaping AND the group cannot be pinned, the group cleanup
// kill is skipped — and something the script backgrounded can still hold the
// run's stdout after the shell exits. The run must end anyway, within the
// stop grace, and say why — otherwise stop, reload and shutdown wait on that
// process forever.
func TestSkippedGroupKillDoesNotWedgeTheRun(t *testing.T) {
	exited := watchLeaderExited
	t.Cleanup(func() { watchLeaderExited = exited })
	watchLeaderExited = func(int) error { return errors.New("stubbed: no unreaped wait") }
	anchor := spawnWatchGroupAnchor
	t.Cleanup(func() { spawnWatchGroupAnchor = anchor })
	spawnWatchGroupAnchor = func(int) func() { return nil }

	s, _ := newTestSupervisor(t, staticTasks())
	s.lifelineExe = "" // a lifeline would kill the leftover itself on the shell's exit
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "leftover.pid")
	warnings := captureWarnings(t)

	done := make(chan error, 1)
	go func() {
		_, err := s.newTaskWatcher(watchTask("leftover", "sleep 30 & echo $! > "+pidFile+"; exit 0", dir)).runOnce()
		done <- err
	}()
	t.Cleanup(func() {
		if pid, ok := readPIDFile(pidFile); ok {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v, want the shell's clean exit", err)
		}
	case <-time.After(s.stopGrace + 3*time.Second):
		t.Fatal("the run is still waiting on the output a leftover process holds")
	}
	if !strings.Contains(warnings.String(), "still holds its output") {
		t.Fatalf("no warning that the run's output was closed under a leftover process; warnings:\n%s", warnings.String())
	}
}

// TestFailedProbeOutputStaysOutOfTheTask: a binary that does not speak the
// protocol still ran with the run's stdout and stderr, which the shell then
// inherits. What it printed is not the task's: it must not become an event,
// land in the task's stderr log or its failure tail — only in the warning.
func TestFailedProbeOutputStaysOutOfTheTask(t *testing.T) {
	dir := t.TempDir()
	oldAF := filepath.Join(dir, "af-old")
	stub := "#!/bin/sh\necho probe-stdout\necho probe-stderr >&2\nexit 1\n"
	if err := os.WriteFile(oldAF, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	s, rec := newTestSupervisor(t, staticTasks())
	s.lifelineExe = oldAF
	warnings := captureWarnings(t)
	tail, err := s.newTaskWatcher(watchTask("probe", "echo shell-stdout; echo shell-stderr >&2; exit 3", dir)).runOnce()
	if err == nil || err.Error() != "exit status 3" {
		t.Fatalf("run returned %v, want the shell's exit status 3", err)
	}
	logPath, _ := s.logPath("probe")
	stderrLog, _ := os.ReadFile(logPath)
	rec.mu.Lock()
	events := strings.Join(rec.events, "\n")
	rec.mu.Unlock()
	for what, got := range map[string]string{"events": events, "stderr log": string(stderrLog), "failure tail": tail.logSuffix()} {
		if strings.Contains(got, "probe-") {
			t.Errorf("the failed probe's output reached the task's %s:\n%s", what, got)
		}
	}
	if !strings.Contains(events, "shell-stdout") || !strings.Contains(string(stderrLog), "shell-stderr") {
		t.Fatalf("the shell's own output is missing: events %q, stderr log %q", events, stderrLog)
	}
	if got := warnings.String(); !strings.Contains(got, "probe-stdout") || !strings.Contains(got, "probe-stderr") {
		t.Fatalf("fallback warning %q does not carry what the probe printed", got)
	}
}

// TestFallbackReapKeepsTheGroupNumberPinned pins the property the stop
// watchdog's group signals rely on when the leader's exit can only be
// observed by reaping it: the group's number must still name this run's
// group while beforeKill disarms the watchdog — the window every watchdog
// signal in flight lands in — and while the cleanup kill is sent. Before the
// anchor, the reap freed the number first: kill(-pgid, 0) answered ESRCH
// inside beforeKill, so a signal landing there could hit a reused group. The
// leader must still be running when the anchor joins — a group reduced to a
// leader zombie can already be unjoinable — so it stays up for the probe.
func TestFallbackReapKeepsTheGroupNumberPinned(t *testing.T) {
	exited := watchLeaderExited
	t.Cleanup(func() { watchLeaderExited = exited })
	watchLeaderExited = func(int) error { return errors.New("stubbed: no unreaped wait") }

	kill := watchGroupKill
	t.Cleanup(func() { watchGroupKill = kill })
	var unpinned []string
	watchGroupKill = func(target int, sig syscall.Signal) error {
		// Signal 0 answers ESRCH only once the group is gone — the leader
		// reaped and nothing else keeping its number reserved.
		if err := kill(target, 0); errors.Is(err, syscall.ESRCH) {
			unpinned = append(unpinned, sig.String())
		}
		return kill(target, sig)
	}

	cmd := exec.Command("sleep", "0.5")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid

	pinned := false
	groupKilled, err := reapWatchGroupLeader(cmd, nil, func() {
		pinned = syscall.Kill(-pgid, 0) == nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !pinned {
		t.Fatal("the group number was released before the watchdog was disarmed; " +
			"a stop signal landing in this window could reach a reused group")
	}
	if !groupKilled {
		t.Fatal("the group cleanup kill was skipped although the number stayed pinned")
	}
	if len(unpinned) > 0 {
		t.Fatalf("group signals sent with the number unpinned: %v", unpinned)
	}
}

// TestLifelineProbeRetriesAfterCooldown: a failed probe suppresses the
// lifeline only for the retry cooldown, never for the daemon's life — a
// launcher failure (a systemd-run scope hiccup) reads exactly like a missing
// protocol, so latching off would silently reopen the orphaning until daemon
// restart. Inside the cooldown the shell starts directly; after it the probe
// runs again, and a speaking binary re-arms.
func TestLifelineProbeRetriesAfterCooldown(t *testing.T) {
	dir := t.TempDir()
	oldAF := filepath.Join(dir, "af-old")
	stub := "#!/bin/sh\necho 'Error: unknown command \"" + watchlifeline.Arg + "\" for \"af\"' >&2\nexit 1\n"
	if err := os.WriteFile(oldAF, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestSupervisor(t, staticTasks())
	realExe := s.lifelineExe
	if realExe == "" {
		t.Fatal("no lifeline executable resolved for this test binary")
	}
	s.lifelineExe = oldAF

	if _, err := s.newTaskWatcher(watchTask("degraded", "true", dir)).runOnce(); err != nil {
		t.Fatal(err)
	}
	if !s.lifelineUnsupported.Load() {
		t.Fatal("the failed probe was not recorded")
	}
	if until := s.lifelineProbeHold.Load(); until <= time.Now().UnixNano() {
		t.Fatal("a failed probe did not hold off the next probe")
	}

	// Inside the cooldown no probe runs — the shell starts directly even
	// though the binary at lifelineExe now speaks the protocol.
	s.lifelineExe = realExe
	pidFile := filepath.Join(dir, "cooldown.pid")
	if _, err := s.newTaskWatcher(watchTask("cooldown", "echo $$ > "+pidFile, dir)).runOnce(); err != nil {
		t.Fatal(err)
	}
	shell, ok := readPIDFile(pidFile)
	if !ok {
		t.Fatal("the cooldown run did not record its shell pid")
	}
	if args := parentArgs(t, shell); strings.Contains(args, watchlifeline.Arg) {
		t.Fatalf("a lifeline probe ran inside the cooldown (shell parent %q)", args)
	}

	// After the cooldown the probe runs again and a speaking binary clears
	// the recorded failure.
	s.lifelineProbeHold.Store(0)
	pidFile2 := filepath.Join(dir, "recovered.pid")
	if _, err := s.newTaskWatcher(watchTask("recovered", "echo $$ > "+pidFile2, dir)).runOnce(); err != nil {
		t.Fatal(err)
	}
	shell2, ok := readPIDFile(pidFile2)
	if !ok {
		t.Fatal("the recovered run did not record its shell pid")
	}
	if args := parentArgs(t, shell2); !strings.Contains(args, watchlifeline.Arg) {
		t.Fatalf("the post-cooldown run's shell parent is %q, want the watch lifeline", args)
	}
	if s.lifelineUnsupported.Load() {
		t.Fatal("a successful probe did not clear the recorded failure")
	}
}

// TestWatchGroupAnchorPinsUntilReleased pins the anchor's contract in a live
// group: spawned while the leader runs, it survives the group's SIGTERM —
// what a stop sends first — and still holds the group's number after the
// leader has been reaped, so a group signal in that window can only name
// this group. Releasing it ends the group.
func TestWatchGroupAnchorPinsUntilReleased(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	release := startWatchGroupAnchor(pgid)
	if release == nil {
		t.Fatal("the anchor could not join a group whose leader was running")
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM to the group: %v", err)
	}
	// The leader dies of the TERM (or the insurance KILL) and is reaped —
	// after this only the anchor can answer for the group.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("the anchor did not pin the group past the leader's reap and the group's TERM: %v", err)
	}
	release()
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the group should be empty once the anchor is released, got %v", err)
	}
}

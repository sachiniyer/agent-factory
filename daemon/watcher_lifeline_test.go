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
				return processGone(shell) || isZombie(shell)
			})
			if elapsed := time.Since(start); elapsed > s.stopGrace+3*time.Second {
				t.Fatalf("task %s took %s to end its watch command", change, elapsed)
			}
		})
	}
}

// isZombie reports a process that has exited but not yet been reaped by its
// new parent; it is no longer running.
func isZombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
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

	if err := reapWatchGroupLeader(cmd); err != nil {
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
	if err := reapWatchGroupLeader(cmd, func() { leaderRunning = !processGone(pid) }); err != nil {
		t.Fatal(err)
	}
	if leaderRunning {
		t.Fatal("the stop watchdog was ended while the watch command was still running")
	}
}

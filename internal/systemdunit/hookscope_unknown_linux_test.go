//go:build linux

package systemdunit

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
)

// These pin how the hook sweeps treat a process the launcher oracle cannot
// classify (#7239). That is a live process of ours with no readable argv: one
// mid-execve, which may be the very launcher the oracle looks for, or one
// mid-exit, whose address space is being torn down. The second lasts about
// 130 ms per GiB resident (measured in the #7242 review), so an ordinary exit
// is the common case, and archive's own teardown produces exactly those.

// startArgvlessProcess starts a live process of ours whose command line reads as
// nothing to classify for its whole life. It is exec'd with an EMPTY argv, so
// Linux 5.18 and later substitute argv = [""] and /proc/<pid>/cmdline is "\0".
// The sleeper needs no arguments and does not dispatch on argv[0].
func startArgvlessProcess(t *testing.T) *os.Process {
	t.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.StartProcess(sleeper.Path(t), []string{}, &os.ProcAttr{
		Dir:   t.TempDir(),
		Files: []*os.File{devNull, devNull, devNull},
	})
	_ = devNull.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The process this test started and recorded, and nothing else.
		_ = process.Kill()
		_, _ = process.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, lookupErr := proctree.Lookup(process.Pid)
		if lookupErr == nil && p.Comm == "sleeper" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture: the argv-less sleeper never finished exec: %+v %v", p, lookupErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if argv := proctree.Argv(process.Pid); argv != nil {
		t.Fatalf("fixture: an empty argv must read as no command line, got %q", argv)
	}
	return process
}

// R1, deterministic. The sweep must hold a process it cannot classify the way it
// holds a launcher it found: it waits within its settle budget and returns once
// the process is gone, rather than refusing on the first read. Refusing there
// fails every archive that lands while one of the session's own processes is
// exiting.
func TestStopHookScopesWaitsOutAnUnclassifiableProcess(t *testing.T) {
	installSystemctlShim(t, "exit 0\n")
	shortenLauncherSettle(t, 30*time.Second, 20*time.Millisecond)
	unknown := startArgvlessProcess(t)

	done := make(chan error, 1)
	go func() { done <- StopHookScopes(HookScopeUnitPrefix("s7239")) }()

	select {
	case err := <-done:
		t.Fatalf("StopHookScopes returned (%v) while a process of ours it could not classify was still alive; "+
			"an unknown must be waited on like a found launcher, not refused at once (#7239)", err)
	case <-time.After(stillBlockedWindow):
	}

	_ = unknown.Kill()
	_, _ = unknown.Wait()
	if err := waitForSweep(t, done); err != nil {
		t.Fatalf("StopHookScopes refused after the unclassifiable process was gone: %v", err)
	}
}

// R1's control. An unknown that outlives the whole budget still refuses. The
// refusal names the process and the time actually spent waiting, so the
// caller holds off on the same evidence a found-but-unregistered launcher gives.
func TestStopHookScopesRefusesAnUnclassifiableProcessThatOutlivesTheBudget(t *testing.T) {
	installSystemctlShim(t, "exit 0\n")
	shortenLauncherSettle(t, 300*time.Millisecond, 20*time.Millisecond)
	unknown := startArgvlessProcess(t)

	err := StopHookScopes(HookScopeUnitPrefix("s7239"))
	var unreadable *proctree.ArgvUnreadableError
	if !errors.As(err, &unreadable) || unreadable.PID != unknown.Pid {
		t.Fatalf("err = %v; want a refusal naming the unclassifiable pid %d", err, unknown.Pid)
	}
	var unsettled *HookLauncherUnclassifiedError
	if !errors.As(err, &unsettled) || unsettled.Waited < 300*time.Millisecond {
		t.Fatalf("err = %v; want a HookLauncherUnclassifiedError carrying the full budget it waited", err)
	}
}

// R1 on a REAL exit, the review's shape. A process of ours holding about 1 GiB
// is SIGKILLed, and the sweep starts while the kernel is still tearing its
// address space down: present in /proc, state R, same start stamp, and an empty
// command line. Measured on the devbox: 114 ms at 1 GiB, 212 ms at 2 GiB.
//
// The fixture must be SINGLE-threaded, which is why it is python3 and not a Go
// helper. In a multi-threaded process the thread-group leader exits at once and
// /proc/<tgid> reads Z within about 100 us, while another thread does the
// teardown. That never shows the window (measured on a 2 GiB Go child). A
// single-threaded process tears down on its leader, so the window is visible,
// and shells, interpreters and compilers are single-threaded.
func TestStopHookScopesDoesNotRefuseOverAnOrdinaryExit(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed; the single-threaded large-exit fixture needs it, " +
			"and TestStopHookScopesWaitsOutAnUnclassifiableProcess covers the same property deterministically")
	}
	installSystemctlShim(t, "exit 0\n")
	shortenLauncherSettle(t, 30*time.Second, 20*time.Millisecond)

	helper := exec.Command(python, "-c", "import sys, time\n"+
		"held = b'\\x01' * (1024 << 20)\n"+ // 1 GiB, written, so resident
		"sys.stdout.write('ready\\n'); sys.stdout.flush()\n"+
		"time.sleep(120)\n")
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	pid := helper.Process.Pid
	t.Cleanup(func() {
		// The helper this test started and recorded, and nothing else.
		_ = helper.Process.Kill()
		_ = helper.Wait()
	})
	ready := make(chan error, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		if readErr == nil && line != "ready\n" {
			readErr = errors.New("unexpected helper output " + strconv.Quote(line))
		}
		ready <- readErr
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("the large-resident helper never became ready: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the large-resident helper never became ready")
	}
	process, err := proctree.Lookup(pid)
	if err != nil {
		t.Fatal(err)
	}

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// The precondition this test is about: the sweep starts INSIDE the exit
	// window, with the process still present and already argv-less. Without
	// this the test could pass by racing a process that was already gone.
	deadline := time.Now().Add(5 * time.Second)
	for !(proctree.Argv(pid) == nil && proctree.AliveSame(process)) {
		if time.Now().After(deadline) || !proctree.AliveSame(process) {
			t.Fatalf("fixture: the helper left /proc before its exit window could be observed; " +
				"it needs more resident memory to model a real exit here")
		}
		time.Sleep(100 * time.Microsecond)
	}

	if err := StopHookScopes(HookScopeUnitPrefix("s7239")); err != nil {
		t.Fatalf("StopHookScopes refused while a process of ours was merely exiting: %v. "+
			"Every archive runs this sweep while the session's own processes are exiting, "+
			"so this fails archive after its panes are already gone (#7239 review R1)", err)
	}
}

// R2. RunningHookPrefixes is a fail-OPEN reader: on an error its callers report
// no hooks in flight. So an unknown from the launcher oracle must degrade to the
// units-only answer, master's answer, and never erase a scope the manager
// still lists. Otherwise one exiting process on one poll ends an adopted hook's
// watch for good while its scope is still registered.
func TestRunningHookPrefixesKeepsUnitEvidenceWhenALauncherIsUnclassifiable(t *testing.T) {
	installSystemctlShim(t, "printf '%s\\n' 'af-hook-s1-g0-0.scope loaded active running Hook'\nexit 0\n")
	startArgvlessProcess(t)

	live, err := RunningHookPrefixes("af-hook-s1", "af-hook-s2")
	if err != nil {
		t.Fatalf("RunningHookPrefixes failed (%v) on an unclassifiable process; its callers fail open, "+
			"so this reports a still-registered hook as finished (#7239 review R2)", err)
	}
	if len(live) != 1 || live[0] != "af-hook-s1" {
		t.Fatalf("live prefixes = %v, want the registered af-hook-s1", live)
	}
}

// #7242 review R2-1, end to end on the real process table. A live launcher held
// before registration is the #3667 window. Beside it sits an unrelated process
// of ours that cannot be classified. The oracle must report the launcher it
// found AND the unknown. Then RunningHookPrefixes, whose four fail-closed
// callers act only on err == nil && len(live) == 0, must never answer "none":
// that answer resumes hooks and records the claimant as failed while its
// launcher is about to run.
func TestAnUnknownBesideAFoundLauncherNeverReadsAsAbsent(t *testing.T) {
	installSystemctlShim(t, "exit 0\n") // the manager lists nothing: the #3667 window
	prefix := HookScopeUnitPrefix("s7242")
	launcher := startStubHookLauncher(t, "systemd-run", HookScopeUnit(prefix, "g0", 0),
		testguard.BoundedSpin(time.Second, 5*time.Minute))
	startArgvlessProcess(t)

	launchers, err := RunningHookLaunchers(prefix)
	var unreadable *proctree.ArgvUnreadableError
	if !errors.As(err, &unreadable) {
		t.Fatalf("err = %v; the argv-less process must be reported as unknown", err)
	}
	found := false
	for _, l := range launchers {
		found = found || l.PID == launcher
	}
	if !found {
		// Errorf, not Fatalf: the assertion below is the caller-facing half, and
		// it must be seen on its own.
		t.Errorf("launchers = %v; the live launcher pid %d was discarded by an unknown about another process", launchers, launcher)
	}

	live, err := RunningHookPrefixes(prefix)
	if err == nil && len(live) == 0 {
		t.Fatal("RunningHookPrefixes answered \"no hook in flight\" while a launcher of that prefix was alive; " +
			"its fail-closed callers would resume the hooks and record the claimant as failed (#7242 review R2-1)")
	}
}

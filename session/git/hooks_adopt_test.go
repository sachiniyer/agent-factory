//go:build linux

package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
)

// fastHookAdoptionPoll collapses the poll clock so a test can observe the
// "hooks finished" edge without waiting out the production interval.
func fastHookAdoptionPoll(t *testing.T) {
	t.Helper()
	previous := hookAdoptionPollInterval
	hookAdoptionPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { hookAdoptionPollInterval = previous })
}

// installHookLaunchRecorder records any systemd-run spawn. It is prepended to
// PATH AFTER installSurvivorSystemctl has prepended its own directory, so
// systemd-run resolves here while systemctl still resolves to the scripted
// manager: the two shims answer for different programs and must not collide.
func installHookLaunchRecorder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "systemd-run.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "systemd-run"), []byte(script), 0o700); err != nil {
		t.Fatalf("write systemd-run recorder: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func waitForClosed(t *testing.T, done <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("%s within %s", what, within)
	}
}

func requireOpen(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	if done == nil {
		t.Fatalf("%s: no hook run is reported in flight at all", what)
	}
	select {
	case <-done:
		t.Fatalf("%s", what)
	default:
	}
}

// The gap #3682 closes. A hook run that outlived the daemon generation that
// started it is still going over an intact tree, and the restored session has to
// say so through the SAME channel a first run uses — the readiness budget and
// the task-lifecycle teardown both read that channel and neither should learn a
// second way to ask.
func TestAdoptionReportsASurvivingHookRunAsInFlight(t *testing.T) {
	installSurvivorSystemctl(t, `case "$*" in
  *list-units*) printf '%s\n' 'af-hook-sess3682-g0-0.scope loaded active running Hook' ;;
esac
exit 0
`)
	claimDaemonProcess(t)
	gw := worktreeWithRecordedScope(t, "af-hook-sess3682")

	AdoptRunningHooks([]*GitWorktree{gw})

	requireOpen(t, gw.HooksDone(),
		"a surviving hook scope is active, but the restored session reports nothing in flight (#3682)")
}

// The other half of reporting: the state must clear itself. A channel that
// stayed open after the survivor exited would hold the readiness budget and a
// task's on_complete teardown against a session with nothing running — worse
// than the silence being fixed.
func TestAdoptedRunReportsFinishedOnceItsScopeIsGone(t *testing.T) {
	fastHookAdoptionPoll(t)
	finished := filepath.Join(t.TempDir(), "finished")
	installSurvivorSystemctl(t, `case "$*" in
  *list-units*)
    if [ ! -f '`+finished+`' ]; then
      printf '%s\n' 'af-hook-sess3682-g0-0.scope loaded active running Hook'
    fi ;;
esac
exit 0
`)
	claimDaemonProcess(t)
	gw := worktreeWithRecordedScope(t, "af-hook-sess3682")

	AdoptRunningHooks([]*GitWorktree{gw})
	done := gw.HooksDone()
	// Dwell for many poll intervals BEFORE releasing the survivor. Without this
	// the assertion would also pass for a watcher that closed on its first tick
	// whatever the manager said — the claim is that the answer is read, not that
	// the channel eventually closes.
	time.Sleep(50 * hookAdoptionPollInterval)
	requireOpen(t, done, "the run was reported finished while its scope is still active")

	if err := os.WriteFile(finished, nil, 0o600); err != nil {
		t.Fatalf("retire the survivor: %v", err)
	}
	waitForClosed(t, done, 10*time.Second, "the adopted run never reported finishing after its scope exited")
}

// The refinement the maintainer attached to #3650's design, re-asserted at the
// place that could most easily break it: a survivor over an INTACT tree is left
// to finish. Adoption takes over the reporting, never the run — it must not
// execute the operator's provisioning commands a second time over the same path
// (#2770), and it must not spawn anything at all.
func TestAdoptionNeverRunsTheHookOverTheIntactTree(t *testing.T) {
	installSurvivorSystemctl(t, `case "$*" in
  *list-units*) printf '%s\n' 'af-hook-sess3682-g0-0.scope loaded active running Hook' ;;
esac
exit 0
`)
	launchLog := installHookLaunchRecorder(t)
	claimDaemonProcess(t)
	reran := filepath.Join(t.TempDir(), "hook-ran")
	repoPath := freshRepoConfig(t, []string{fmt.Sprintf("printf ran > %q", reran)})

	gw := worktreeWithRecordedScope(t, "af-hook-sess3682")
	gw.repoPath = repoPath
	AdoptRunningHooks([]*GitWorktree{gw})

	requireOpen(t, gw.HooksDone(), "the survivor was not adopted, so this test proves nothing about re-running")
	// Give a hypothetical second run time to land its side effect.
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(reran); err == nil {
		t.Fatal("adoption ran post_worktree_commands again over the intact tree (#2770)")
	}
	if raw, err := os.ReadFile(launchLog); err == nil && len(raw) > 0 {
		t.Fatalf("adoption spawned systemd-run: %s", raw)
	}
}

// Being the daemon is the whole gate, exactly as it is for creating a scope. A
// TUI or CLI never put a hook in a scope, so it has none to find — and it has no
// guaranteed user manager either, so consulting one would be both pointless and
// a new failure mode on a path that has none today.
func TestAdoptionIsSkippedWhenThisProcessIsNotTheDaemon(t *testing.T) {
	logPath := installSurvivorSystemctl(t, `case "$*" in
  *list-units*) printf '%s\n' 'af-hook-sess3682-g0-0.scope loaded active running Hook' ;;
esac
exit 0
`)
	t.Setenv("AGENT_FACTORY_SYSTEMD_UNIT", "")
	t.Setenv("SYSTEMD_EXEC_PID", "")
	if systemdunit.RunningDaemonProcess() {
		// The cgroup check is authoritative for units installed before the marker
		// existed, so on a box where the test binary really is inside the daemon
		// unit the non-daemon path is not observable at all.
		t.Skip("this process really is the daemon; the non-daemon path is not observable here")
	}
	gw := worktreeWithRecordedScope(t, "af-hook-sess3682")

	AdoptRunningHooks([]*GitWorktree{gw})

	if gw.HooksDone() != nil {
		t.Fatal("a non-daemon process adopted a hook scope it could never have created")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		raw, _ := os.ReadFile(logPath)
		t.Fatalf("a non-daemon process consulted the systemd user manager on restore:\n%s", raw)
	}
}

// Adoption fails OPEN, which is the opposite of the sweep on the rebuild path
// and deliberately so. This read decides only what a session SAYS. Guessing
// "running" from an unreadable manager would defer the readiness budget and hold
// every task teardown on that box, and it would never resolve because the same
// error repeats; guessing "not running" costs exactly the visibility that did
// not exist before. The safety property is not on this path —
// TestRebuildPathRefusesWhenTheUserManagerCannotBeQueried still owns it.
func TestAdoptionFailsOpenWhenTheManagerCannotBeQueried(t *testing.T) {
	installSurvivorSystemctl(t, "echo 'Failed to connect to bus: No such file or directory' >&2\nexit 1\n")
	claimDaemonProcess(t)
	gw := worktreeWithRecordedScope(t, "af-hook-sess3682")

	AdoptRunningHooks([]*GitWorktree{gw})

	if gw.HooksDone() != nil {
		t.Fatal("an unreadable manager was turned into a permanent 'hooks running' report; nothing could ever clear it")
	}
}

// The #3667 oracle, on the restore path. A daemon that died between
// systemd-run's execve and its StartTransientUnit reply left a launcher alive
// with no unit at all, so the manager's silence is not evidence: a hook is still
// coming, and the restored session must report it rather than read the empty
// unit list as "finished".
func TestAdoptionFindsALauncherThatHasNotRegisteredItsScope(t *testing.T) {
	fastHookAdoptionPoll(t)
	installSurvivorSystemctl(t, "exit 0\n") // the manager lists nothing: this IS the window
	claimDaemonProcess(t)
	const prefix = "af-hook-sess3682launch"
	gate := filepath.Join(t.TempDir(), "release")
	startStubHookLauncher(t, prefix+"-g0-0.scope", fmt.Sprintf("while [ ! -f %q ]; do sleep 1; done", gate))

	gw := worktreeWithRecordedScope(t, prefix)
	AdoptRunningHooks([]*GitWorktree{gw})

	done := gw.HooksDone()
	requireOpen(t, done,
		"a live systemd-run for this session's prefix was read as no hook at all; the manager's silence is not evidence (#3667)")
	// The poll has to keep reading the launcher oracle too, not just the initial
	// probe: a watcher that consulted only the manager would report this run
	// finished on its very first tick while the launcher was still on its way in.
	time.Sleep(50 * hookAdoptionPollInterval)
	requireOpen(t, done, "the adopted run was reported finished while its launcher was still live and unregistered")

	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatalf("release the stub launcher: %v", err)
	}
	waitForClosed(t, done, 60*time.Second, "the adopted run never reported finishing after its launcher exited")
}

// The #2770 ordering is unchanged by adoption, and this is where that has to be
// proven rather than assumed: the rebuild path cancels first, and for an adopted
// run the cancel can only stop the WATCHER — a survivor's pgid and cmd.Wait died
// with the daemon that started it. What actually makes the tree provably
// untouched is the sweep that follows, which stops the scope through the manager
// and proves it gone.
func TestARebuildOverAnAdoptedSurvivorStillStopsItFirst(t *testing.T) {
	stopped := filepath.Join(t.TempDir(), "stopped")
	logPath := installSurvivorSystemctl(t, `case "$*" in
  *" stop "*) : > '`+stopped+`'; exit 0 ;;
esac
if [ -f '`+stopped+`' ]; then exit 0; fi
printf '%s\n' 'af-hook-sess3682-g0-0.scope loaded active running Hook'
exit 0
`)
	claimDaemonProcess(t)
	gw := worktreeWithRecordedScope(t, "af-hook-sess3682")
	AdoptRunningHooks([]*GitWorktree{gw})
	requireOpen(t, gw.HooksDone(), "the survivor was not adopted, so this proves nothing about the rebuild path")

	result := make(chan error, 1)
	go func() { result <- gw.cancelAndWaitHooks() }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("cancelAndWaitHooks: %v", err)
		}
	case <-time.After(10 * time.Second):
		// hookStopTimeout is 30s, so blocking here means the adopted watcher did
		// not release on cancellation and the rebuild would have failed closed on
		// a session whose survivor was perfectly stoppable.
		t.Fatal("the rebuild path blocked on an adopted hook run that cancellation should have released")
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("systemctl was never consulted: %v", err)
	}
	if !strings.Contains(string(raw), "stop -- af-hook-sess3682-g0-0.scope") {
		t.Fatalf("the adopted survivor was never stopped before the tree was touched:\n%s", raw)
	}
}

// And the fail-closed half survives adoption. A survivor that cannot be stopped
// must still refuse the rebuild — the closed hooksDone an adopted run produces
// on cancellation says only "the watcher stopped", never "the process is gone".
func TestARebuildOverAnAdoptedSurvivorStillRefusesWhenItWillNotStop(t *testing.T) {
	installSurvivorSystemctl(t, `case "$*" in
  *" stop "*) exit 0 ;;
esac
printf '%s\n' 'af-hook-sess3682-g0-0.scope loaded active running Hook'
exit 0
`)
	claimDaemonProcess(t)
	gw := worktreeWithRecordedScope(t, "af-hook-sess3682")
	AdoptRunningHooks([]*GitWorktree{gw})
	requireOpen(t, gw.HooksDone(), "the survivor was not adopted, so this proves nothing about the refusal")

	err := gw.cancelAndWaitHooks()
	if err == nil {
		t.Fatal("an adopted survivor that outlived its stop was treated as gone")
	}
	if !strings.Contains(err.Error(), "refusing to modify the worktree") {
		t.Fatalf("unexpected refusal message: %v", err)
	}
}

// A worktree with a live in-process run already reports itself. Overwriting its
// channel would strand the join cancelAndWaitHooks performs — the one thing that
// makes a rebuilt tree provably untouched by a run this daemon CAN join.
func TestAdoptionLeavesALiveInProcessRunAlone(t *testing.T) {
	installSurvivorSystemctl(t, `case "$*" in
  *list-units*) printf '%s\n' 'af-hook-sess3682-g0-0.scope loaded active running Hook' ;;
esac
exit 0
`)
	claimDaemonProcess(t)
	gw := worktreeWithRecordedScope(t, "af-hook-sess3682")
	inProcess := make(chan struct{})
	gw.hooksDone = inProcess

	AdoptRunningHooks([]*GitWorktree{gw})

	if got := gw.HooksDone(); got != (<-chan struct{})(inProcess) {
		t.Fatal("adoption replaced a live in-process hook run's own completion channel")
	}
}

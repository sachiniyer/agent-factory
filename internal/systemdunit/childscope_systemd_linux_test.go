//go:build linux

package systemdunit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// requireRealUserManager gates the real-systemd tests below on a reachable
// systemd user manager. Unlike integration's #2284 lifecycle test they need no
// ephemeral runner and never touch agent-factory-daemon.service: every unit they
// create carries this process's pid in its name and is removed on the way out.
func requireRealUserManager(t *testing.T) {
	t.Helper()
	if os.Getenv("AF_SYSTEMD_LIFECYCLE_TEST") != "1" {
		t.Skip("real systemd scope test requires AF_SYSTEMD_LIFECYCLE_TEST=1 and a systemd user manager")
	}
	for _, tool := range []string{"systemctl", "systemd-run"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required when AF_SYSTEMD_LIFECYCLE_TEST=1: %v", tool, err)
		}
	}
	if out, err := exec.Command("systemctl", "--user", "show-environment").CombinedOutput(); err != nil {
		t.Fatalf("no reachable systemd user manager: %v\n%s", err, out)
	}
}

func stopUnit(unit string) {
	_ = exec.Command("systemctl", "--user", "stop", unit).Run()
	_ = exec.Command("systemctl", "--user", "reset-failed", unit).Run()
}

func unitActiveState(t *testing.T, unit string) string {
	t.Helper()
	out, _ := exec.Command("systemctl", "--user", "show", "-p", "ActiveState", "--value", unit).Output()
	return strings.TrimSpace(string(out))
}

func waitForUnitState(t *testing.T, unit, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		if last = unitActiveState(t, unit); last == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("unit %s did not reach ActiveState=%s within %s (last %q)", unit, want, timeout, last)
}

// startSleeper runs cmd in the background, waits for it to publish its pid, and
// returns that pid together with a channel closed when the child is REAPED.
//
// The channel is the oracle, not the pid. kill(pid, 0) succeeds on a zombie, so
// a child that systemd has already killed still reads as alive until its parent
// waits for it — which would let the discriminating assertion below pass while
// the bound scope was in fact torn down, i.e. fail open on exactly the property
// under test. Measured: the first draft of this test did precisely that.
func startSleeper(t *testing.T, cmd *exec.Cmd, pidFile string) (int, <-chan struct{}) {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", cmd.Args, err)
	}
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-reaped
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
				return pid, reaped
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child never published its pid to %s", pidFile)
	return 0, reaped
}

// TestBoundScopeDiesWithItsOwnerWhileUnboundScopeSurvives is #3650's second red,
// reduced to the property that discriminates the two shapes. It is the reason
// the obvious fix was rejected before any code was written: routing
// post_worktree_commands through the daemon-BOUND shape would kill an operator's
// in-flight `make dev_install` on every daemon restart and every #2212
// auto-upgrade, silently, on a timer they do not control.
//
// Both halves run against the same throwaway owner unit, so the only difference
// between them is the scope shape itself.
func TestBoundScopeDiesWithItsOwnerWhileUnboundScopeSurvives(t *testing.T) {
	requireRealUserManager(t)
	claimDaemon(t)

	owner := fmt.Sprintf("af3650-owner-%d.service", os.Getpid())
	t.Cleanup(func() { stopUnit(owner) })
	out, err := exec.Command("systemd-run", "--user", "--quiet", "--collect",
		"--unit="+owner, "--property=KillMode=process", "--", "sleep", "600").CombinedOutput()
	if err != nil {
		t.Fatalf("start owner unit: %v\n%s", err, out)
	}
	waitForUnitState(t, owner, "active", 15*time.Second)

	dir := t.TempDir()
	boundPid := filepath.Join(dir, "bound.pid")
	unboundPid := filepath.Join(dir, "unbound.pid")
	unboundUnit := fmt.Sprintf("af3650-unbound-%d.scope", os.Getpid())
	t.Cleanup(func() { stopUnit(unboundUnit) })

	script := func(path string) string {
		return fmt.Sprintf("printf '%%s' \"$$\" > %q; sleep 120", path)
	}
	boundPID, boundReaped := startSleeper(t,
		newBoundChildCommandForUnit(owner, "sh", "-c", script(boundPid)), boundPid)
	unboundPID, unboundReaped := startSleeper(t,
		NewUnboundScopeCommand(unboundUnit, "sh", "-c", script(unboundPid)), unboundPid)

	stopUnit(owner)
	waitForUnitState(t, owner, "inactive", 15*time.Second)

	select {
	case <-boundReaped:
	case <-time.After(15 * time.Second):
		t.Errorf("the BOUND shape (pid %d) did not stop with its owner; its BindsTo= is what makes it right for a watcher and wrong for a hook — if this no longer holds, the two shapes have collapsed", boundPID)
	}
	select {
	case <-unboundReaped:
		t.Fatalf("the UNBOUND hook scope (pid %d) died when the daemon unit stopped: an operator's in-flight post_worktree_commands would be killed by every daemon restart and every auto-upgrade (#3650)", unboundPID)
	case <-time.After(3 * time.Second):
	}
}

// TestUnboundScopeIsASiblingOfTheDaemonUnit is #3650's first red at the level of
// the scope itself: the process must leave the caller's control group, which is
// the daemon's in production. Asserted on /proc/<pid>/cgroup, never on ancestry
// — the hook stays a child of the daemon either way.
func TestUnboundScopeIsASiblingOfTheDaemonUnit(t *testing.T) {
	requireRealUserManager(t)
	claimDaemon(t)

	unit := fmt.Sprintf("af3650-cgroup-%d.scope", os.Getpid())
	t.Cleanup(func() { stopUnit(unit) })
	cgroupFile := filepath.Join(t.TempDir(), "child.cgroup")
	cmd := NewUnboundScopeCommand(unit, "sh", "-c",
		fmt.Sprintf("cat /proc/self/cgroup > %q", cgroupFile))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scoped child failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(cgroupFile)
	if err != nil {
		t.Fatalf("scoped child recorded no cgroup: %v", err)
	}
	child := strings.TrimSpace(string(raw))
	self, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatalf("read own cgroup: %v", err)
	}
	if child == strings.TrimSpace(string(self)) {
		t.Fatalf("scoped child stayed in the spawning process's cgroup: %s", child)
	}
	if !strings.Contains(child, unit) {
		t.Fatalf("scoped child is not in its named scope %s: %s", unit, child)
	}
}

// TestStopHookScopesKillsATermIgnoringSurvivorWithinTheBound is why the unbound
// shape carries a TimeoutStopSec at all. The survivor sweep in session/git fails
// CLOSED, so a stop that does not complete becomes af refusing to rebuild the
// worktree — and systemd's default here is 90s, measured on a real user manager.
// A hook that traps SIGTERM (a Makefile wrapper, a build tool with its own
// handler) is exactly the case that reaches it.
//
// This also exercises RunningHookScopes and StopHookScopes against a real
// manager rather than a scripted shim: the shim tests pin the parsing, this pins
// that the parsing matches what systemctl actually prints.
func TestStopHookScopesKillsATermIgnoringSurvivorWithinTheBound(t *testing.T) {
	requireRealUserManager(t)
	claimDaemon(t)

	prefix := HookScopeUnitPrefix(fmt.Sprintf("termignore-%d", os.Getpid()))
	unit := HookScopeUnit(prefix, "g0", 0)
	t.Cleanup(func() { stopUnit(unit) })
	pidFile := filepath.Join(t.TempDir(), "hook.pid")
	_, reaped := startSleeper(t, NewUnboundScopeCommand(unit, "sh", "-c",
		fmt.Sprintf("trap '' TERM; printf '%%s' \"$$\" > %q; while :; do sleep 5; done", pidFile)), pidFile)

	units, err := RunningHookScopes(prefix)
	if err != nil {
		t.Fatalf("RunningHookScopes: %v", err)
	}
	if len(units) != 1 || units[0] != unit {
		t.Fatalf("RunningHookScopes = %v, want [%s] — the sweep cannot stop what it cannot see", units, unit)
	}

	start := time.Now()
	if err := StopHookScopes(prefix); err != nil {
		t.Fatalf("a TERM-ignoring hook survivor was not stopped: %v", err)
	}
	elapsed := time.Since(start)

	select {
	case <-reaped:
	case <-time.After(10 * time.Second):
		t.Fatal("StopHookScopes reported success while the hook was still running")
	}
	// Generous against a loaded runner, but far below the 90s default this
	// property exists to replace — at the default, StopHookScopes' own context
	// would have expired first and reported a failure instead.
	if elapsed > 45*time.Second {
		t.Fatalf("stopping a TERM-ignoring survivor took %s; the bounded TimeoutStopSec is not in effect", elapsed)
	}
	remaining, err := RunningHookScopes(prefix)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("scopes still listed after the stop: %v (err %v)", remaining, err)
	}
}

// The two oracles have to HAND OFF, and only a real systemd-run can prove it.
//
// RunningHookLaunchers keys on argv, and `systemd-run --scope` EXECs: the moment
// the scope exists, that argv is gone and the process is the hook shell instead.
// The stubbed tests cannot see that transition — they hold a process that never
// execs — so this is where the assumption underneath the whole design is
// checked. If argv survived the exec, every sweep for a session with a RUNNING
// hook would find a "launcher" that can never register, wait out the settle
// budget and then refuse: a rebuild wedged by the fix for #3667 rather than by
// the defect.
func TestTheLauncherOracleReleasesAScopeOnceItIsRegistered(t *testing.T) {
	requireRealUserManager(t)
	claimDaemon(t)

	prefix := HookScopeUnitPrefix(fmt.Sprintf("handoff-%d", os.Getpid()))
	unit := HookScopeUnit(prefix, "g0", 0)
	t.Cleanup(func() { stopUnit(unit) })
	pidFile := filepath.Join(t.TempDir(), "hook.pid")
	_, reaped := startSleeper(t, NewUnboundScopeCommand(unit, "sh", "-c",
		fmt.Sprintf("printf '%%s' \"$$\" > %q; sleep 120", pidFile)), pidFile)
	waitForUnitState(t, unit, "active", 15*time.Second)

	launchers, err := RunningHookLaunchers(prefix)
	if err != nil {
		t.Fatalf("RunningHookLaunchers: %v", err)
	}
	if len(launchers) != 0 {
		t.Fatalf("a hook that already HAS its scope was still reported as an unregistered launcher (%+v); "+
			"every sweep for a session with a running hook would wait out the settle budget and then refuse", launchers)
	}
	units, err := RunningHookScopes(prefix)
	if err != nil || len(units) != 1 || units[0] != unit {
		t.Fatalf("the scope oracle did not pick the hook up after the launcher released it: %v (err %v)", units, err)
	}

	// And the sweep completes on the scope's own terms rather than on the
	// launcher timeout, which is the observable difference between the two.
	start := time.Now()
	if err := StopHookScopes(prefix); err != nil {
		t.Fatalf("StopHookScopes: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= hookLauncherSettleTimeout {
		t.Fatalf("the sweep spent %s, at or past the launcher settle budget of %s: it was waiting for a launcher that had already handed off",
			elapsed, hookLauncherSettleTimeout)
	}
	select {
	case <-reaped:
	case <-time.After(10 * time.Second):
		t.Fatal("StopHookScopes reported success while the hook was still running")
	}
}

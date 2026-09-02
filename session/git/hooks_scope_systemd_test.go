//go:build linux

package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
)

const systemdHookScopeTestEnv = "AF_SYSTEMD_LIFECYCLE_TEST"

// requireSystemdUserManager gates the two real-cgroup tests below. They need a
// reachable systemd user manager, which the ordinary suite has nowhere: the
// container harness ships no user manager at all, and the dev box's manager is
// the maintainer's own. They run on the prepared ephemeral runner that already
// hosts integration's #2284 lifecycle test.
func requireSystemdUserManager(t *testing.T) {
	t.Helper()
	if os.Getenv(systemdHookScopeTestEnv) != "1" || os.Getenv("CI") != "true" {
		t.Skip("real systemd hook-scope test requires an explicitly prepared ephemeral CI runner")
	}
	for _, tool := range []string{"systemctl", "systemd-run"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required on the opted-in runner: %v", tool, err)
		}
	}
	if out, err := exec.Command("systemctl", "--user", "show-environment").CombinedOutput(); err != nil {
		t.Fatalf("the opted-in runner has no reachable systemd user manager: %v\n%s", err, out)
	}
}

// claimDaemonMarker makes RunningDaemonProcess() answer true for this process.
// Deliberately a local copy rather than a shared helper: this file is the pair
// of reds, and it must compile and fail on a tree that has none of the fix.
func claimDaemonMarker(t *testing.T) {
	t.Helper()
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
}

func selfCgroup(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatalf("read /proc/self/cgroup: %v", err)
	}
	return strings.TrimSpace(string(data))
}

// TestDaemonSpawnedPostWorktreeHookLeavesTheDaemonCgroup is #3650's first red.
//
// The oracle is /proc/<pid>/cgroup of the hook process itself, NOT its ancestry:
// ancestry is identical before and after the fix — the hook is a child of the
// daemon either way — while cgroup membership is the entire bug. A hook that
// shares the spawning process's cgroup is charged to the daemon unit, which is
// what made MemoryPeak read 17-38 GB (#3625) and what makes any MemoryMax= an
// operator sets on the daemon land on the operator's build instead.
//
// The spawning process stands in for the daemon: it is the process the hook
// runner inherits a cgroup from, and RunningDaemonProcess() is made true for it
// below, which is the only thing production consults.
func TestDaemonSpawnedPostWorktreeHookLeavesTheDaemonCgroup(t *testing.T) {
	requireSystemdUserManager(t)
	claimDaemonMarker(t)

	cgroupFile := filepath.Join(t.TempDir(), "hook.cgroup")
	repoPath := freshRepoConfig(t, []string{fmt.Sprintf("cat /proc/self/cgroup > %q", cgroupFile)})
	spawner := selfCgroup(t)

	done := RunPostWorktreeHooksAsyncWithEnvironment(context.Background(), repoPath, t.TempDir(), nil)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("post-worktree hook did not finish")
	}

	raw, err := os.ReadFile(cgroupFile)
	if err != nil {
		t.Fatalf("hook did not record its own cgroup: %v", err)
	}
	hook := strings.TrimSpace(string(raw))
	if hook == "" {
		t.Fatal("hook recorded an empty cgroup; the assertion below would be vacuous")
	}
	if hook == spawner {
		t.Fatalf("daemon-spawned post-worktree hook runs in the spawning daemon's own cgroup (#3650)\n  hook:    %s\n  spawner: %s", hook, spawner)
	}
	if !strings.Contains(hook, "af-hook-") {
		t.Fatalf("hook left the daemon cgroup but not into a named af-hook scope, so no durable handle exists for it: %s", hook)
	}
}

// TestDaemonSpawnedPostWorktreeHookSurvivesTheDaemonUnitStopping is #3650's
// second red, and it is the regression test for the question that stopped the
// obvious fix: routing hooks through the daemon-BOUND scope shape would kill a
// user's in-flight `make dev_install` on every daemon restart and every #2212
// auto-upgrade. That shape's BindsTo= is exactly what this asserts is absent.
//
// It passes on master (a raw child of a KillMode=process unit survives) and it
// passes with the unbound scope. It FAILS against the naive route, which is the
// whole reason it exists.
func TestDaemonSpawnedPostWorktreeHookSurvivesTheDaemonUnitStopping(t *testing.T) {
	requireSystemdUserManager(t)

	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("resolve user config dir: %v", err)
	}
	unitPath := filepath.Join(configDir, "systemd", "user", "agent-factory-daemon.service")
	if _, statErr := os.Stat(unitPath); !os.IsNotExist(statErr) {
		t.Fatalf("refusing to touch a pre-existing %s on the runner (stat err=%v)", unitPath, statErr)
	}
	// A transient stand-in for the daemon unit. The bound shape names this unit
	// in BindsTo=/After=, so stopping it is what a restart or an auto-upgrade
	// looks like to a scope; the unbound shape has no edge to it at all.
	startStandInDaemonUnit(t)
	claimDaemonMarker(t)

	pidFile := filepath.Join(t.TempDir(), "hook.pid")
	repoPath := freshRepoConfig(t, []string{
		fmt.Sprintf("printf '%%s' \"$$\" > %q; sleep 120", pidFile),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := RunPostWorktreeHooksAsyncWithEnvironment(ctx, repoPath, t.TempDir(), nil)

	pid := waitForPidFile(t, pidFile, 30*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	stopStandInDaemonUnit(t)
	// The runner's own channel is the oracle, not kill(pid, 0): a hook systemd
	// has already killed stays a ZOMBIE until the runner waits for it, and a
	// liveness probe reads a zombie as alive — which would pass this test in the
	// exact case it exists to catch. The runner closes done only after cmd.Wait
	// has reaped the shell, so done closing means the hook is genuinely gone.
	select {
	case <-done:
		t.Fatalf("the operator's in-flight post_worktree_commands (pid %d) was killed by the daemon unit stopping; a daemon restart or a #2212 auto-upgrade would take a running `make dev_install` with it (#3650)", pid)
	case <-time.After(6 * time.Second):
	}
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		t.Fatalf("the operator's in-flight post_worktree_commands (pid %d) did not survive the daemon unit stopping (#3650)", pid)
	}
}

func startStandInDaemonUnit(t *testing.T) {
	t.Helper()
	out, err := exec.Command("systemd-run", "--user", "--quiet", "--collect",
		"--unit=agent-factory-daemon.service", "--property=KillMode=process",
		"--", "sleep", "600").CombinedOutput()
	if err != nil {
		t.Fatalf("start stand-in daemon unit: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "--user", "stop", "agent-factory-daemon.service").Run()
		_ = exec.Command("systemctl", "--user", "reset-failed", "agent-factory-daemon.service").Run()
	})
	waitForUnitActiveState(t, "agent-factory-daemon.service", "active", 15*time.Second)
}

func stopStandInDaemonUnit(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("systemctl", "--user", "stop", "agent-factory-daemon.service").CombinedOutput(); err != nil {
		t.Fatalf("stop stand-in daemon unit: %v\n%s", err, out)
	}
	waitForUnitActiveState(t, "agent-factory-daemon.service", "inactive", 15*time.Second)
}

func waitForUnitActiveState(t *testing.T, unit, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		out, _ := exec.Command("systemctl", "--user", "show", "-p", "ActiveState", "--value", unit).Output()
		last = strings.TrimSpace(string(out))
		if last == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("unit %s did not reach ActiveState=%s within %s (last %q)", unit, want, timeout, last)
}

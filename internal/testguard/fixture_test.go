//go:build linux || darwin

package testguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// #4412: every fixture loop these builders emit must self-terminate — the
// ceiling, not the test's cleanup, is what a leaked process still has.

func TestBoundedSpinSelfTerminates(t *testing.T) {
	cmd := StartGroupProcess(t, exec.Command("sh", "-c", BoundedSpin(10*time.Millisecond, 50*time.Millisecond)))
	if err := cmd.Wait(); err != nil {
		t.Fatalf("bounded spin exited %v, want success", err)
	}
}

func TestBoundedGateWaitFindsAnExistingGate(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "release")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := StartGroupProcess(t, exec.Command("sh", "-c", BoundedGateWait(gate, 5*time.Millisecond, time.Minute)))
	if err := cmd.Wait(); err != nil {
		t.Fatalf("gate already present: exited %v, want success", err)
	}
}

func TestBoundedGateWaitSeesALateGate(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "release")
	cmd := StartGroupProcess(t, exec.Command("sh", "-c", BoundedGateWait(gate, 5*time.Millisecond, time.Minute)))
	time.Sleep(30 * time.Millisecond)
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("gate released mid-wait: exited %v, want success", err)
	}
}

func TestBoundedGateWaitTimesOut(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "never-written")
	cmd := StartGroupProcess(t, exec.Command("sh", "-c", BoundedGateWait(gate, 5*time.Millisecond, 30*time.Millisecond)))
	err := cmd.Wait()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 124 {
		t.Fatalf("missing gate: exited %v, want status 124", err)
	}
}

func TestBoundedLoopSelfTerminates(t *testing.T) {
	countFile := filepath.Join(t.TempDir(), "count")
	body := fmt.Sprintf("echo x >> %s; sleep 0.005", shellquoteForTest(countFile))
	cmd := StartGroupProcess(t, exec.Command("sh", "-c", BoundedLoop(5*time.Millisecond, 20*time.Millisecond, body)))
	if err := cmd.Wait(); err != nil {
		t.Fatalf("bounded loop exited %v, want success", err)
	}
	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("bounded loop body never ran")
	}
}

func TestStartGroupProcessMakesALeader(t *testing.T) {
	cmd := StartGroupProcess(t, exec.Command("sleep", "60"))
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	if pgid != cmd.Process.Pid {
		t.Fatalf("pgid %d != pid %d: fixture is not a process group leader", pgid, cmd.Process.Pid)
	}
}

// The group kill is the whole point (#4412): a fixture shell that
// backgrounds children must lose them all, not just its own pid.
func TestStartGroupProcessKillReachesGrandchildren(t *testing.T) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutR.Close()
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $!; sleep 60 & echo $!; wait")
	cmd.Stdout = stdoutW
	StartGroupProcess(t, cmd)
	stdoutW.Close()
	var grandchildA, grandchildB int
	if _, err := fmt.Fscanf(stdoutR, "%d\n%d", &grandchildA, &grandchildB); err != nil {
		t.Fatalf("read grandchild pids: %v", err)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill group: %v", err)
	}
	_ = cmd.Wait()
	for _, pid := range []int{grandchildA, grandchildB} {
		if dead := waitForProcessDeath(pid, 2*time.Second); !dead {
			t.Errorf("grandchild %d survived the process-group kill", pid)
		}
	}
}

// A re-exec'd fixture must die when its owner does (#4412): spawn a wrapper
// that starts the watchdog child, kill the wrapper, and the child — now
// reparented — must exit on its own.
func TestExitWhenOrphaned(t *testing.T) {
	if os.Getenv("AF_TEST_ORPHAN_WATCHDOG_CHILD") == "1" {
		ExitWhenOrphaned(5 * time.Millisecond)
		select {}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// $$ is the shell's pid, which `exec sleep 60` keeps — i.e. the child's
	// real parent — so the watchdog still fires when this process only boots
	// after the wrapper already died and a captured ppid would name a reaper.
	wrapper := exec.Command("sh", "-c", fmt.Sprintf(
		ExpectedParentEnv+"=$$ %q -test.run=^TestExitWhenOrphaned$ & echo $! > %q; exec sleep 60", self, pidFile))
	wrapper.Env = append(os.Environ(), "AF_TEST_ORPHAN_WATCHDOG_CHILD=1")
	StartGroupProcess(t, wrapper)

	childPID := -1
	for i := 0; i < 200; i++ {
		if data, err := os.ReadFile(pidFile); err == nil {
			if _, err := fmt.Sscanf(string(data), "%d", &childPID); err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatal("watchdog child never wrote its pid")
	}
	// Adopt the about-to-be-orphaned child where a subreaper exists, so its
	// zombie does not linger under a container init that never collects — the
	// death assertion below treats the zombie as dead either way, which is
	// why the leak needed its own mechanism (#4412).
	becomeOrphanReaper(t)
	if err := wrapper.Process.Kill(); err != nil {
		t.Fatalf("kill wrapper (child's parent): %v", err)
	}
	if !waitForProcessDeath(childPID, 2*time.Second) {
		t.Errorf("orphaned watchdog child %d was still alive 2s after its parent died", childPID)
	}
	reapOrphanedChild(t, childPID)
}

// A zombie owner must read as dead to the watchdog's existence arm (#4417
// review): kill(pid, 0) succeeds on a zombie, so a signal-based processAlive
// held an orphaned fixture alive over its owner's uncollected corpse forever.
// Hold a killed child unreaped — that is the zombie — and the answer must be
// not-alive.
func TestProcessAliveReportsZombieDead(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start zombie candidate: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill zombie candidate: %v", err)
	}
	// No Wait: the corpse stays held until the cleanup reaps it, so the
	// process is a zombie for the whole assertion window.
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(cmd.Process.Pid) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("unreaped zombie %d still reported alive by processAlive", cmd.Process.Pid)
}

// The group pin must die with the test process it watches, not with the
// test's cleanups: the crash/timeout case the pin exists for never runs them,
// and an unguarded sleeper survives a day per StartGroupProcess call (#4417
// review). Spawn the pin script under a killable stand-in for the test
// binary, kill it while it is still an UNREAPED ZOMBIE of this test — the
// case a kill-0 check would miss — and the pin must exit on its own.
func TestGroupPinExitsWithItsSpawner(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pin.pid")
	// `sh -c '...' &` makes the pin a REAL child of the wrapper — the same
	// spawner→pin shape StartGroupProcess creates — and $$ hands it the
	// wrapper's pid as its $1 owner, which `exec sleep 60` keeps as its pid
	// until the kill.
	wrapper := exec.Command("sh", "-c", fmt.Sprintf(
		"sh -c '%s' af-testguard-pin $$ & echo $! > %q; exec sleep 60", groupPinScript, pidFile))
	StartGroupProcess(t, wrapper)

	pinPID := -1
	for i := 0; i < 200; i++ {
		if data, err := os.ReadFile(pidFile); err == nil {
			if _, err := fmt.Sscanf(string(data), "%d", &pinPID); err == nil && pinPID > 0 {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pinPID <= 0 {
		t.Fatal("group pin never wrote its pid")
	}
	becomeOrphanReaper(t)
	if err := wrapper.Process.Kill(); err != nil {
		t.Fatalf("kill pin's spawner: %v", err)
	}
	if !waitForProcessDeath(pinPID, 3*time.Second) {
		t.Errorf("group pin %d survived its spawner's death — the abandoned-pin leak is back", pinPID)
	}
	reapOrphanedChild(t, pinPID)
}

// The unpinned variant exists so a test can observe its fixture's group
// EMPTY after a mid-test group signal (#4417 review): kill(-pgid, 0) counts
// a held pin zombie, so the pinned variant reports the group alive forever.
// Kill the single-member group, reap the leader, and the group probe must
// go ESRCH — while the cleanup's direct-child kill stays a safe no-op past
// the reap instead of risking a recycled pgid.
func TestStartGroupProcessUnpinnedLeavesObservableEmptyGroup(t *testing.T) {
	cmd := StartGroupProcessUnpinned(t, exec.Command("sh", "-c", "exec sleep 60"))
	pgid := cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill fixture group: %v", err)
	}
	_, _ = cmd.Process.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("process group %d still visible after its only member was killed and reaped", pgid)
}

// waitForProcessDeath asks proctree, not kill(pid, 0): signal-0 answers for a
// ZOMBIE, and a watchdog-killed orphan stays one until whatever it reparented
// to collects it — on that reaper's schedule, which a container init may never
// keep. Lookup reports zombies and gone processes alike as dead (#4412).
func waitForProcessDeath(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := proctree.Lookup(pid); err != nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func shellquoteForTest(s string) string {
	// Tests use plain temp paths, but quote anyway so the emitted script is
	// robust if a temp dir ever contains a space.
	q := "'"
	for _, c := range s {
		if c == '\'' {
			q += `'\''`
		} else {
			q += string(c)
		}
	}
	return q + "'"
}

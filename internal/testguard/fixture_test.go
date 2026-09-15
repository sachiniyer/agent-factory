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
	wrapper := exec.Command("sh", "-c", fmt.Sprintf(
		"%q -test.run=^TestExitWhenOrphaned$ & echo $! > %q; exec sleep 60", self, pidFile))
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
	if err := wrapper.Process.Kill(); err != nil {
		t.Fatalf("kill wrapper (child's parent): %v", err)
	}
	if !waitForProcessDeath(childPID, 2*time.Second) {
		t.Errorf("orphaned watchdog child %d was still alive 2s after its parent died", childPID)
	}
}

func waitForProcessDeath(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
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

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

func TestMain(m *testing.M) {
	log.Initialize(false)
	code := m.Run()
	os.Exit(code)
}

// processAlive returns true if sending signal 0 to pid succeeds, meaning the process is still
// running and reachable.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// TestStopDaemon_DoesNotKillUnrelatedPID verifies that StopDaemon refuses to kill a process whose
// command line does not match an agent-factory daemon. Regression test for issue #264.
func TestStopDaemon_DoesNotKillUnrelatedPID(t *testing.T) {
	// Redirect config dir to a scratch location so we don't touch the user's real daemon.pid.
	tmpHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)

	pidFile := filepath.Join(tmpHome, "daemon.pid")

	// Spawn a long-running process that is NOT an agent-factory daemon.
	sleepCmd := exec.Command("sleep", "60")
	if err := sleepCmd.Start(); err != nil {
		t.Fatalf("failed to start sleep process: %v", err)
	}
	victimPID := sleepCmd.Process.Pid
	defer func() {
		// Best-effort cleanup regardless of test outcome.
		_ = sleepCmd.Process.Kill()
		_, _ = sleepCmd.Process.Wait()
	}()

	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", victimPID)), 0644); err != nil {
		t.Fatalf("failed to write PID file: %v", err)
	}

	if err := StopDaemon(); err != nil {
		t.Fatalf("StopDaemon returned error: %v", err)
	}

	// Give the process a brief moment; if StopDaemon killed it (the bug), it will have exited.
	time.Sleep(100 * time.Millisecond)

	if !processAlive(victimPID) {
		t.Fatalf("StopDaemon killed an unrelated process (PID %d); the vulnerability is still present", victimPID)
	}

	// PID file should have been cleaned up as stale.
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("expected stale PID file to be removed, stat err = %v", err)
	}
}

// TestStopDaemon_NoPIDFile verifies StopDaemon succeeds silently when there is no PID file.
func TestStopDaemon_NoPIDFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)

	if err := StopDaemon(); err != nil {
		t.Fatalf("StopDaemon with no PID file should succeed, got: %v", err)
	}
}

// TestStopDaemon_NonExistentPID verifies that StopDaemon treats a PID file pointing at a dead
// process as stale and removes it instead of returning an error or killing a reused PID.
func TestStopDaemon_NonExistentPID(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)
	pidFile := filepath.Join(tmpHome, "daemon.pid")

	// Use a large PID that we're confident isn't in use. On Linux the default pid_max is 32768
	// and on macOS it's 99999; 0x7fffffff is well above both.
	deadPID := 0x7fffffff

	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", deadPID)), 0644); err != nil {
		t.Fatalf("failed to write PID file: %v", err)
	}

	if err := StopDaemon(); err != nil {
		t.Fatalf("StopDaemon returned error for dead PID: %v", err)
	}

	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("expected stale PID file to be removed, stat err = %v", err)
	}
}

// TestStopDaemon_RefusesSelfPID verifies that StopDaemon refuses to kill the current test process
// even if the PID file points at it.
func TestStopDaemon_RefusesSelfPID(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)
	pidFile := filepath.Join(tmpHome, "daemon.pid")

	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		t.Fatalf("failed to write PID file: %v", err)
	}

	// If StopDaemon killed us, the test binary would exit with signal: killed.
	if err := StopDaemon(); err != nil {
		t.Fatalf("StopDaemon returned error: %v", err)
	}

	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("expected PID file to be removed, stat err = %v", err)
	}
}

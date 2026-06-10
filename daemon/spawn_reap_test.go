package daemon

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestStartDaemonChildReapsExitedChild pins the #816 fix: startDaemonChild
// must reap its child once it exits, or every dead daemon lingers as an
// `[af] <defunct>` zombie for the life of the TUI — one per upgrade/respawn
// cycle. The test spawns `true` (which exits immediately, ignoring the
// --daemon argument) instead of a real daemon, so it never touches the
// control socket or any supervised daemon on the machine.
//
// Detection: a zombie still occupies its process-table slot, so signal 0
// succeeds against it. Only after the parent Wait()s does the kernel release
// the PID and signal 0 return ESRCH.
func TestStartDaemonChildReapsExitedChild(t *testing.T) {
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("no `true` binary on PATH: %v", err)
	}

	pid, err := startDaemonChild(truePath)
	if err != nil {
		t.Fatalf("startDaemonChild: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return // child exited and was reaped
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child PID %d still occupies a process-table slot 5s after exit; daemon child was not reaped (#816)", pid)
}

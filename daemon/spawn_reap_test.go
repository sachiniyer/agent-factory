package daemon

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"testing"
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

	// `true` exits immediately; once startDaemonChild reaps it the kernel
	// releases the PID and signal 0 returns ESRCH. Event-driven with a generous
	// bound so a loaded runner cannot expire the wait before the reap lands
	// (#878) — a genuine reap regression (#816) still fails, just after the
	// generous timeout.
	waitForReady(t, fmt.Sprintf("daemon child PID %d reaped (signal 0 -> ESRCH)", pid), func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	})
}

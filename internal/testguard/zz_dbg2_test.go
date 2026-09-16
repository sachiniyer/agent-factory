//go:build linux

package testguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestGroupPinDebug2(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pin.pid")
	wrapper := exec.Command("sh", "-c", fmt.Sprintf(
		"sh -c '%s' & echo $! > %q; exec sleep 60", groupPinScript, pidFile))
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
	becomeOrphanReaper(t)
	wrapper.Process.Kill()
	dead := waitForProcessDeath(pinPID, 3*time.Second)
	t.Logf("waitForProcessDeath=%v", dead)
	out, _ := exec.Command("ps", "-o", "pid=,ppid=,stat=,args=", "-p", fmt.Sprint(pinPID)).CombinedOutput()
	t.Logf("pin at end: %s", out)
	if !dead {
		t.Errorf("pin survived")
	}
}

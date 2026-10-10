//go:build linux || darwin

package doctor

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// observedZombie reports whether pid is parked as a zombie (exited but not yet
// collected), read straight from the platform process table independently of
// the code under test. It mirrors the proctree package's own zombie observers
// (see proctree/zombie_*_test.go).
func observedZombie(pid int) bool {
	_, err := proctree.Lookup(pid)
	// ErrProcessExited is the positive zombie finding on both Linux (/proc
	// state 'Z') and Darwin (sysctl SZOMB).
	return err != nil && errors.Is(err, proctree.ErrProcessExited)
}

// TestProcessExitedBeforeCancel_Zombie asserts the non-waiting exit probe used
// by cmd.Cancel sees a process that has exited but not yet been reaped. This is
// the state an external SIGKILL (OOM killer, kill -9) leaves at the moment the
// deadline fires — the case the prior wait4/waitid probes deadlocked on Darwin
// trying to observe. The probe must report it exited so ctxKilled stays false
// and the death is classified as a failure rather than a timeout.
func TestProcessExitedBeforeCancel_Zombie(t *testing.T) {
	// Start a real child, kill it, and deliberately do NOT reap it so the
	// kernel keeps its entry as a zombie for the duration of the check.
	cmd := exec.Command("sleep", "300")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })

	require.NoError(t, cmd.Process.Kill())

	// Wait for the kernel to park the entry in the zombie state before probing.
	deadline := time.Now().Add(2 * time.Second)
	for !observedZombie(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d never became a zombie", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if !processExitedBeforeCancel(pid) {
		t.Fatalf("processExitedBeforeCancel(zombie pid %d) = false, want true: an exited-but-uncollected process must be seen as exited so the context does not claim to have killed it", pid)
	}
}

// TestProcessExitedBeforeCancel_Alive asserts the probe does not misreport a
// running process as exited, which would suppress the context's kill and let a
// genuinely hung coder whoami escape the timeout classification.
func TestProcessExitedBeforeCancel_Alive(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	if processExitedBeforeCancel(cmd.Process.Pid) {
		t.Fatalf("processExitedBeforeCancel(live pid %d) = true, want false: a running process must not be reported as exited", cmd.Process.Pid)
	}
}

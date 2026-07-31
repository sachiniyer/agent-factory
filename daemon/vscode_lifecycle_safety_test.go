package daemon

// These tests cover fail-closed boundaries between editor startup, reaping,
// respawn admission, and daemon shutdown.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestVSCodeServer_ReaperRetainsOwnerUntilGroupExitConfirmed(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the throwaway leader: %v", err)
	}
	ownerPath := filepath.Join(t.TempDir(), "editor.owner.json")
	if err := os.WriteFile(ownerPath, []byte("owned\n"), 0o600); err != nil {
		t.Fatalf("writing owner fixture: %v", err)
	}

	s := &vscodeServer{
		worktree: "/worktree", cmd: cmd, ownerPath: ownerPath,
		stopGrace: 10 * time.Millisecond, exited: make(chan struct{}),
		killGroup:  func(int, syscall.Signal) error { return nil },
		groupAlive: func(int) (bool, error) { return true, nil },
	}
	s.reap()

	if _, err := os.Stat(ownerPath); err != nil {
		t.Fatalf("reaper removed durable ownership before confirming group exit: %v", err)
	}
	if s.reapErr == nil {
		t.Fatal("reaper reported success when the process group still existed after SIGKILL")
	}
}

func TestVSCodeServer_DoesNotExecBeforeOwnerPersist(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "editor.args")
	binary := writeFakeVSCodeBinary(t, "code-server", map[string]string{fakeVSCodeArgsEnv: argsPath})
	v := newTestVSCodeSupervisor(t, binary)
	worktree := t.TempDir()

	originalWrite := writeVSCodeOwnerForStart
	writeEntered := make(chan struct{})
	releaseWrite := make(chan struct{})
	writeVSCodeOwnerForStart = func(path string, owner vscodeOwnerRecord) error {
		close(writeEntered)
		<-releaseWrite
		return originalWrite(path, owner)
	}
	t.Cleanup(func() { writeVSCodeOwnerForStart = originalWrite })

	result := make(chan error, 1)
	go func() {
		_, err := v.ensureServer("startup-owner", worktree)
		result <- err
	}()
	select {
	case <-writeEntered:
	case <-time.After(5 * time.Second):
		close(releaseWrite)
		t.Fatal("startup never reached durable owner persistence")
	}

	startedBeforeOwner := false
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(argsPath); err == nil {
			startedBeforeOwner = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(releaseWrite)
	if err := <-result; err != nil {
		t.Fatalf("ensureServer after owner persistence: %v", err)
	}
	if startedBeforeOwner {
		t.Fatal("editor executed before its durable ownership record was persisted")
	}
}

func TestVSCodeSupervisor_ReconcilesFailedReaperBeforeRespawn(t *testing.T) {
	shortAFHome(t)
	const (
		key        = "failed-reaper"
		instanceID = "instance-1"
	)
	writeUnreadableVSCodeOwnerFixture(t, key)
	v := newVSCodeSupervisor()
	configured := false
	v.configuredBinary = func() string {
		configured = true
		return "/missing/code-server"
	}
	deadExited := make(chan struct{})
	close(deadExited)
	v.servers[key] = &vscodeServer{
		worktree: t.TempDir(), instanceID: instanceID, exited: deadExited,
		ready: true, reapErr: errors.New("group cleanup unknown"),
	}

	_, err := v.ensureServerForInstance(key, instanceID, t.TempDir())
	if err == nil {
		t.Fatal("ensureServer replaced an editor whose durable cleanup remained unknown")
	}
	if configured {
		t.Fatal("ensureServer began respawn before reconciling the failed reaper's durable owner")
	}
}

func TestVSCodeSupervisor_StopWaitsForReservedReconciliation(t *testing.T) {
	shortAFHome(t)
	v := newVSCodeSupervisor()
	const key = "shutdown-reservation"
	v.mu.Lock()
	if !v.reserveReconcileLocked(key) {
		v.mu.Unlock()
		t.Fatal("could not reserve reconciliation fixture")
	}
	v.mu.Unlock()

	done := make(chan struct{})
	go func() {
		v.Stop()
		close(done)
	}()
	returnedEarly := false
	select {
	case <-done:
		returnedEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	v.releaseReconcile(key)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not finish after the reserved reconciliation was released")
	}
	if returnedEarly {
		t.Fatal("Stop scanned persisted owners before an active per-key reconciliation drained")
	}
}

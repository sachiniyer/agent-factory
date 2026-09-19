//go:build linux

package testguard

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"golang.org/x/sys/unix"
)

// childSubreaper reads the process-wide PR_SET_CHILD_SUBREAPER flag — the
// attribute becomeOrphanReaper must hold only while a fixture group is live.
// The getter writes through arg2, so PrctlRetInt cannot reach it.
func childSubreaper(t *testing.T) int {
	t.Helper()
	var v int
	if err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&v)), 0, 0, 0); err != nil {
		t.Fatalf("read child-subreaper flag: %v", err)
	}
	return v
}

func orphanReaperDepth() int {
	orphanReaperCount.mu.Lock()
	defer orphanReaperCount.mu.Unlock()
	return orphanReaperCount.n
}

// #4417 review: the flag is process-wide while the cleanups are per group,
// so clearing it at the first cleanup un-adopts a second fixture's orphans —
// the pin's `sleep 1` included — back to an init that may never collect.
// Two registrations must nest: set once, released by the LAST cleanup.
func TestBecomeOrphanReaperReleasesOnlyAfterLastGroup(t *testing.T) {
	t.Run("inner", func(t *testing.T) {
		becomeOrphanReaper(t)
		becomeOrphanReaper(t)
		if depth := orphanReaperDepth(); depth != 2 {
			t.Fatalf("two registrations: reaper depth %d, want 2", depth)
		}
		if got := childSubreaper(t); got != 1 {
			t.Fatalf("subreaper flag %d mid-test, want set", got)
		}
	})
	if depth := orphanReaperDepth(); depth != 0 {
		t.Fatalf("reaper depth %d after the inner test's cleanups, want 0", depth)
	}
	if got := childSubreaper(t); got != 0 {
		t.Fatalf("subreaper flag %d after the last group cleaned up, want cleared", got)
	}
}

// pinExit waits for a spawned pin process to die on its own. The watch polls
// once a second, so the window leaves generous slack for a loaded runner.
func pinExit(t *testing.T, pin *exec.Cmd, waitErr chan error) {
	t.Helper()
	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("pin exited %v, want a clean exit", err)
		}
	case <-time.After(10 * time.Second):
		_ = pin.Process.Kill()
		<-waitErr
		t.Fatal("pin kept watching after its owner check should have ended it")
	}
}

// #4417 review: the pin's watch binds the owner's process instance, not its
// pid slot. A stamp naming another process — what a recycled pid would
// present — ends the pin on the first poll instead of holding the group.
func TestGroupPinExitsOnStampMismatch(t *testing.T) {
	pin := StartGroupProcess(t, exec.Command("sh", "-c", groupPinScript,
		"pin-test", strconv.Itoa(os.Getpid()), "1"))
	waitErr := make(chan error, 1)
	go func() { waitErr <- pin.Wait() }()
	pinExit(t, pin, waitErr)
}

// The other half: a CORRECT stamp keeps the pin alive — the identity arm
// must not fire on the real owner.
func TestGroupPinStaysForMatchingStamp(t *testing.T) {
	self, err := proctree.Lookup(os.Getpid())
	if err != nil || self.StartID == 0 {
		t.Skipf("this kernel did not stamp the test process: %v", err)
	}
	pin := StartGroupProcess(t, exec.Command("sh", "-c", groupPinScript,
		"pin-test", strconv.Itoa(os.Getpid()), strconv.FormatUint(self.StartID, 10)))
	done := make(chan error, 1)
	go func() { done <- pin.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("pin exited %v against its real owner stamp, want it still watching", err)
	case <-time.After(2500 * time.Millisecond):
		// Still running: the cleanup kill registered by StartGroupProcess
		// collects it from here.
	}
}

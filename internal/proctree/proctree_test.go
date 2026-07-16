package proctree

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// startSleeper spawns a `sleep` child owned by this test and returns its
// Process entry. The child is killed on cleanup regardless of test outcome.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func TestSnapshotIncludesSelfAndChild(t *testing.T) {
	testguard.RequireProcFS(t)
	child := startSleeper(t)
	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	self, ok := snap[os.Getpid()]
	if !ok {
		t.Fatalf("snapshot missing our own pid %d", os.Getpid())
	}
	if self.StartTicks == 0 {
		t.Errorf("self StartTicks = 0, want nonzero")
	}
	cp, ok := snap[child.Process.Pid]
	if !ok {
		t.Fatalf("snapshot missing child pid %d", child.Process.Pid)
	}
	if cp.PPID != os.Getpid() {
		t.Errorf("child PPID = %d, want %d", cp.PPID, os.Getpid())
	}
	if cp.Comm != "sleep" {
		t.Errorf("child Comm = %q, want %q", cp.Comm, "sleep")
	}
}

func TestTreeOfFindsGrandchild(t *testing.T) {
	testguard.RequireProcFS(t)
	// sh -c spawns sleep as a grandchild of the test process.
	cmd := exec.Command("sh", "-c", "sleep 300 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sh: %v", err)
	}
	t.Cleanup(func() {
		snap, _ := Snapshot()
		for _, p := range TreeOf(snap, cmd.Process.Pid) {
			_ = syscall.Kill(p.PID, syscall.SIGKILL)
		}
		_, _ = cmd.Process.Wait()
	})

	// The shell needs a moment to fork the sleep.
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, err := Snapshot()
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		tree := TreeOf(snap, cmd.Process.Pid)
		if len(tree) >= 2 {
			foundSleep := false
			for _, p := range tree[1:] {
				if p.Comm == "sleep" {
					foundSleep = true
				}
			}
			// The grandchild sleep may not have exec'd yet even though the
			// shell's children are already visible; keep polling until the
			// deadline instead of failing on the first incomplete scan.
			if foundSleep {
				if tree[0].PID != cmd.Process.Pid {
					t.Errorf("tree root = %d, want %d", tree[0].PID, cmd.Process.Pid)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("sleep grandchild never appeared under pid %d", cmd.Process.Pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTreeOfMissingRoot(t *testing.T) {
	testguard.RequireProcFS(t)
	snap := map[int]Process{2: {PID: 2, PPID: 1}}
	if tree := TreeOf(snap, 999999); tree != nil {
		t.Errorf("TreeOf(missing root) = %v, want nil", tree)
	}
}

func TestSessionMembers(t *testing.T) {
	testguard.RequireProcFS(t)
	snap := map[int]Process{
		10: {PID: 10, PPID: 1, SID: 10},
		11: {PID: 11, PPID: 1, SID: 10}, // reparented but same kernel session
		12: {PID: 12, PPID: 1, SID: 12},
	}
	members := SessionMembers(snap, 10)
	if len(members) != 2 {
		t.Fatalf("SessionMembers = %v, want 2 members", members)
	}
	for _, m := range members {
		if m.SID != 10 {
			t.Errorf("member %d has SID %d, want 10", m.PID, m.SID)
		}
	}
}

func TestSignalVerifiesIdentity(t *testing.T) {
	testguard.RequireProcFS(t)
	child := startSleeper(t)
	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	p := snap[child.Process.Pid]

	// Wrong start time must refuse to signal.
	stale := p
	stale.StartTicks++
	if err := Signal(stale, syscall.SIGTERM); err != ErrIdentityChanged {
		t.Errorf("Signal(stale identity) = %v, want ErrIdentityChanged", err)
	}
	if !AliveSame(p) {
		t.Fatalf("child died from a stale-identity signal — identity guard failed")
	}

	// Correct identity kills it.
	if err := Signal(p, syscall.SIGKILL); err != nil {
		t.Fatalf("Signal(valid identity) = %v", err)
	}
	_, _ = child.Process.Wait()
	if AliveSame(p) {
		t.Errorf("child still alive after SIGKILL")
	}
}

// TestSignalReapedInTOCTOUWindow simulates the process being reaped in the
// unavoidable window between the identity check and the delivery of the
// signal: AliveSame passes (the child is genuinely alive) but the kernel
// returns ESRCH for the kill. Signal must map that to ErrIdentityChanged so
// callers treat "already gone" as success, not a warnable failure (#1151).
func TestSignalReapedInTOCTOUWindow(t *testing.T) {
	testguard.RequireProcFS(t)
	child := startSleeper(t)
	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	p := snap[child.Process.Pid]
	if !AliveSame(p) {
		t.Fatalf("child not alive at snapshot time")
	}

	orig := kill
	t.Cleanup(func() { kill = orig })
	kill = func(pid int, sig syscall.Signal) error { return syscall.ESRCH }

	if err := Signal(p, syscall.SIGTERM); err != ErrIdentityChanged {
		t.Errorf("Signal(reaped-in-window) = %v, want ErrIdentityChanged", err)
	}
}

// TestSignalPropagatesNonESRCHErrors makes sure the ESRCH coercion does not
// swallow genuine signal failures (e.g. EPERM): those must still surface.
func TestSignalPropagatesNonESRCHErrors(t *testing.T) {
	testguard.RequireProcFS(t)
	child := startSleeper(t)
	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	p := snap[child.Process.Pid]

	orig := kill
	t.Cleanup(func() { kill = orig })
	kill = func(pid int, sig syscall.Signal) error { return syscall.EPERM }

	if err := Signal(p, syscall.SIGTERM); err != syscall.EPERM {
		t.Errorf("Signal(EPERM) = %v, want EPERM", err)
	}
}

// TestKillEscalatingNoWarnOnTOCTOUExit is the end-to-end contract: when a
// survivor is reaped in the TOCTOU window, KillEscalating must not log a
// "failed to" warning for it (#1151).
func TestKillEscalatingNoWarnOnTOCTOUExit(t *testing.T) {
	testguard.RequireProcFS(t)
	child := startSleeper(t)
	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	p := snap[child.Process.Pid]

	orig := kill
	t.Cleanup(func() { kill = orig })
	kill = func(pid int, sig syscall.Signal) error { return syscall.ESRCH }

	var logged []string
	logf := func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	// Zero grace so the alive child is treated as a survivor immediately,
	// then its kill races-out to ESRCH.
	KillEscalating([]Process{p}, 0, 10*time.Millisecond, logf)

	for _, line := range logged {
		if strings.Contains(line, "failed to") {
			t.Errorf("KillEscalating logged a failure for a process gone in the TOCTOU window: %q", line)
		}
	}
}

func TestSignalRefusesSelfAndInit(t *testing.T) {
	testguard.RequireProcFS(t)
	if err := Signal(Process{PID: 1}, syscall.SIGTERM); err == nil {
		t.Errorf("Signal(pid 1) succeeded, want refusal")
	}
	if err := Signal(Process{PID: os.Getpid()}, syscall.SIGTERM); err == nil {
		t.Errorf("Signal(self) succeeded, want refusal")
	}
}

func TestEnvValue(t *testing.T) {
	testguard.RequireProcFS(t)
	cmd := exec.Command("sleep", "300")
	cmd.Env = append(os.Environ(), "AF_TEST_MARKER=hello")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	// /proc/<pid>/environ is empty in the window between fork and exec;
	// poll briefly rather than racing it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got, ok := EnvValue(cmd.Process.Pid, "AF_TEST_MARKER"); ok && got == "hello" {
			break
		}
		if time.Now().After(deadline) {
			got, ok := EnvValue(cmd.Process.Pid, "AF_TEST_MARKER")
			t.Fatalf("EnvValue = (%q, %v), want (\"hello\", true)", got, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := EnvValue(cmd.Process.Pid, "AF_TEST_ABSENT"); ok {
		t.Errorf("EnvValue found an absent variable")
	}
}

func TestCPUFractionIdleSleeper(t *testing.T) {
	testguard.RequireProcFS(t)
	child := startSleeper(t)
	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	frac, _, err := CPUFraction(snap[child.Process.Pid])
	if err != nil {
		t.Fatalf("CPUFraction: %v", err)
	}
	if frac > 0.5 {
		t.Errorf("idle sleeper CPU fraction = %v, want near 0", frac)
	}
}

func TestCmdline(t *testing.T) {
	testguard.RequireProcFS(t)
	child := startSleeper(t)
	// /proc/<pid>/cmdline is empty in the window between fork and exec;
	// poll briefly rather than racing it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := Cmdline(child.Process.Pid); got == "sleep 300" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Cmdline = %q, want %q", Cmdline(child.Process.Pid), "sleep 300")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

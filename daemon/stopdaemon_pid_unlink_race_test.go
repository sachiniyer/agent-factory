package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// These tests pin the TOCTOU narrowing on stopDaemonUntil's two reachable
// stale-PID early branches: the dead-PID branch (proc.Signal(0) failed) and
// the not-a-daemon branch (isAgentFactoryDaemon false). #295 added those
// branches, each unconditionally os.Remove'ing daemon.pid based on the single
// os.ReadFile at the top of stopDaemonUntil — so a same-home daemon that
// atomically rewrote daemon.pid (writeDaemonPIDFile: temp-then-rename under the
// sidecar lock) in the window between that read and the unlink had its freshly
// written file deleted and was orphaned from the PID-file stop/identify path.
// #4795 added the TOCTOU-safe sibling removePIDFileIfStillNames and routed only
// the daemonForeign/daemonUnverifiable branches through it; these tests pin the
// routing of the two reachable early branches through the same safe sibling,
// driving stopDaemonUntil end-to-end with the race deterministically
// interleaved by testHookStopDaemonBeforeStaleUnlink.

// fakeFreshDaemonPID is the PID a concurrently-starting same-home daemon writes
// to daemon.pid in the race window. It is a fixed, plausible PID value used
// only as file CONTENT (the safe sibling compares the number, it never probes
// the process), so its liveness is irrelevant — it only has to differ from the
// stale PID the branch under test read.
const fakeFreshDaemonPID = 654321

// reliablyDeadPID returns a PID that is no longer running (exited and reaped),
// so stopDaemonUntil's proc.Signal(0) deterministically hits the dead-PID
// branch. A reaped process is gone from the kernel's process table, but its
// number may be recycled; the loop re-checks pidLooksAlive and spawns a fresh
// short-lived proc if the number was reused before the call returns. Less than
// one iteration is the common case.
func reliablyDeadPID(t *testing.T) int {
	t.Helper()
	if _, err := exec.LookPath("true"); err != nil {
		t.Skipf("true not available to reap a reliably-dead PID: %v", err)
	}
	for i := 0; i < 16; i++ {
		cmd := exec.Command("true")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start short-lived proc: %v", err)
		}
		_ = cmd.Wait() // reap; the PID is now gone from the process table
		pid := cmd.Process.Pid
		if !pidLooksAlive(pid) {
			return pid
		}
	}
	t.Skip("could not obtain a reliably-dead PID (numbers kept being recycled); retry")
	return 0
}

// rewritePIDFileLikeFreshDaemon atomically rewrites pidFile with pid under the
// sidecar daemon.pid.lock, mirroring writeDaemonPIDFile (withDaemonPIDLock +
// config.AtomicWriteFileRefusingLink, temp-then-rename). It is the realistic
// writer a concurrently-starting same-home daemon is: a writer whose lock does
// not protect it against an unlink that does not coordinate with the sidecar
// lock, which is exactly the unlinked remover the early branches were before
// this fix.
func rewritePIDFileLikeFreshDaemon(t *testing.T, pidFile string, pid int) {
	t.Helper()
	if err := withDaemonPIDLock(pidFile, time.Now().Add(2*time.Second), func() error {
		return config.AtomicWriteFileRefusingLink(pidFile, []byte(fmt.Sprintf("%d", pid)), 0600)
	}); err != nil {
		t.Fatalf("rewrite daemon.pid like a fresh daemon: %v", err)
	}
}

// installFreshRewriteHook installs testHookStopDaemonBeforeStaleUnlink so that,
// when a stale-PID branch fires, a same-home daemon's atomic rewrite of
// daemon.pid lands in the window between stopDaemonUntil's top-of-function
// read and the branch's cleanup. The race is serialized deterministically
// (the hook runs synchronously, right before the cleanup call), reproducing
// the worst-case interleaving without flakiness. It returns the fresh PID the
// hook wrote so the caller can assert the file still names it.
func installFreshRewriteHook(t *testing.T, pidFile string, avoid int) int {
	t.Helper()
	fresh := fakeFreshDaemonPID
	if fresh == avoid {
		fresh = fakeFreshDaemonPID + 1
	}
	prev := testHookStopDaemonBeforeStaleUnlink
	t.Cleanup(func() { testHookStopDaemonBeforeStaleUnlink = prev })
	rewrote := false
	testHookStopDaemonBeforeStaleUnlink = func() {
		if rewrote {
			return
		}
		rewrote = true
		rewritePIDFileLikeFreshDaemon(t, pidFile, fresh)
	}
	return fresh
}

// TestStopDaemonUntil_DeadPIDBranchUnlinksFreshDaemonPIDFile is the
// deterministic reproduction of the race #295 introduced on the dead-PID
// branch and #4795 closed only for the foreign/unverifiable branches. A stale
// daemon.pid names a dead PID; stopDaemonUntil reads it and reaches the
// Signal(0)-failed (dead-PID) branch; testHookStopDaemonBeforeStaleUnlink fires
// right before that branch's cleanup and atomically rewrites daemon.pid with a
// fresh daemon's PID under the sidecar lock. Pre-fix the branch's
// removeStaleDaemonPIDFile unconditionally unlinked whatever the file named at
// that instant — the freshly written PID file — orphaning the new running
// daemon. Post-fix the branch routes through removePIDFileIfStillNames, which
// re-reads under the sidecar lock, sees the fresh PID ≠ the stale dead PID it
// read, and leaves the file to its owner.
func TestStopDaemonUntil_DeadPIDBranchUnlinksFreshDaemonPIDFile(t *testing.T) {
	tmpHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)
	pidFile := filepath.Join(tmpHome, "daemon.pid")

	stale := reliablyDeadPID(t)
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", stale)), 0600); err != nil {
		t.Fatalf("write stale PID file: %v", err)
	}

	fresh := installFreshRewriteHook(t, pidFile, stale)

	stopped, err := stopDaemonUntil(time.Time{})
	if err != nil {
		t.Fatalf("stopDaemonUntil: %v", err)
	}
	if stopped {
		t.Fatalf("stopDaemonUntil reported stopped=true for a stale dead PID %d; expected false", stale)
	}

	data, statErr := os.ReadFile(pidFile)
	if statErr != nil {
		t.Fatalf("stopDaemonUntil's dead-PID branch deleted the PID file after a fresh daemon "+
			"atomically rewrote it with PID %d (the file no longer names the stale dead PID %d the "+
			"branch read); the unconditional removeStaleDaemonPIDFile is not guarded by the sidecar "+
			"lock + re-read that removePIDFileIfStillNames performs — the TOCTOU race the safe "+
			"sibling was built to close", fresh, stale)
	}
	if got := strings.TrimSpace(string(data)); got != fmt.Sprintf("%d", fresh) {
		t.Fatalf("PID file content = %q, want the fresh daemon's PID %d preserved", got, fresh)
	}
}

// TestStopDaemonUntil_InvalidPIDBranchUnlinksFreshDaemonPIDFile covers the
// third reachable early branch: the invalid-PID branch (pid <= 1 or pid ==
// os.Getpid()), which #295 unconditionally os.Remove'd on the same single read
// as the other early branches. A same-home daemon can atomically rewrite
// daemon.pid between that read and this cleanup, so the branch must re-read
// under the sidecar lock and leave a freshly written valid PID file rather than
// unlinking it unconditionally — the same fix as the dead-PID and not-a-daemon
// branches above.
func TestStopDaemonUntil_InvalidPIDBranchUnlinksFreshDaemonPIDFile(t *testing.T) {
	tmpHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)
	pidFile := filepath.Join(tmpHome, "daemon.pid")

	// pid == 0 is the cheapest invalid PID the branch accepts (pid <= 1); its
	// only role is file CONTENT for the branch under test, so it does not need
	// to be a live or recycled process.
	const stale = 0
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", stale)), 0600); err != nil {
		t.Fatalf("write stale PID file: %v", err)
	}

	fresh := installFreshRewriteHook(t, pidFile, stale)

	stopped, err := stopDaemonUntil(time.Time{})
	if err != nil {
		t.Fatalf("stopDaemonUntil: %v", err)
	}
	if stopped {
		t.Fatalf("stopDaemonUntil reported stopped=true for an invalid PID %d; expected false", stale)
	}

	data, statErr := os.ReadFile(pidFile)
	if statErr != nil {
		t.Fatalf("stopDaemonUntil's invalid-PID branch deleted the PID file after a fresh daemon "+
			"atomically rewrote it with PID %d (the file no longer names the invalid PID %d the "+
			"branch read); the unconditional removeStaleDaemonPIDFile is not guarded by the sidecar "+
			"lock + re-read that removePIDFileIfStillNames performs — the TOCTOU race the safe "+
			"sibling was built to close", fresh, stale)
	}
	if got := strings.TrimSpace(string(data)); got != fmt.Sprintf("%d", fresh) {
		t.Fatalf("PID file content = %q, want the fresh daemon's PID %d preserved", got, fresh)
	}
}

// TestStopDaemonUntil_NotADaemonBranchUnlinksFreshDaemonPIDFile is the companion
// covering the second reachable early branch: a live PID that is NOT an
// agent-factory daemon (a recycled number now owned by an unrelated process, a
// wider window because isAgentFactoryDaemon reads /proc/<pid>/cmdline before
// the cleanup). The race and the fix are the same as the dead-PID branch
// above — the not-a-daemon branch must also re-read under the sidecar lock and
// leave a freshly written valid PID file rather than unlinking it
// unconditionally.
func TestStopDaemonUntil_NotADaemonBranchUnlinksFreshDaemonPIDFile(t *testing.T) {
	tmpHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)
	pidFile := filepath.Join(tmpHome, "daemon.pid")

	// A live process whose argv is plainly NOT an agent-factory daemon so the
	// not-a-daemon branch fires (Signal(0) succeeds, isAgentFactoryDaemon is
	// false). Its own process group so cleanup reaps the whole tree.
	sleepCmd := exec.Command("sleep", "300")
	sleepCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sleepCmd.Start(); err != nil {
		t.Fatalf("start live non-daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-sleepCmd.Process.Pid, syscall.SIGKILL)
		_ = sleepCmd.Wait()
	})
	liveNonDaemon := sleepCmd.Process.Pid
	waitForReady(t, fmt.Sprintf("live non-daemon pid=%d is alive and not classified as an af daemon", liveNonDaemon), func() bool {
		return pidLooksAlive(liveNonDaemon) && !isAgentFactoryDaemon(liveNonDaemon)
	})

	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", liveNonDaemon)), 0600); err != nil {
		t.Fatalf("write stale PID file: %v", err)
	}

	fresh := installFreshRewriteHook(t, pidFile, liveNonDaemon)

	stopped, err := stopDaemonUntil(time.Time{})
	if err != nil {
		t.Fatalf("stopDaemonUntil: %v", err)
	}
	if stopped {
		t.Fatalf("stopDaemonUntil reported stopped=true for a live non-daemon PID %d; expected false", liveNonDaemon)
	}

	data, statErr := os.ReadFile(pidFile)
	if statErr != nil {
		t.Fatalf("stopDaemonUntil's not-a-daemon branch deleted the PID file after a fresh daemon "+
			"atomically rewrote it with PID %d (the file no longer names the stale non-daemon PID %d "+
			"the branch read); the unconditional removeStaleDaemonPIDFile is not guarded by the "+
			"sidecar lock + re-read that removePIDFileIfStillNames performs — the TOCTOU race the "+
			"safe sibling was built to close", fresh, liveNonDaemon)
	}
	if got := strings.TrimSpace(string(data)); got != fmt.Sprintf("%d", fresh) {
		t.Fatalf("PID file content = %q, want the fresh daemon's PID %d preserved", got, fresh)
	}

	if !pidLooksAlive(liveNonDaemon) {
		t.Fatalf("stopDaemonUntil killed the live non-daemon pid=%d; the not-a-daemon branch must "+
			"treat the PID file as stale, not signal the process it names", liveNonDaemon)
	}
}

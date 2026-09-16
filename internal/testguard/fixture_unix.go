//go:build linux || darwin

package testguard

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// StartGroupProcess starts cmd in its own process group
// (SysProcAttr.Setpgid) and registers cleanups that SIGKILL the whole group
// and then reap the direct child — the surviving-the-test guarantee every
// spinning or TERM-ignoring fixture needs (#4412).
//
// Kill the GROUP, not just the direct process: fixtures like
// `while :; do sleep 1; done` fork a fresh sleeper each iteration, and
// `... &` fixtures background children on purpose — killing only the shell
// orphans whatever is mid-flight. SIGKILL, not SIGTERM, because several
// fixtures deliberately `trap "" TERM` and must stay uncooperative for the
// test to remain meaningful; cleanup kills them from the harness side.
//
// The returned cmd is started; callers may still Wait on it themselves — the
// cleanup reap is a harmless no-op then — and should not Setpgid twice.
func StartGroupProcess(t testing.TB, cmd *exec.Cmd) *exec.Cmd {
	t.Helper()
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture process %q: %v", cmd.Path, err)
	}
	// Pin the group id with a second child joined to the same group and kept
	// unreaped until AFTER the cleanup kill. While any member exists — alive,
	// or a zombie the harness still holds — the pgid cannot be recycled
	// (POSIX XBD 3.297), so the cleanup's kill(-pgid) can never land on a
	// foreign group that acquired the number after the fixture's own members
	// died. Without the pin a caller that Waits the leader mid-test frees the
	// id early, and a group kill at an arbitrarily later point is exactly the
	// recycled-pgid hazard the daemon's reapers avoid by killing microseconds
	// after their own Wait (daemon/vscode_server.go reap). The pin dies with
	// the group when the kill lands; if a test kills the group itself first,
	// its held zombie keeps pinning.
	//
	// The pin cannot be an unguarded `sleep 86400`: the crash/kill case it
	// exists for never runs the cleanup, so the sleeper would be reparented
	// and survive a day for EVERY StartGroupProcess call — confirmed on a
	// `go test -timeout` kill, which left the pin under PID 1 (#4417 review).
	// It watches the test's own pid instead — the same existence arm
	// ExitWhenOrphaned uses for fixtures under an intermediary, made
	// zombie-aware through ps state because a killed owner still answers
	// kill -0 while it awaits collection. ppid drift cannot stand in for
	// this: a pin orphaned between fork and first poll captures the reaper
	// as its parent and watches a process that never goes away.
	pin := exec.Command("sh", "-c", groupPinScript, "af-testguard-pin",
		strconv.Itoa(os.Getpid()))
	pin.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: cmd.Process.Pid}
	if err := pin.Start(); err != nil {
		// The leader is already running and no cleanup for it is registered
		// yet — a bare Fatalf here strands the indefinitely-blocking fixture
		// this helper exists to contain, in exactly the failure conditions
		// (a transient fork/exec refusal) that produce one. Kill the group
		// and collect the leader before failing; the pin never started, so
		// nothing else holds the id.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		t.Fatalf("start fixture group pin: %v", err)
	}
	// Registered in this order so cleanup runs kill-then-wait (t.Cleanup is
	// LIFO): the pin's reap is registered before the leader's and the kill is
	// registered last, so both members still pin the id when the kill is
	// delivered. Wait before the kill would block forever on a live fixture.
	// Process.Wait rather than cmd.Wait: a test may already hold a cmd.Wait
	// goroutine, and concurrent Cmd.Wait calls are a data race on
	// ProcessState — os.Process.Wait is the concurrent-safe reap.
	t.Cleanup(func() { _, _ = pin.Process.Wait() })
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })
	KillProcessGroupOnCleanup(t, cmd.Process.Pid)
	return cmd
}

// StartGroupProcessUnpinned starts cmd in its own process group like
// StartGroupProcess but WITHOUT the pin member, for fixtures whose tests
// must OBSERVE the group empty mid-test: a held pin keeps kill(-pgid, 0)
// reporting the group alive after the fixture's real members die, which
// defeats any wait-for-group-exit loop the code under test runs (the daemon
// vscode teardown waits on exactly that probe).
//
// Dropping the pin drops the pgid-recycling guard with it, so this variant
// never signals the GROUP at cleanup — it kills the direct child only.
// os.Process.Kill is a safe no-op once the child is reaped (the handle
// reports the process done instead of signaling a recycled pid), which makes
// this correct ONLY for single-member fixtures — `exec sleep N` and the
// like. A fixture that forks children into its group needs
// StartGroupProcess: without the pin, a late group kill is the hazard the
// pin was added to close.
//
// The pin is redundant for the callers that need this variant anyway: their
// mid-test group signals go through a seam that only signals while the
// recorded leader is alive (daemon/vscode_owner.go refuses to signal a
// leaderless group), and a live leader pins its own pgid for the signal's
// duration. The cleanup child-kill covers the test that never signals.
func StartGroupProcessUnpinned(t testing.TB, cmd *exec.Cmd) *exec.Cmd {
	t.Helper()
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture process %q: %v", cmd.Path, err)
	}
	// Kill the direct child, not the group: with no member pinning the id, a
	// post-empty group kill is the recycled-pgid hazard the pinned variant
	// exists to close. LIFO registration runs Kill before Wait, and a caller
	// holding its own Wait goroutine makes the cleanup reap a no-op.
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

// groupPinScript is the pin process's body; $1 is the pid it must outlive —
// its spawner, the test binary. See StartGroupProcess for why a bare `sleep
// 86400` leaks and why the death signal is the owner's ps STATE rather than
// the pin's ppid or a kill -0: a zombie owner still answers signal-0, and a
// ppid captured after reparenting names a reaper that never dies. Empty ps
// output means the owner is gone outright; Z/X/x mean it is dead but
// uncollected — both end the pin. command -v degrades a ps-less minimal box
// to the bounded sleeper, which can still leak for a day but keeps the pgid
// pin it was bought for.
const groupPinScript = `if command -v ps >/dev/null 2>&1; then _af_pin_i=0; while [ "$_af_pin_i" -lt 86400 ]; do _af_pin_stat=$(ps -o stat= -p "$1" 2>/dev/null); case "$_af_pin_stat" in ""|Z*|X*|x*) exit 0;; esac; sleep 1; _af_pin_i=$((_af_pin_i + 1)); done; else sleep 86400; fi`

// processAlive reports whether pid currently names a RUNNING process. It reads
// the process table rather than kill(pid, 0): signal-0 succeeds on a ZOMBIE,
// and the owner an ExitWhenOrphaned watchdog watches can be exactly that — a
// fixture that booted after its spawner died gets reparented before its first
// check, so only the expected-pid arm can stop it, and a kill-0 arm would hold
// it alive over a corpse the spawner's parent has not collected (#4417
// review). Lookup reports zombies and gone processes alike as exited, and any
// unreadable pid collapses to dead — the safe direction for a watchdog, which
// loses a test process to a false positive but never leaks one to a false
// negative (the #4412 failure shape).
func processAlive(pid int) bool {
	_, err := proctree.Lookup(pid)
	return err == nil
}

// KillProcessGroupOnCleanup registers a t.Cleanup that SIGKILLs process
// group pgid. For fixture processes the test already put in their own
// process group — or that made themselves group leaders, e.g. under
// systemd-run --scope — where the only handle the harness has is the pgid.
func KillProcessGroupOnCleanup(t testing.TB, pgid int) {
	t.Helper()
	t.Cleanup(func() {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Logf("kill fixture process group %d: %v", pgid, err)
		}
	})
}

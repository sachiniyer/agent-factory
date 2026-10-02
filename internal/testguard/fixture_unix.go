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
// The group kill also ORPHANS those mid-flight grandchildren, so the test
// process is marked a child subreaper for the test's duration and the
// cleanup reaps the reparented dead after the kill: on a containerized
// runner whose PID 1 never collects, each killed grandchild would otherwise
// sit as a permanent zombie under it (#4417 review). Platforms without a
// subreaper mechanism no-op both halves — their init does reap.
//
// The returned cmd is started; callers may still Wait on it themselves — the
// cleanup reap is a harmless no-op then — and should not Setpgid twice.
func StartGroupProcess(t testing.TB, cmd *exec.Cmd) *exec.Cmd {
	t.Helper()
	// Adopt the group's orphans BEFORE any can exist: reparenting only lands
	// on a subreaper that was marked before the orphan's parent died, so this
	// must precede the fixture's first child, not the kill that orphans them.
	becomeOrphanReaper(t)
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
	//
	// The watch is bound to the owner's process instance, not its slot: a
	// bare pid is reissued once the owner dies, and the live replacement
	// would hold this pin for the whole bound — the same reuse race
	// ExpectedOwnerStartEnv closes for the Go watchdog, closed here by
	// passing the spawner's proctree StartID as $2 (#4417 review). The ps
	// arm cannot read a StartID, so it gets the owner's start SECOND as $3 —
	// the granularity ps -o lstart itself reports — and validates it every
	// poll rather than self-binding to whatever pid reuse put in the slot
	// before the first check. A platform where either stamp cannot be read
	// passes an empty argument and that arm degrades to its fallback.
	pinStart, pinStartEpoch := "", ""
	if self, err := proctree.Lookup(os.Getpid()); err == nil && self.StartID != 0 {
		pinStart = strconv.FormatUint(self.StartID, 10)
		if !self.StartedAt.IsZero() {
			pinStartEpoch = strconv.FormatInt(self.StartedAt.Unix(), 10)
		}
	}
	pin := exec.Command("sh", "-c", groupPinScript, "af-testguard-pin",
		strconv.Itoa(os.Getpid()), pinStart, pinStartEpoch)
	pin.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: cmd.Process.Pid}
	if err := pin.Start(); err != nil {
		// The leader is already running and no cleanup for it is registered
		// yet — a bare Fatalf here strands the indefinitely-blocking fixture
		// this helper exists to contain, in exactly the failure conditions
		// (a transient fork/exec refusal) that produce one. Kill the group
		// and collect the leader before failing; the pin never started, so
		// nothing else holds the id. reapGroupOrphans runs for the same
		// reason as in the normal cleanup: a leader that already forked a
		// child leaves that grandchild orphaned to this process under the
		// subreaper mark, and collecting only the leader strands it as a
		// zombie for a non-reaping container PID 1 (#4417 review).
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		reapGroupOrphans(t, cmd.Process.Pid)
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
	// reapGroupOrphans sits between the kill and the Waits so the grandchildren
	// the kill just orphaned are collected while the group's members are
	// confirmed dead — the Waits then no-op on whatever the drain took first.
	t.Cleanup(func() { _, _ = pin.Process.Wait() })
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })
	t.Cleanup(func() { reapGroupOrphans(t, cmd.Process.Pid) })
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

// groupPinScript is the pin process's body. $1 is the pid it must outlive —
// its spawner, the test binary — $2 that pid's proctree StartID, and $3 its
// start time in epoch seconds for the ps arm; any is empty when the
// platform could not stamp it. See StartGroupProcess for why a bare
// `sleep 86400` leaks and why the death signal is the owner's process STATE
// rather than the pin's ppid or a kill -0: a zombie owner still answers
// signal-0, and a ppid captured after reparenting names a reaper that never
// dies. Two platform arms, both ending the pin when the owner is gone or
// dead-but-uncollected:
//
//   - /proc (linux): one read yields the state letter AND the starttime —
//     stat field 22 is exactly what proctree reports as StartID — so this
//     arm validates pid AND instance every poll. A reused owner pid shows a
//     different starttime and the pin exits instead of holding the group
//     for a recycled stranger.
//   - ps (darwin and /proc-less boxes): ps reports no StartID, but lstart
//     IS the stamp at second precision — so this arm converts each poll's
//     lstart to epoch seconds and compares it to the passed $3, binding the
//     watch to the recorded owner instead of self-binding to whoever holds
//     the slot at first poll. The residual window is the granularity ps
//     itself reports: a same-second replacement compares equal and holds
//     the pin. An empty $3 or an unconvertible lstart falls back to the
//     old self-bind, and an empty capture degrades to the state check.
//
// Neither arm trusts kill -0 or ppid drift, for the same reasons as before.
// A box with neither /proc nor ps degrades to the bounded sleeper, which
// can still leak for a day but keeps the pgid pin it was bought for. Both
// live arms stay bounded too: the loop is the last-resort ceiling when
// every check keeps passing on an immortal owner.
const groupPinScript = `_af_pin_pid=$1; _af_pin_stamp=$2; _af_pin_epoch=$3; _af_pin_i=0
if [ -r "/proc/$_af_pin_pid/stat" ]; then
	while [ "$_af_pin_i" -lt 86400 ]; do
		_af_pin_raw=$(cat "/proc/$_af_pin_pid/stat" 2>/dev/null) || exit 0
		[ -n "$_af_pin_raw" ] || exit 0
		set -f; set -- ${_af_pin_raw##*)}; set +f
		case "$1" in Z*|X*|x*) exit 0;; esac
		[ -z "$_af_pin_stamp" ] || [ "${20}" = "$_af_pin_stamp" ] || exit 0
		sleep 1; _af_pin_i=$((_af_pin_i + 1))
	done
elif command -v ps >/dev/null 2>&1; then
	_af_pin_id=""
	while [ "$_af_pin_i" -lt 86400 ]; do
		_af_pin_stat=$(ps -o stat= -p "$_af_pin_pid" 2>/dev/null)
		case "$_af_pin_stat" in ""|Z*|X*|x*) exit 0;; esac
		_af_pin_now=$(ps -o lstart= -p "$_af_pin_pid" 2>/dev/null)
		if [ -n "$_af_pin_epoch" ]; then
			_af_pin_secs=$(date -j -f "%a %b %e %T %Y" "$_af_pin_now" +%s 2>/dev/null)
			if [ -n "$_af_pin_secs" ] && [ "$_af_pin_secs" != "$_af_pin_epoch" ]; then
				exit 0
			fi
		elif [ -z "$_af_pin_id" ]; then
			_af_pin_id=$_af_pin_now
		elif [ -n "$_af_pin_now" ] && [ "$_af_pin_now" != "$_af_pin_id" ]; then
			exit 0
		fi
		sleep 1; _af_pin_i=$((_af_pin_i + 1))
	done
else
	sleep 86400
fi`

// processAlive reports whether pid currently names a RUNNING process — the
// instance stamped start when the spawner recorded one, any live process
// when start is 0. It reads the process table rather than kill(pid, 0):
// signal-0 succeeds on a ZOMBIE, and the owner an ExitWhenOrphaned watchdog
// watches can be exactly that — a fixture that booted after its spawner died
// gets reparented before its first check, so only the expected-pid arm can
// stop it, and a kill-0 arm would hold it alive over a corpse the spawner's
// parent has not collected (#4417 review). The stamp matters for the same
// arm: a bare pid names a slot, and once the owner's number is reissued the
// recycled process still answers Lookup — the watchdog would hold the
// fixture to a stranger for as long as the replacement lives, which is the
// leak this arm exists to close. Lookup reports zombies and gone processes
// alike as exited, and any unreadable pid collapses to dead — the safe
// direction for a watchdog, which loses a test process to a false positive
// but never leaks one to a false negative (the #4412 failure shape).
func processAlive(pid int, start uint64) bool {
	p, err := proctree.Lookup(pid)
	if err != nil {
		return false
	}
	return start == 0 || p.StartID == start
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

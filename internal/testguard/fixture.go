// Fixture process lifetime (#4412). A test helper that spins on a file
// sentinel or idles forever used to die only when its test's cleanup ran;
// a test binary that timed out, crashed, or was killed left the helper
// reparented to init, still polling a gate file in a deleted t.TempDir —
// hundreds of them accumulated on one host, the oldest spinning for days.
//
// The helpers here close both halves of that failure. StartGroupProcess and
// KillProcessGroupOnCleanup (fixture_unix.go) put each fixture in its own
// process group and SIGKILL the whole group at cleanup, so a shell that
// forks a fresh `sleep` every iteration loses the current sleeper too.
// The Bounded* builders give every wait loop its own wall-clock ceiling so
// a fixture that IS orphaned self-terminates instead of spinning forever.
// ExitWhenOrphaned covers the remaining case: a fixture that is a re-exec'd
// copy of the test binary dies the moment its real owner does.
package testguard

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

// BoundedSpin returns a POSIX sh fragment that sleeps in a loop until
// lifetime has elapsed, then exits. Use it in place of `while :; do sleep N;
// done` so a fixture that outlives its test still self-terminates (#4412).
// period must be positive; the loop runs lifetime/period iterations rounded
// down, with a minimum of one.
func BoundedSpin(period, lifetime time.Duration) string {
	return fmt.Sprintf("_af_spin_i=0; while [ \"$_af_spin_i\" -lt %d ]; do sleep %s; _af_spin_i=$((_af_spin_i + 1)); done",
		spinIterations(period, lifetime), sleepSeconds(period))
}

// BoundedGatePoll returns a POSIX sh fragment that polls for the gate file
// every period until it appears or lifetime elapses, then re-checks the gate
// so the fragment's exit status reports whether the gate ever appeared.
// Compose it with `|| exit 124` (see BoundedGateWait) or a custom timeout
// action instead of an unbounded `while [ ! -f ... ]` wait.
func BoundedGatePoll(gate string, period, lifetime time.Duration) string {
	q := shellquote.Quote(gate)
	return fmt.Sprintf("_af_gate_i=0; while [ ! -f %s ] && [ \"$_af_gate_i\" -lt %d ]; do sleep %s; _af_gate_i=$((_af_gate_i + 1)); done; [ -f %s ]",
		q, spinIterations(period, lifetime), sleepSeconds(period), q)
}

// BoundedGateWait is BoundedGatePoll followed by `|| exit 124`: a fixture
// whose gate never appears exits with the conventional timeout status
// instead of polling a deleted t.TempDir path forever (#4412). When the gate
// matters mid-script, exit status 124 stops the fixture before it runs
// payload it was only holding open for.
func BoundedGateWait(gate string, period, lifetime time.Duration) string {
	return BoundedGatePoll(gate, period, lifetime) + " || exit 124"
}

// BoundedLoop returns a POSIX sh fragment that runs body once per iteration
// until lifetime/period iterations have run. For fixtures that must keep
// doing something — writing output, spamming a stream — rather than just
// sleeping: same orphan ceiling as BoundedSpin.
func BoundedLoop(period, lifetime time.Duration, body string) string {
	return fmt.Sprintf("_af_loop_i=0; while [ \"$_af_loop_i\" -lt %d ]; do %s; _af_loop_i=$((_af_loop_i + 1)); done",
		spinIterations(period, lifetime), body)
}

// ExpectedParentEnv names the environment variable a spawner uses to tell a
// re-exec'd fixture the pid of the process whose survival keeps it alive —
// the spawner's own os.Getpid() for a direct child, or the TEST's pid for a
// fixture launched through an intermediary (a tmux pane, a supervising
// shell) whose immediate parent is a daemon that outlives the test.
// ExitWhenOrphaned watches it by existence rather than by ppid: a fixture
// can still be booting when its owner dies — especially on a loaded runner —
// and a ppid captured AFTER the reparent is init or a subreaper, a parent
// that never goes away, which leaves a drift-only watchdog watching nothing
// forever.
const ExpectedParentEnv = "AF_TESTGUARD_EXPECTED_PPID"

// ExitWhenOrphaned starts a watchdog that exits the current process once its
// original owner is gone. It exists for fixtures that are re-exec'd copies
// of the test binary (exec.Command(os.Args[0], ...)): the test binary is
// their only legitimate owner, and once they are reparented nothing else
// will ever signal or reap them — which is how `daemon.test --socket`
// fixtures survived for weeks (#4412). Call it at the top of the re-exec'd
// fixture main. poll is the sampling interval.
//
// Two arms, because "owner is gone" has two shapes:
//
//   - the IMMEDIATE parent changed: the process was reparented, which is
//     what an owner's death looks like from inside. This fires on pane-shell
//     and supervisor death too — a tmux-pane fixture whose pane dies has no
//     owner left even when the test that spawned it is alive.
//   - the pid named by ExpectedParentEnv no longer EXISTS: the owner the
//     spawner named is gone outright, reparent or no reparent. This is the
//     arm that covers the intermediary case — a fixture in a detached tmux
//     pane is parented to the tmux server, which outlives the test; without
//     it the ppid arm watches a survivor and a crashed test leaves the
//     fixture running forever.
func ExitWhenOrphaned(poll time.Duration) {
	ppid := os.Getppid()
	expected := 0
	if v, err := strconv.Atoi(os.Getenv(ExpectedParentEnv)); err == nil && v > 0 {
		expected = v
	}
	go func() {
		for {
			time.Sleep(poll)
			if os.Getppid() != ppid {
				os.Exit(0)
			}
			if expected != 0 && !processAlive(expected) {
				os.Exit(0)
			}
		}
	}()
}

func spinIterations(period, lifetime time.Duration) int64 {
	if period <= 0 {
		panic("testguard: fixture loop period must be positive")
	}
	n := int64(lifetime / period)
	if n < 1 {
		n = 1
	}
	return n
}

func sleepSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

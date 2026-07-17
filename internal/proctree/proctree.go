// Package proctree inspects the local process table. It exists so session
// teardown can reap every descendant of a tmux pane (#1104) and so `af doctor`
// can trace leaked processes back to the session that spawned them.
//
// Every operation that signals a process guards against PID reuse by pairing
// the PID with a platform-defined start identifier: a (pid, StartID) pair names
// a process *instance*, not just a slot. A PID that has been recycled since the
// snapshot fails the identity check and is never signalled.
//
// # Platform support
//
// The process table is read through a per-platform backend, selected by build
// tag: proctree_linux.go reads /proc, proctree_darwin.go reads the kernel's
// sysctl process table. A platform with neither gets proctree_other.go, whose
// every entry point fails with ErrUnsupportedPlatform.
//
// That structure is the point of this package's shape, not an accident of it.
// This is a DIAGNOSTIC layer, and the worst thing a diagnostic layer can do is
// answer "nothing found" when the truth is "I cannot look" — a caller cannot
// tell the two apart, so blindness reads as health. Before #1939 this package
// was /proc-only with no build tag: on darwin Snapshot() failed on every call,
// session/tmux/reap.go swallowed the error, and `af doctor` reported a clean
// bill from an empty snapshot. It had been shipping that way to every macOS
// user for as long as we have shipped darwin binaries.
//
// Two rules keep that from coming back, and both are load-bearing:
//
//  1. A platform we cannot read must not COMPILE, rather than silently no-op.
//     proctree_other.go exists to be a loud failure on any future GOOS, and its
//     errors are unconditional — there is no path through it that reports an
//     empty process table as success.
//  2. "I cannot see" must be REPORTABLE, so callers can say it out loud.
//     ErrUnsupportedPlatform is distinguishable with errors.Is, and doctor
//     renders a failed Snapshot as a FAIL row rather than omitting the check
//     (doctor/doctor.go). Never reintroduce a caller that maps a Snapshot error
//     to "no processes".
package proctree

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// ErrUnsupportedPlatform is returned by every read entry point on a platform
// with no process-table backend. Callers that can degrade must report it —
// never treat it as an empty result. Test with errors.Is.
var ErrUnsupportedPlatform = errors.New("proctree: reading the process table is not supported on this platform")

// Process identifies one live process at snapshot time.
type Process struct {
	PID  int
	PPID int
	// StartID is an opaque, platform-defined process start stamp. Together
	// with PID it uniquely identifies a process instance until reboot; it is
	// compared for EQUALITY only and its units differ per platform (Linux:
	// clock ticks since boot, from /proc/<pid>/stat field 22; darwin:
	// microseconds since the epoch, from kinfo_proc's p_starttime). Never do
	// arithmetic on it — use StartedAt for anything time-shaped.
	StartID uint64
	// StartedAt is when the process started, in wall-clock time. Derived by
	// the platform backend so age arithmetic is portable.
	StartedAt time.Time
	// SID is the kernel session id. Every process spawned inside a tmux pane
	// shares the pane root's SID unless it called setsid, so SID membership
	// proves pane ancestry even after a process is reparented to init.
	SID int
	// Comm is the kernel task name (truncated by the kernel: 15 chars on
	// Linux, 16 on darwin).
	Comm string
}

// sidUnknown is the SID of a process whose kernel session id could not be
// read. It is deliberately not 0: 0 is a real value (kernel threads carry it),
// so reusing it would make "I could not tell" indistinguishable from a fact,
// and SessionMembers would then hand every unreadable process to the caller as
// a session member. See SessionMembers.
const sidUnknown = -1

// Snapshot reads the whole process table once. The result is a point-in-time
// view: processes may die (or PIDs be recycled) immediately after. Callers
// must use Signal/AliveSame — which re-verify identity — before acting on an
// entry.
//
// An error means the table could not be READ, which is never the same fact as
// "no processes are running": report it, do not treat it as an empty map.
func Snapshot() (map[int]Process, error) {
	return snapshot()
}

// TreeOf returns root plus every descendant of root present in snap, in BFS
// order (root first). Returns nil when root is not in the snapshot.
func TreeOf(snap map[int]Process, root int) []Process {
	rp, ok := snap[root]
	if !ok {
		return nil
	}
	children := make(map[int][]int, len(snap))
	for pid, p := range snap {
		children[p.PPID] = append(children[p.PPID], pid)
	}
	tree := []Process{rp}
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if seen[c] {
				continue
			}
			seen[c] = true
			tree = append(tree, snap[c])
			queue = append(queue, c)
		}
	}
	return tree
}

// SessionMembers returns every process in snap whose kernel session id is
// sid. Complements TreeOf: a pane descendant that was reparented to init
// (its spawner exited first) drops out of the ppid tree but keeps the pane
// root's SID unless it called setsid.
//
// A non-positive sid matches NOTHING, and that is a safety property rather
// than an edge case. The result of this function flows into KillEscalating
// (session/tmux/reap.go), so "every process whose session id I could not read"
// must never be answerable as a set: sidUnknown and 0 (kernel threads) are
// both excluded, on both platforms.
func SessionMembers(snap map[int]Process, sid int) []Process {
	if sid <= 0 {
		return nil
	}
	var members []Process
	for _, p := range snap {
		if p.SID == sid {
			members = append(members, p)
		}
	}
	return members
}

// AliveSame reports whether the same process instance (matching PID and start
// identifier) is still running.
func AliveSame(p Process) bool {
	cur, err := readProc(p.PID)
	if err != nil {
		return false
	}
	return cur.StartID == p.StartID
}

// ErrIdentityChanged is returned by Signal when the PID no longer names the
// snapshotted process instance (it exited, or the PID was recycled).
var ErrIdentityChanged = errors.New("process exited or pid was recycled")

// kill is the syscall used to deliver signals. It is a package variable only
// so tests can simulate the TOCTOU window (a process reaped between the
// identity check and the signal, making the kernel return ESRCH).
var kill = syscall.Kill

// Signal delivers sig to p only if the PID still names the same process
// instance. The verify-then-kill pair has an unavoidable microsecond TOCTOU
// window; PID recycling within it would require the kernel to cycle through
// the entire PID space between the two syscalls, which does not happen in
// practice. If the process is reaped inside that window, syscall.Kill returns
// ESRCH — indistinguishable from the AliveSame failure path, so it is coerced
// to ErrIdentityChanged and callers treat "already gone" as success.
func Signal(p Process, sig syscall.Signal) error {
	if p.PID <= 1 || p.PID == os.Getpid() {
		return fmt.Errorf("refusing to signal pid %d", p.PID)
	}
	if !AliveSame(p) {
		return ErrIdentityChanged
	}
	if err := kill(p.PID, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ErrIdentityChanged
		}
		return err
	}
	return nil
}

// WaitForExits polls until every process in procs is gone (or its PID was
// recycled — same thing for our purposes) or the timeout elapses, and
// returns the ones still alive.
func WaitForExits(procs []Process, timeout time.Duration) []Process {
	deadline := time.Now().Add(timeout)
	for {
		var alive []Process
		for _, p := range procs {
			if AliveSame(p) {
				alive = append(alive, p)
			}
		}
		if len(alive) == 0 || time.Now().After(deadline) {
			return alive
		}
		procs = alive
		time.Sleep(50 * time.Millisecond)
	}
}

// KillEscalating gives procs the grace period to exit on their own, SIGTERMs
// survivors, waits termWait, SIGKILLs what remains, and returns anything
// still alive after a final bounded wait (should be empty). Every signal is
// identity-verified (see Signal) and reported through logf, one line per
// process. logf may be nil.
func KillEscalating(procs []Process, grace, termWait time.Duration, logf func(format string, args ...any)) []Process {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	survivors := WaitForExits(procs, grace)
	if len(survivors) == 0 {
		return nil
	}
	for _, p := range survivors {
		err := Signal(p, syscall.SIGTERM)
		switch {
		case err == nil:
			logf("reaping leaked process %d (%s) with SIGTERM: %s", p.PID, p.Comm, Cmdline(p.PID))
		case !errors.Is(err, ErrIdentityChanged):
			logf("failed to SIGTERM leaked process %d (%s): %v", p.PID, p.Comm, err)
		}
	}
	survivors = WaitForExits(survivors, termWait)
	for _, p := range survivors {
		err := Signal(p, syscall.SIGKILL)
		switch {
		case err == nil:
			logf("leaked process %d (%s) ignored SIGTERM; sent SIGKILL", p.PID, p.Comm)
		case !errors.Is(err, ErrIdentityChanged):
			logf("failed to SIGKILL leaked process %d (%s): %v", p.PID, p.Comm, err)
		}
	}
	remaining := WaitForExits(survivors, time.Second)
	for _, p := range remaining {
		logf("leaked process %d (%s) survived SIGKILL", p.PID, p.Comm)
	}
	return remaining
}

// ErrCPUUnknown is returned by CPUFraction when the kernel would not report
// the process's CPU time (typically another user's process). It is distinct
// from a zero fraction: "not measurable" and "idle" are different findings,
// and only one of them means the caller may stop looking.
var ErrCPUUnknown = errors.New("proctree: cpu time is not readable for this process")

// CPUFraction returns the process's lifetime-average CPU usage as a fraction
// of one core (1.0 = a full core since it started), plus its age in seconds.
// A brand new process (age < 1s) reports 0 to avoid a noisy division.
//
// The CPU counter is read fresh rather than carried on Process: only doctor
// asks for it, and making it a snapshot field would charge every teardown reap
// for a number it never looks at.
//
// An unreadable counter is an ERROR (ErrCPUUnknown), never a 0.0 fraction.
// Doctor uses this to find processes pegging a core, so "I could not measure"
// and "measured, idle" must not arrive at the caller wearing the same face.
func CPUFraction(p Process) (frac float64, ageSeconds float64, err error) {
	if p.StartedAt.IsZero() {
		// No start time means the backend could not establish boot time (a
		// procfs mounted subset=pid hides /proc/uptime while still serving
		// /proc/<pid>/stat). Wrapped in ErrCPUUnknown so it classifies exactly
		// like a refused counter: unmeasurable, NOT idle.
		return 0, 0, fmt.Errorf("%w: pid %d has no start time, so its age is unknown", ErrCPUUnknown, p.PID)
	}
	age := time.Since(p.StartedAt).Seconds()
	cpu, err := readCPUTime(p.PID)
	if err != nil {
		return 0, age, fmt.Errorf("%w: %v", ErrCPUUnknown, err)
	}
	if age < 1 {
		return 0, age, nil
	}
	return cpu.Seconds() / age, age, nil
}

// ErrEnvUnreadable is returned when a process's environment could not be read.
//
// It means "I was not allowed to look", NEVER "the variable is not set". The
// difference is the whole reason this error exists: see EnvStatus.
var ErrEnvUnreadable = errors.New("proctree: the process environment could not be read")

// EnvStatus is the outcome of an environment probe. There are THREE, not two,
// and the third is the point.
//
// A (value, bool) probe cannot say "I could not tell you". It can only say
// found or not-found, so a DENIED read arrives at the caller wearing the same
// face as a definite negative — and the caller then acts on a fact nobody
// established. That is how a redacted read becomes a deletion: see
// doctor/checks.go's processReferencedHomes, where "no process references this
// home" is the predicate for os.RemoveAll.
//
// This is #1962's shape ("a bool cannot say unknown"), on the platform we just
// gained the ability to test.
type EnvStatus int

const (
	// EnvUnknown means the environment could not be read: denied, redacted,
	// or the process is gone. The variable may or may not be set — we do not
	// know, and neither does the caller.
	//
	// It is the ZERO VALUE deliberately. A forgotten assignment, an
	// unhandled switch arm, or a zero-valued struct field therefore means
	// "unknown" rather than "definitely absent", so the failure mode of
	// sloppiness is caution instead of a fabricated negative.
	EnvUnknown EnvStatus = iota
	// EnvFound means the environment was read and the variable is set.
	EnvFound
	// EnvAbsent means the environment was READ and the variable is
	// definitively not in it. Only this justifies acting on the variable's
	// absence.
	EnvAbsent
)

func (s EnvStatus) String() string {
	switch s {
	case EnvFound:
		return "found"
	case EnvAbsent:
		return "absent"
	default:
		return "unknown"
	}
}

// EnvLookup, OwnerUID and WorkingDir are the three-return / two-return process
// readers #1920's daemon-skew detection depends on (doctor/skew.go). They were
// added straight onto /proc while this file was still Linux-only; here they are
// re-expressed as thin wrappers over the platform-backed primitives, so #1920
// keeps its exact contract AND gains a darwin backend instead of silently
// failing there. Their signatures are unchanged, so skew.go and skew_test.go
// are untouched.

// EnvLookup reads key from a process's initial environment, separating "absent"
// (false, nil) from "could not read" (false, err) — the distinction #1044 turns
// on. It is the (string, bool, error) spelling of LookupEnv, and it inherits the
// same three-way honesty: a redacted or empty (indistinguishable) environment is
// an error, not a definite "unset". Prefer LookupEnv in new code.
func EnvLookup(pid int, key string) (string, bool, error) {
	switch v, st := LookupEnv(pid, key); st {
	case EnvFound:
		return v, true, nil
	case EnvAbsent:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("%w: pid %d", ErrEnvUnreadable, pid)
	}
}

// OwnerUID returns the uid owning pid; false when it cannot be determined. It is
// an alias for UID, kept because skew.go injects it by this name.
func OwnerUID(pid int) (int, bool) {
	return UID(pid)
}

// WorkingDir returns pid's current working directory; false when it cannot be
// read (a foreign process, a kernel thread, an exited process — or a platform
// with no backend for it). The bool is an honest unknown channel, not a
// fabricated fact: skew.go treats false as "I could not resolve a relative home
// in this frame" and skips, never as a positive claim (#1044).
func WorkingDir(pid int) (string, bool) {
	return readWorkingDir(pid)
}

// LookupEnv reads key from the process's initial environment and reports which
// of the three things happened. Callers MUST handle EnvUnknown explicitly and
// must never collapse it into EnvAbsent.
//
// The environment reflects the process's *initial* state — exactly what we want
// for ancestry markers, since a process cannot retroactively lose the marker it
// inherited.
func LookupEnv(pid int, key string) (string, EnvStatus) {
	// Through Environ, so the "did we actually get an answer?" classification
	// lives in exactly one place and this cannot drift from it.
	env, err := Environ(pid)
	if err != nil {
		return "", EnvUnknown
	}
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):], EnvFound
		}
	}
	return "", EnvAbsent
}

// Environ returns the process's initial environment as "KEY=VALUE" strings, or
// an error wrapping ErrEnvUnreadable when we did not get an answer.
//
// # Why an EMPTY environment is an error and not a result
//
// This is the one place that decides whether we were told anything, and it does
// so by looking at what came back rather than by predicting what we are allowed
// to read. That distinction is the whole design.
//
// A withheld environment and an empty one are BYTE-IDENTICAL. darwin's
// sysctl_procargsx omits the env section from the KERN_PROCARGS2 buffer when it
// declines, producing exactly what `env -i cmd` produces, and it does not say
// which happened. So "zero variables" is not a fact about the process — it is
// the absence of an answer, and it must not be handed to a caller as
// "AGENT_FACTORY_HOME is not set". Downstream, that negative is the predicate
// for deleting a directory (doctor's processReferencedHomes → os.RemoveAll).
//
// The obvious alternative — work out up front whether the kernel will serve us
// — is what this replaced, and it was wrong. XNU withholds on at least TWO
// independent grounds (uid mismatch AND cs_restricted, which SIP makes ordinary
// on a real Mac), and a gate modelling only ownership let same-uid restricted
// processes through to be misread. Predicting a policy you do not own means
// reimplementing it, and a reimplementation is wrong as soon as the real policy
// grows a clause — entitlements, hardened runtime, platform binaries are all
// candidates. Classifying the ANSWER collapses every ground, present and
// future, into this single branch, and it cannot rot when Apple adds one.
//
// The cost is precise and acceptable: a process genuinely started with an empty
// environment (`env -i cmd`) reports unknown rather than absent. We say "I
// cannot tell" about a process we truly cannot tell apart from a redacted one.
// That is the honest answer, and the callers that could act on it all treat
// unknown as a reason not to.
func Environ(pid int) ([]string, error) {
	env, err := readEnviron(pid)
	if err != nil {
		return nil, err
	}
	if len(env) == 0 {
		return nil, fmt.Errorf("%w: pid %d returned no environment variables at all, which is "+
			"indistinguishable from an environment the kernel withheld — it does not report which",
			ErrEnvUnreadable, pid)
	}
	return env, nil
}

// UID returns the uid owning pid. The second return is false when ownership
// cannot be determined (the process exited, or the platform will not say),
// which callers MUST treat as "unknown" and never as "ours" — this gates
// whether `af reset` may signal a process.
func UID(pid int) (int, bool) {
	return readUID(pid)
}

// EnvValue is DELETED, deliberately. Do not reintroduce it.
//
// It was `func EnvValue(pid int, key string) (string, bool)`, and the bool was
// the bug: it collapsed "the variable is not set" and "I was not allowed to
// read the environment" into one false. Every caller then treated a denied read
// as a definite negative — including processReferencedHomes, whose negative is
// the predicate for deleting a directory.
//
// A two-valued answer to a three-valued question cannot be used safely, so the
// fix is not a careful caller: it is the removal of the API that made the
// mistake expressible. Use LookupEnv and handle EnvUnknown.

// Argv returns the process's argument vector with argument BOUNDARIES intact,
// or nil when it cannot be read.
//
// The boundaries are the whole point: an install path containing a space
// ("/Users/John Smith/.local/bin/af") is one argv entry, and any source that
// hands back a pre-joined string has already destroyed the only evidence of
// that. Re-splitting such a string on whitespace yields argv[0] = "/Users/John"
// — which is how spaced-install daemon detection broke on darwin (#1942) while
// working on Linux, where /proc/<pid>/cmdline preserved the NULs. Callers that
// classify a process by its binary name MUST use this, never Cmdline.
func Argv(pid int) []string {
	argv, err := readArgv(pid)
	if err != nil {
		return nil
	}
	return argv
}

// Cmdline returns the process's argv joined with spaces, or "" when
// unreadable. For kernel threads (empty argv) it returns "". This is a lossy,
// display-oriented view: it collapses argv boundaries, so a binary path
// containing spaces cannot be recovered from it. Do not use it for
// classification that depends on argv boundaries — use Argv (#1214, #1942).
func Cmdline(pid int) string {
	argv := Argv(pid)
	if len(argv) == 0 {
		return ""
	}
	out := argv[0]
	for _, a := range argv[1:] {
		out += " " + a
	}
	return out
}

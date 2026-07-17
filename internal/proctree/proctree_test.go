package proctree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
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
	child := startSleeper(t)
	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	self, ok := snap[os.Getpid()]
	if !ok {
		t.Fatalf("snapshot missing our own pid %d", os.Getpid())
	}
	if self.StartID == 0 {
		t.Errorf("self StartID = 0, want nonzero")
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
	snap := map[int]Process{2: {PID: 2, PPID: 1}}
	if tree := TreeOf(snap, 999999); tree != nil {
		t.Errorf("TreeOf(missing root) = %v, want nil", tree)
	}
}

func TestSessionMembers(t *testing.T) {
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
	child := startSleeper(t)
	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	p := snap[child.Process.Pid]

	// Wrong start time must refuse to signal.
	stale := p
	stale.StartID++
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
	if err := Signal(Process{PID: 1}, syscall.SIGTERM); err == nil {
		t.Errorf("Signal(pid 1) succeeded, want refusal")
	}
	if err := Signal(Process{PID: os.Getpid()}, syscall.SIGTERM); err == nil {
		t.Errorf("Signal(self) succeeded, want refusal")
	}
}

func TestLookupEnv(t *testing.T) {
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
		if got, st := LookupEnv(cmd.Process.Pid, "AF_TEST_MARKER"); st == EnvFound && got == "hello" {
			break
		}
		if time.Now().After(deadline) {
			got, st := LookupEnv(cmd.Process.Pid, "AF_TEST_MARKER")
			t.Fatalf("LookupEnv = (%q, %v), want (\"hello\", found)", got, st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A variable that is not set, in an environment we CAN read, is
	// definitively absent — the one case that justifies acting on absence.
	if _, st := LookupEnv(cmd.Process.Pid, "AF_TEST_ABSENT"); st != EnvAbsent {
		t.Errorf("LookupEnv(absent var) = %v, want absent", st)
	}
}

func TestCPUFractionIdleSleeper(t *testing.T) {
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

// TestUIDMatchesOurOwn proves this platform's ownership read actually works.
//
// It is deliberately an assertion and not a nicety: daemon/stopall.go compares
// this against os.Getuid() to decide whether `af reset` may SIGTERM a process.
// A backend that returned a wrong-but-plausible uid would make af either skip
// its own stale daemons forever or, far worse, mistake someone else's daemon
// for its own. On darwin the value comes from kinfo_proc's p_ruid, and this is
// what proves that field is populated rather than trusting that it is.
func TestUIDMatchesOurOwn(t *testing.T) {
	uid, ok := UID(os.Getpid())
	if !ok {
		t.Fatalf("UID(self) reported unknown — af reset cannot verify ownership on this platform, " +
			"so it will treat every daemon as unverifiable and stop none of them (#1939)")
	}
	if uid != os.Getuid() {
		t.Errorf("UID(self) = %d, want %d — a wrong uid makes `af reset` misclassify daemon ownership", uid, os.Getuid())
	}
}

// TestUIDOfDeadProcessIsUnknown pins the failure value. "Unknown" must not be
// answerable as uid 0, or a dead pid would read as root-owned.
func TestUIDOfDeadProcessIsUnknown(t *testing.T) {
	// A pid that cannot exist: the kernel caps pids well below this.
	if uid, ok := UID(1 << 30); ok {
		t.Errorf("UID(nonexistent pid) = (%d, true), want unknown", uid)
	}
}

// TestEnvironDistinguishesAbsentFromUnreadable locks the distinction
// daemon/stopall.go's daemonHomeEnv depends on: a readable environment that
// simply lacks the key is NOT the same fact as an unreadable one. Conflating
// them makes `af reset` either skip the stale daemon it exists to kill, or
// guess at a home it cannot see.
func TestEnvironDistinguishesAbsentFromUnreadable(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	cmd.Env = append(os.Environ(), "AF_TEST_MARKER=hello")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Readable: nil error, and the marker is present.
	deadline := time.Now().Add(5 * time.Second)
	var env []string
	for {
		var err error
		env, err = Environ(cmd.Process.Pid)
		if err == nil && len(env) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Environ(child) never became readable: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	found := false
	for _, kv := range env {
		if kv == "AF_TEST_MARKER=hello" {
			found = true
		}
	}
	if !found {
		t.Errorf("Environ(child) did not contain the marker we set; got %d vars", len(env))
	}

	// Unreadable: an error, NOT an empty-but-successful result.
	if env, err := Environ(1 << 30); err == nil {
		t.Errorf("Environ(nonexistent pid) = (%v, nil), want an error — an unreadable environment "+
			"must never look like an empty one", env)
	}
}

// foreignPID returns the pid of a process owned by a DIFFERENT user, or skips.
//
// It SEARCHES for one rather than assuming pid 1 will do. An earlier version
// hardcoded pid 1 on the reasoning that init/launchd is always root's — true on
// a normal machine, false in our own test container, where pid 1 is the
// entrypoint running as the same unprivileged user as the tests. The assertion
// then failed for a reason that had nothing to do with what it tests.
//
// Which is this PR's own bug class aimed at its own tests: an environment
// assumption that holds where it was written and nowhere else. So: find a real
// foreign process, and if the environment genuinely has none (a container with
// a single uid), say so and skip instead of inventing a conclusion.
func foreignPID(t *testing.T) int {
	t.Helper()
	self := os.Getuid()
	if self == 0 {
		t.Skip("running as root: root can read every environment, so there is no refusal to observe")
	}
	snap, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	pids := make([]int, 0, len(snap))
	for pid := range snap {
		pids = append(pids, pid)
	}
	sort.Ints(pids) // deterministic pick across runs
	for _, pid := range pids {
		if uid, ok := UID(pid); ok && uid != self {
			return pid
		}
	}
	t.Skip("no process owned by another user is visible here (a container running everything as one " +
		"uid), so the kernel has no reason to refuse us and the refusal cannot be observed")
	return 0
}

// TestLookupEnvOfForeignProcessIsUnknown exercises the REAL refusal against a
// real foreign process, on both platforms — no simulation.
//
// The two platforms refuse differently and that is exactly the point: Linux
// answers EACCES, while darwin hands back a buffer with argv and NO env —
// indistinguishable from `env -i`. Both must arrive here as EnvUnknown.
//
// If this ever returns EnvAbsent, af is asserting "this variable is not set"
// about a process it was not allowed to read, and the callers that delete
// directories and signal processes are acting on it.
func TestLookupEnvOfForeignProcessIsUnknown(t *testing.T) {
	pid := foreignPID(t)
	// PATH is set for essentially every process, so EnvAbsent here could only
	// mean we mistook a refusal for a fact.
	if _, st := LookupEnv(pid, "PATH"); st != EnvUnknown {
		t.Errorf("LookupEnv(pid %d, PATH) = %v, want unknown: that process belongs to another user and "+
			"its environment is not ours to read, so any definite answer is fabricated", pid, st)
	}
}

// TestEnvironOfForeignProcessErrors is the same fact through Environ, and it
// must wrap ErrEnvUnreadable so callers can tell "denied" from "empty".
func TestEnvironOfForeignProcessErrors(t *testing.T) {
	pid := foreignPID(t)
	env, err := Environ(pid)
	if err == nil {
		t.Fatalf("Environ(pid %d) = %q, nil — want an error; a refused read must not look like a result", pid, env)
	}
	if !errors.Is(err, ErrEnvUnreadable) {
		t.Errorf("Environ(pid %d) error = %v, want it to wrap ErrEnvUnreadable so callers can classify it", pid, err)
	}
}

// TestLookupEnvOfOwnProcessIsAuthoritative is the other half: for a process we
// DO own, both answers must be definite. If the guard above were too broad it
// would make every read unknown, and af would stop being able to identify its
// own orphans at all — a silent, total loss of the capability.
func TestLookupEnvOfOwnProcessIsAuthoritative(t *testing.T) {
	// PATH, not a t.Setenv marker: this reads the process's INITIAL
	// environment, which is fixed at exec and cannot be changed from inside
	// (that immutability is exactly why ancestry markers are trustworthy). A
	// t.Setenv here would not appear and the test would fail for a reason that
	// has nothing to do with what it asserts.
	if _, ok := os.LookupEnv("PATH"); !ok {
		t.Skip("no PATH in this environment; nothing definite to assert against")
	}
	if v, st := LookupEnv(os.Getpid(), "PATH"); st != EnvFound || v == "" {
		t.Errorf("LookupEnv(self, PATH) = (%q, %v), want a value and found", v, st)
	}
	if _, st := LookupEnv(os.Getpid(), "AF_TEST_DEFINITELY_NOT_SET_XYZ"); st != EnvAbsent {
		t.Errorf("LookupEnv(self, unset var) = %v, want absent — our own environment IS readable, "+
			"so an unset variable is a fact we may act on", st)
	}
}

// TestEmptyEnvironIsUnknownNotEmpty is the second P1's regression, and it is
// the whole structural fix in one assertion.
//
// A process started with `env -i` has NO environment variables. On darwin, a
// process whose environment the kernel WITHHELD produces the identical buffer —
// argv, no env section — and the kernel does not report which happened. So zero
// variables cannot be handed back as a fact.
//
// This drives the real thing on any platform: `env -i` is a genuinely empty
// environment, which is byte-for-byte what a redacted read returns. If this ever
// reports EnvAbsent, then every ground darwin withholds on — uid mismatch,
// cs_restricted (which SIP makes ORDINARY, and which the first fix's permission
// gate did not model), entitlements, whatever Apple adds next — arrives at
// callers as "this variable is definitely not set", and doctor deletes a home on
// the strength of it.
func TestEmptyEnvironIsUnknownNotEmpty(t *testing.T) {
	// env -i: no variables at all, but a live process we own — so no permission
	// rule of any kind is in play. The ONLY thing making this unknown is that
	// the answer is indistinguishable from a withheld one.
	cmd := exec.Command("env", "-i", "sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting env -i sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		// Wait for the exec: pre-exec the child still shows OUR environment.
		if argv := Argv(cmd.Process.Pid); len(argv) > 0 && argv[0] == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			t.Skipf("the env -i child never exec'd into sleep; cannot pose the question here")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, st := LookupEnv(cmd.Process.Pid, "AGENT_FACTORY_HOME"); st != EnvUnknown {
		t.Errorf("LookupEnv(env -i process) = %v, want unknown: a zero-variable environment is "+
			"byte-identical to one the kernel withheld, so it is the ABSENCE OF AN ANSWER and must "+
			"never be reported as a definite negative", st)
	}
	if env, err := Environ(cmd.Process.Pid); err == nil {
		t.Errorf("Environ(env -i process) = %q, nil — want ErrEnvUnreadable", env)
	} else if !errors.Is(err, ErrEnvUnreadable) {
		t.Errorf("Environ(env -i process) error = %v, want it to wrap ErrEnvUnreadable", err)
	}
}

// TestEnvironDoesNotPredictPermission locks the STRUCTURE, not a behaviour: the
// classification must come from the answer, never from a model of the kernel's
// policy.
//
// This exists because the first fix modelled XNU's rule (uid + P_SUGID) and was
// wrong the moment a ground it did not know about — cs_restricted — walked
// through it. Any reintroduced predictive gate would have to answer "am I
// allowed?" BEFORE reading, and would therefore be wrong again the next time
// Apple adds a clause. If you are here because this test failed: do not add the
// missing clause. Delete the prediction.
//
// The observable proxy: a process we own, with a non-empty environment, must be
// readable — and a process whose answer is empty must be unknown — with no
// reference to WHY in either direction.
func TestEnvironDoesNotPredictPermission(t *testing.T) {
	// Ours, non-empty: an answer, so a definite result.
	if _, err := Environ(os.Getpid()); err != nil {
		t.Errorf("Environ(self) = %v, want our own non-empty environment to be readable", err)
	}
	// Nonexistent: no answer, so unknown — and NOT because we predicted a rule.
	if _, st := LookupEnv(1<<30, "PATH"); st != EnvUnknown {
		t.Errorf("LookupEnv(nonexistent pid) = %v, want unknown", st)
	}
}

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
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// shSingleQuote wraps s in single quotes for safe embedding in a `bash -c`
// script, escaping any embedded single quotes. Used to build the fake-daemon
// spawn recipe without shell-injection surprises when argv0 contains spaces.
func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// spawnFakeDaemonProc launches a long-lived process whose /proc/<pid>/cmdline
// exposes REAL argv boundaries: argv[0] == argv0 (which may itself contain
// spaces, e.g. "/home/John Smith/.local/bin/af"), followed by extraArgs as
// distinct argv elements. This is the shape the daemon detection path
// (daemonArgs → argsAreDaemonBinary/argsHaveDaemonFlag) must classify, and the
// only shape that exercises spaced-path parsing (#1214): the older
// `exec -a 'af --daemon af-test' sleep 60` recipe jammed everything into a
// single argv[0], so it could never test real boundary handling.
//
// It returns once the crafted argv is visible, and no further: a caller that
// classifies or binds the fake BY HOME (StopDaemon's PID binding,
// classifyDaemonHome) must use spawnFakeDaemonHomeProc, which also waits for the
// environ. Keeping the environ out of this one means an argv-only caller never
// depends on environ access (#6195).
//
// The underlying program is bash running `script`. `script` MUST be a compound
// command (contain a `;`, a loop, etc.) so bash's single-simple-command exec
// optimization does not replace the crafted argv with the child's. The process
// and any children share a new process group so cleanup can reap the whole tree
// — bash dies by the test's SIGTERM/SIGKILL, orphaning any `sleep` child in the
// group.
func spawnFakeDaemonProc(t *testing.T, argv0, script string, extraArgs ...string) *exec.Cmd {
	t.Helper()
	cmd := fakeDaemonCmd(t, argv0, script, extraArgs...)
	testguard.StartGroupProcess(t, cmd)
	waitForFakeDaemonArgv(t, cmd, argv0)
	return cmd
}

// spawnFakeDaemonHomeProc is spawnFakeDaemonProc for a caller that classifies the
// fake by home: it returns only once the fake is provable by the SAME
// home/identity predicate the code under test uses, or skips the test when
// this host will not let the test read the environ that predicate needs. See
// waitForFakeDaemon.
func spawnFakeDaemonHomeProc(t *testing.T, argv0, script string, extraArgs ...string) *exec.Cmd {
	t.Helper()
	cmd := fakeDaemonCmd(t, argv0, script, extraArgs...)
	testguard.StartGroupProcess(t, cmd)
	waitForFakeDaemon(t, cmd, argv0)
	return cmd
}

// fakeDaemonCmd builds (but does not start) the fake-daemon command described
// by spawnFakeDaemonProc. It is split out so a caller that must set the child's
// ENVIRONMENT before it execs can do so: AF-home scoping reads a daemon's
// AGENT_FACTORY_HOME from /proc/<pid>/environ, which is fixed at exec and
// cannot be set afterwards.
func fakeDaemonCmd(t *testing.T, argv0, script string, extraArgs ...string) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	// Build: exec -a <argv0> bash -c <script> <argv0> <extraArgs...>
	// The resulting bash process argv is [<argv0>, "-c", <script>, <argv0>, extraArgs...].
	parts := []string{"exec", "-a", shSingleQuote(argv0), "bash", "-c", shSingleQuote(script), shSingleQuote(argv0)}
	for _, a := range extraArgs {
		parts = append(parts, shSingleQuote(a))
	}
	cmd := exec.Command("bash", "-c", strings.Join(parts, " "))
	// Put the process (and its children) in their own group so cleanup can reap
	// the whole tree — bash dies by signal, orphaning any `sleep` child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// fakeDaemonHomeKey is the variable daemon scoping reads from a process's environ.
const fakeDaemonHomeKey = "AGENT_FACTORY_HOME"

// waitForFakeDaemon blocks until cmd's fake daemon can be classified BY HOME:
// its argv is the one it was built to expose, its environ is readable and
// agrees with the environment it was started with, AND the home/identity
// predicate the code under test runs on it (classifyDaemonHome for
// StopDaemon and the sigterm fallback, verifyScopedDaemon for the reset scan)
// has reached a DECISIVE verdict — daemonOurs or daemonForeign — bounded by a
// deadline that fails the test clearly if it lapses. A test that classifies
// or binds the fake by home must wait here (never on argv alone). A test that
// reads only argv waits on waitForFakeDaemonArgv and never touches the
// environ (#6195).
//
// argv and environ are separate /proc reads with no ordering guarantee between
// them. A fake can show its crafted argv while its environ is still empty or
// refused, and the classifier is deliberately fail-closed on that: an environ it
// could not read is daemonUnverifiable, never foreign, so the test then fails on
// the setup's timing rather than on the code under test (#6129, and again in
// TestStopDaemon_SIGTERMFirst: "PID could not be bound to this home").
//
// The argv half also matters. spawnFakeDaemonProc runs `bash -c "exec -a <argv0>
// bash ..."`, so the outer bash has a readable argv — ["bash", "-c", ...] — for a
// moment BEFORE it execs into the crafted one. Classify it in that window and
// argsAreDaemonBinary sees "bash" and writes the daemon off as foreign: an
// assertion that should fail passes, and one that should pass fails, for a reason
// that has nothing to do with the code under test.
//
// The environ half has a second window neither wait above covers: for a moment
// after exec the child is still rewriting its initial environ block in place —
// bash's startup variable pass temporarily NULs the '=' in a "KEY=VALUE" entry,
// so AGENT_FACTORY_HOME that already read back correctly can read ABSENT on the
// very next read. The classifier reads that as a default-home daemon, and with
// no $HOME to resolve the default against it is daemonUnverifiable — the
// fail-closed verdict the code under test then gets blamed for (#6705:
// TestSigtermFallback_AmbiguousCandidates signaling only one of two fakes,
// TestStopDaemon_ForeignHomePIDFileNotKilled erroring "could not be bound to
// this home"). A first good environ read does not prove the block has settled;
// waitForFakeDaemonProvable polls the predicate itself until it decides.
//
// When the environ stays unreadable (EnvUnknown) the test is SKIPPED with the
// reason, not failed: on a SIP-enabled Mac with Apple's /bin/bash, this binary can
// read its own environ but not the SIP-protected bash child's, so no amount of
// waiting helps and the failure would say nothing about the code under test.
//
// The expected home is read from cmd.Env (the whole environment the child was
// given), or from this process's environment when cmd.Env is nil. A child started
// without AGENT_FACTORY_HOME must read back EnvAbsent, i.e. an environ that was
// READ and lacks it, not one that was refused. On a platform where this process
// cannot read even its own environ there is nothing to wait for, and the wait is
// skipped.
func waitForFakeDaemon(t *testing.T, cmd *exec.Cmd, argv0 string) {
	t.Helper()
	waitForFakeDaemonArgv(t, cmd, argv0)
	pid := cmd.Process.Pid

	if _, own := proctree.LookupEnv(os.Getpid(), fakeDaemonHomeKey); own == proctree.EnvUnknown {
		return
	}
	environ := cmd.Env
	if environ == nil {
		environ = os.Environ()
	}
	want, wantSet := fakeDaemonEnv(environ, fakeDaemonHomeKey)
	ready := func() (string, proctree.EnvStatus, bool) {
		got, status := proctree.LookupEnv(pid, fakeDaemonHomeKey)
		if wantSet {
			return got, status, status == proctree.EnvFound && got == want
		}
		return got, status, status == proctree.EnvAbsent
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := ready(); ok {
			waitForFakeDaemonProvable(t, pid, environ)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, status, ok := ready()
	if !ok && status == proctree.EnvUnknown {
		t.Skipf("cannot read fake daemon pid %d's environ although this process can read its own "+
			"(SIP-protected bash on macOS?); a test that classifies by home needs it", pid)
	}
	if !ok {
		t.Fatalf("fake daemon pid %d never exposed a readable environ with %s=%q set=%v (got %q, status %v)",
			pid, fakeDaemonHomeKey, want, wantSet, got, status)
	}
}

// fakeDaemonEnv returns the value environ binds key to — the last "key=value"
// wins, the way exec applies duplicates — and whether the key is set at all.
func fakeDaemonEnv(environ []string, key string) (string, bool) {
	want, ok := "", false
	prefix := key + "="
	for _, kv := range environ {
		if v, found := strings.CutPrefix(kv, prefix); found {
			want, ok = v, true
		}
	}
	return want, ok
}

// waitForFakeDaemonProvable blocks until pid is provable by the SAME predicate
// the code under test binds it with — the uid, environ, and path/frame reads
// classifyDaemonHome (StopDaemon, the sigterm fallback's recorded-PID handling)
// or verifyScopedDaemon (the reset scan) performs — not merely until one
// environ read looks right. See waitForFakeDaemon for why a single good read
// is not proof: the child rewrites its exec-time environ in place during bash
// startup, so the settle signal is the rewrite being done, which a child of
// the fake in /proc/<pid>/task/<pid>/children marks (bash finishes variable
// init before it runs the script's first command, and every fake recipe opens
// with `sleep`). The environ check is re-verified on every poll so a settled
// read is never mistaken for a mid-rewrite one.
//
// Env spellings the recorded-PID classifier can never decide — a bare env with
// no $HOME to expand the default in, a raw "~user", or a home that is (or
// links into) a process-relative procfs path — are daemonUnverifiable by
// DESIGN, not by an unresolved read, so waiting on classifyDaemonHome there
// would only burn the deadline. Those fakes' consumers (the reset-scan tests)
// bind with verifyScopedDaemon instead, which resolves the same env against
// the CALLER's home; for every shape config.ConfigDirFor can resolve, this
// waits on that predicate, which exercises the same uid/environ reads. A raw
// "~user" resolves under neither — there is nothing left to prove.
//
// The wait is bounded by testSpawnReadyTimeout and fails the test with the
// last verdicts and env if it lapses — a fake the code under test cannot
// classify is a broken fixture, not a passing one.
func waitForFakeDaemonProvable(t *testing.T, pid int, environ []string) {
	t.Helper()
	env, envSet := fakeDaemonEnv(environ, fakeDaemonHomeKey)
	envOK := func() bool {
		got, status := proctree.LookupEnv(pid, fakeDaemonHomeKey)
		if envSet {
			return status == proctree.EnvFound && got == env
		}
		return status == proctree.EnvAbsent
	}
	ready := func() bool {
		if !envOK() || !fakeDaemonPastEnvScan(pid) {
			return false
		}
		if fakeDaemonHomeIndecidable(pid, env, environ) {
			if _, err := config.ConfigDirFor(env); err != nil {
				// The env VALUE pins daemonUnverifiable for every predicate
				// (raw "~user"): argv+environ proof is the whole readiness.
				return true
			}
			return verifyScopedDaemon(pid, os.Getuid(), "") != daemonUnverifiable
		}
		return classifyDaemonHome(pid) != daemonUnverifiable
	}
	deadline := time.Now().Add(testSpawnReadyTimeout)
	for {
		if ready() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake daemon pid %d (%s=%q) never became provable under the "+
				"home/identity predicate in %s: last classifyDaemonHome=%v, "+
				"verifyScopedDaemon=%v — the uid/environ/path reads the code "+
				"under test performs on it never all resolved",
				pid, fakeDaemonHomeKey, env, testSpawnReadyTimeout,
				classifyDaemonHome(pid), verifyScopedDaemon(pid, os.Getuid(), ""))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeDaemonHomeIndecidable reports whether classifyDaemonHome's verdict on a
// fake spawned with this AGENT_FACTORY_HOME is pinned to daemonUnverifiable by
// the VALUE itself, so that waiting for a decisive verdict from it would only
// burn the deadline: a default or tilde home the child has no $HOME to expand
// in, a raw "~user" spelling, or a home that is (or symlinks into) a
// process-relative procfs path. This mirrors the env-shape arms inside
// classifyDaemonHome; the per-process reads that can still race (uid and the
// environ itself) are proven through verifyScopedDaemon instead.
func fakeDaemonHomeIndecidable(pid int, env string, environ []string) bool {
	switch {
	case env == "" || env == "~" || strings.HasPrefix(env, "~/"):
		// The default and tilde arms resolve through the child's own $HOME.
		home, ok := fakeDaemonEnv(environ, "HOME")
		return !ok || home == ""
	case strings.HasPrefix(env, "~"):
		return true // a raw "~user" is rejected unconditionally
	}
	resolved := env
	if !filepath.IsAbs(resolved) {
		// A relative home resolves through the child's cwd, which is itself
		// one of the reads that can still race — call it decidable and let
		// the predicate wait cover it.
		cwd, ok := proctree.WorkingDir(pid)
		if !ok || cwd == "" {
			return false
		}
		resolved = filepath.Join(cwd, env)
	}
	return isProcessRelativeProcfsHome(resolved) || homeSymlinkEntersProcessRelativeProcfs(resolved)
}

// fakeDaemonPastEnvScan reports whether the fake's bash has finished rewriting
// its initial environ. During startup bash writes its variable table over the
// exec-time block in place — a "KEY=VALUE" entry can transiently read back as
// a bare "KEY" plus a bare value while it does — so one environ read that saw
// the right value is no proof the block has settled. The rewrite happens in
// initialize_shell_variables, before bash runs the script's first command;
// every fake recipe's script opens with `sleep`, so a child in
// /proc/<pid>/task/<pid>/children marks init complete. Where that file cannot
// be read (no /proc, an older kernel) there is nothing better than the polling
// itself, so this returns true.
func fakeDaemonPastEnvScan(pid int) bool {
	children, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return true
	}
	return len(strings.Fields(string(children))) > 0
}

// waitForFakeDaemonArgv blocks until cmd's process has exec'd into the crafted
// argv (argv[0] == argv0). It reads nothing but cmdline, so it works wherever the
// argv is visible, whether or not the environ is.
func waitForFakeDaemonArgv(t *testing.T, cmd *exec.Cmd, argv0 string) {
	t.Helper()
	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if args := daemonArgs(pid); len(args) > 0 && args[0] == argv0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if args := daemonArgs(pid); len(args) == 0 || args[0] != argv0 {
		t.Fatalf("fake daemon pid %d never exec'd into argv0 %q (argv=%q)", pid, argv0, daemonArgs(pid))
	}
}

package daemon

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

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
// fake by home: it returns only once the environ is readable too, or skips the
// test when this host will not let the test read it. See waitForFakeDaemon.
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

// waitForFakeDaemon blocks until cmd's fake daemon can be classified BY HOME: its
// argv is the one it was built to expose AND its environ is readable and agrees
// with the environment it was started with. A test that classifies or binds the
// fake by home (classifyDaemonHome, StopDaemon's PID binding) must wait here
// (never on argv alone). A test that reads only argv waits on
// waitForFakeDaemonArgv and never touches the environ (#6195).
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
	want, wantSet := "", false
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == fakeDaemonHomeKey {
			want, wantSet = v, true
		}
	}
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

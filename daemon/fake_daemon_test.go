package daemon

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
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
// The underlying program is bash running `script`. `script` MUST be a compound
// command (contain a `;`, a loop, etc.) so bash's single-simple-command exec
// optimization does not replace the crafted argv with the child's. The process
// and any children share a new process group so cleanup can reap the whole tree
// — bash dies by the test's SIGTERM/SIGKILL, orphaning any `sleep` child in the
// group.
func spawnFakeDaemonProc(t *testing.T, argv0, script string, extraArgs ...string) *exec.Cmd {
	t.Helper()
	cmd := fakeDaemonCmd(t, argv0, script, extraArgs...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon proc: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
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

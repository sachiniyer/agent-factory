package session

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// #1967: exec.CommandContext kills only the DIRECT child, not its descendants. A
// descendant that inherits the CombinedOutput()/Output() capture pipe holds it
// open, so without cmd.WaitDelay the call blocks on pipe EOF long past the
// context deadline — the deadline is decorative and a wedged docker daemon could
// hang the container reap (dockerReapTimeout) forever. These tests drive the REAL
// production paths (dockerExec, originRemoteURL) through a fake CLI on PATH that
// backgrounds a straggler holding the pipe, and assert the call returns within
// dockerStragglerGuard rather than waiting the straggler out.
//
// Observed failing before the dockerWaitDelay fix: without it the call returns
// only when the straggler dies (dockerStragglerSleep), tripping the guard. The
// fix must also treat the resulting exec.ErrWaitDelay as success — docker/git
// exited 0 — which the output/URL assertions verify (getting that backwards
// would fail a healthy `docker rm -f` and report a leaked container, #1966).
const (
	// dockerStragglerSleep must exceed dockerStragglerGuard so a missing WaitDelay
	// is a guard failure, not a slow pass; cleanup kills it well before it ends.
	dockerStragglerSleep = 30
	// dockerStragglerGuard sits between the 2s dockerWaitDelay and the straggler
	// sleep, with ample slack over 2s so a loaded box never flakes the fixed path.
	dockerStragglerGuard = 8 * time.Second
)

func TestDockerExec_WaitDelayBoundsStraggler(t *testing.T) {
	installExecStragglerFake(t, "docker", "container-reaped", false)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	var (
		out []byte
		err error
	)
	// Real production path: dockerExec is the single seam every docker invocation
	// (including the `docker rm -f` reap) flows through. The 400ms ctx proves the
	// deadline does not bound the call — only dockerWaitDelay does.
	if !returnsWithinExec(dockerStragglerGuard, func() { out, err = dockerExec(ctx, "rm", "-f", "some-container") }) {
		t.Fatalf("dockerExec did not return within %s — the ctx deadline does not bound CombinedOutput() while a straggler holds the capture pipe; dockerWaitDelay is missing (#1967)", dockerStragglerGuard)
	}
	if err != nil {
		t.Fatalf("dockerExec returned error (a bare exec.ErrWaitDelay must normalize to success — docker exited 0): %v", err)
	}
	if !strings.Contains(string(out), "container-reaped") {
		t.Fatalf("dockerExec output %q missing the captured line — the straggler-held pipe must not lose already-written output", out)
	}
}

func TestOriginRemoteURL_WaitDelayBoundsStraggler(t *testing.T) {
	const url = "https://origin.example/repo.git"
	installExecStragglerFake(t, "git", url, false)

	var got string
	if !returnsWithinExec(dockerStragglerGuard, func() { got = originRemoteURL(t.TempDir()) }) {
		t.Fatalf("originRemoteURL did not return within %s — Output() is blocked on a straggler holding the capture pipe past the deadline; dockerWaitDelay is missing (#1967)", dockerStragglerGuard)
	}
	if got != url {
		t.Fatalf("originRemoteURL = %q, want %q — a bare exec.ErrWaitDelay must be treated as success (git exited 0) rather than dropped to \"\"", got, url)
	}
}

// installExecStragglerFake writes an executable named `name` onto PATH that
// prints line (to stdout, or stderr when toStderr), backgrounds a sleep
// inheriting the capture pipe (the surviving descendant of #1967), records its
// pid, and exits 0. The straggler is killed on cleanup so nothing is orphaned and
// a call left blocked by a regression is unblocked immediately.
func installExecStragglerFake(t *testing.T, name, line string, toStderr bool) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "straggler.pid")
	redir := ""
	if toStderr {
		redir = " 1>&2"
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' " + shSingleQuoteExec(line) + redir + "\n" +
		"sleep " + strconv.Itoa(dockerStragglerSleep) + " &\n" +
		"echo $! > " + shSingleQuoteExec(pidFile) + "\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

func shSingleQuoteExec(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// returnsWithinExec runs fn in a goroutine and reports whether it returned within
// d. A false result is the signature of a missing WaitDelay: the call is blocked
// on a straggler holding the capture pipe past the context deadline (#1967).
func returnsWithinExec(d time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

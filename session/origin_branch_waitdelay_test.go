package session

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lsRemoteBranch sets cmd.WaitDelay = dockerWaitDelay (#1967) but, unlike every
// sibling WaitDelay site, used to omit the other half of the convention —
// normalizing a bare exec.ErrWaitDelay to success. These tests drive the REAL
// lsRemoteBranch through a fake git on PATH that backgrounds a straggler holding
// the capture pipe, the shape that surfaces a bare exec.ErrWaitDelay: git exits 0
// (or 2) and only a descendant holds the pipe past dockerWaitDelay.
//
// Observed failing before the fix: a git that exited 0 after writing the ref
// line, with a straggler outliving it past dockerWaitDelay, produced
// `git ls-remote: exec: WaitDelay expired before I/O complete: <ref>` —
// errors.Is(err, exec.ErrWaitDelay) = true, errors.As(err, &ExitError) = false —
// and lsRemoteBranch returned (nil, false, err), discarding a healthy ls-remote
// answer and refusing a session restore whose branch was on origin. The fix
// normalizes that bare ErrWaitDelay to nil so the already-captured ref line is
// returned, exactly like originRemoteURL and dockerExec.

// TestLsRemoteBranch_WaitDelayBoundsStraggler is the regression: a fake git that
// answers with the ref line, backgrounds a pipe-holding straggler, and exits 0
// must return (out, true, nil) within dockerStragglerGuard — the ctx deadline
// (30s) does not bound the call, only dockerWaitDelay does, and the resulting
// bare exec.ErrWaitDelay is git-exited-0 success, not a failure.
func TestLsRemoteBranch_WaitDelayBoundsStraggler(t *testing.T) {
	const (
		branch = "af/b-2-a"
		sha    = "abc123def4567890abcdef1234567890abcdef12"
	)
	refLine := sha + "\trefs/heads/" + branch
	installExecStragglerFake(t, "git", refLine, false)

	var (
		out   []byte
		found bool
		err   error
	)
	if !returnsWithinExec(dockerStragglerGuard, func() {
		out, found, err = lsRemoteBranch("https://origin.example/repo.git", branch, 30*time.Second)
	}) {
		t.Fatalf("lsRemoteBranch did not return within %s — Output() is blocked on a straggler holding the capture pipe past the deadline; dockerWaitDelay is missing or its bare exec.ErrWaitDelay is not normalized to success (#1967)", dockerStragglerGuard)
	}
	if err != nil {
		t.Fatalf("lsRemoteBranch returned error (a bare exec.ErrWaitDelay must normalize to success — git exited 0 and the ref line is already in out): %v", err)
	}
	if !found {
		t.Fatalf("lsRemoteBranch found=false, want true — git answered with the ref line, so origin has the branch")
	}
	if !strings.Contains(string(out), refLine) {
		t.Fatalf("lsRemoteBranch out=%q missing the captured ref line %q — the straggler-held pipe must not lose already-written output", out, refLine)
	}
}

// TestLsRemoteBranch_WaitDelayExitTwoStillLacksRef is the control for the
// normalization's ordering: a fake git that exits 2 (git ls-remote --exit-code's
// "reached origin, no matching ref") with a straggler must still return
// (nil, false, nil). A non-zero exit surfaces as an *exec.ExitError, not a bare
// exec.ErrWaitDelay (Go's Cmd.Wait discards the WaitDelay error when the process
// already exited non-zero), so the ErrWaitDelay-to-nil guard must NOT mask it.
// Getting this backwards would report a missing branch as present.
func TestLsRemoteBranch_WaitDelayExitTwoStillLacksRef(t *testing.T) {
	installGitStragglerFakeExit(t, "", 2)

	var (
		out   []byte
		found bool
		err   error
	)
	if !returnsWithinExec(dockerStragglerGuard, func() {
		out, found, err = lsRemoteBranch("https://origin.example/repo.git", "af/never-pushed", 30*time.Second)
	}) {
		t.Fatalf("lsRemoteBranch did not return within %s — the exit-2 + straggler case must still be bounded by dockerWaitDelay", dockerStragglerGuard)
	}
	if err != nil {
		t.Fatalf("lsRemoteBranch returned error (exit 2 is 'origin lacks the ref', not a failure; a bare exec.ErrWaitDelay must not mask the *exec.ExitError(2) either): %v", err)
	}
	if found {
		t.Fatalf("lsRemoteBranch found=true, want false — exit 2 means origin positively lacks the ref; the ErrWaitDelay normalization must not have masked it")
	}
	if len(out) != 0 {
		t.Fatalf("lsRemoteBranch out=%q, want empty — git ls-remote --exit-code prints nothing on exit 2", out)
	}
}

// installGitStragglerFakeExit writes a fake git onto PATH that prints line (to
// stdout, or nothing when line == ""), backgrounds a sleep inheriting the capture
// pipe (the surviving descendant of #1967), records its pid, and exits with code.
// A generalization of installExecStragglerFake (which is hardcoded to exit 0)
// for the exit-2 control: git ls-remote --exit-code exits 2 with no stdout when
// origin lacks the ref, yet a straggler may still hold the pipe past
// dockerWaitDelay. The straggler is killed on cleanup so nothing is orphaned.
func installGitStragglerFakeExit(t *testing.T, line string, code int) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "straggler.pid")
	print := ""
	if line != "" {
		print = "printf '%s\\n' " + shellQuote(line) + "\n"
	}
	script := "#!/bin/sh\n" +
		print +
		"sleep " + strconv.Itoa(dockerStragglerSleep) + " &\n" +
		"echo $! > " + shellQuote(pidFile) + "\n" +
		"exit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
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

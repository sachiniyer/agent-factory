package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// #2030: the kill-confirmation's git reads run synchronously in handleKill on the
// Bubble Tea Update loop, on bare exec.Command(...).Output() with no context, no
// timeout, and no WaitDelay. A wedged git — a hung network mount, a D-state
// process holding the worktree, or (as reproduced here) a child that inherited the
// capture pipe and outlives git — would block Output() on pipe EOF and freeze the
// whole TUI. Same class as #1967 (exec.CommandContext without WaitDelay).
//
// This drives a REAL fake git on PATH whose child holds the capture pipe open past
// git's own exit (a blocking mock can't reproduce a pipe-holding descendant, which
// is the exact failure WaitDelay exists for). Without the bound the call waits the
// straggler out (stragglerSleep); with runKillGit's WaitDelay + process-group reap
// it returns in ~killGitWaitDelay.

const (
	// killStragglerSleep must exceed killStragglerGuard so a missing WaitDelay is a
	// guard failure, not a slow pass; the straggler is killed on cleanup.
	killStragglerSleep = 30
	// killStragglerGuard sits between the 2s killGitWaitDelay and the straggler
	// sleep, with ample slack over 2s so a loaded box never flakes the fixed path.
	killStragglerGuard = 8 * time.Second
)

// installStragglerGitOnPath writes a fake `git` onto PATH that backgrounds a sleep
// inheriting the capture pipe (the surviving descendant of #1967) then exits 0, and
// returns a worktree dir to pass to the confirmation helpers. The straggler is
// killed on cleanup so nothing is orphaned and any call left blocked by a
// regression is unblocked immediately.
func installStragglerGitOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "straggler.pid")
	script := "#!/bin/sh\n" +
		"sleep " + strconv.Itoa(killStragglerSleep) + " &\n" +
		"echo $! > " + shSingleQuote(pidFile) + "\n" +
		"exit 0\n"
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
			_ = os.Remove(pidFile)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return t.TempDir() // a distinct dir as the "worktree" arg; the fake ignores it
}

func shSingleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// TestKillConfirmationWarningIsBounded is the #2030 regression driven through the
// real production path (killConfirmationWarning → runKillGit): it must return on
// its own deadline rather than block on a git whose child holds the capture pipe.
func TestKillConfirmationWarningIsBounded(t *testing.T) {
	wt := installStragglerGitOnPath(t)

	done := make(chan string, 1)
	go func() { done <- killConfirmationWarning(wt) }()

	select {
	case <-done:
	case <-time.After(killStragglerGuard):
		t.Fatalf("killConfirmationWarning did not return within %s — its git exec runs on the Bubble Tea "+
			"Update loop with no WaitDelay, so a git whose child holds the capture pipe (or a wedged git) "+
			"freezes the whole TUI (#2030)", killStragglerGuard)
	}
}

// TestUnmergedCommitWarningIsBounded covers the other confirmation helper, which
// makes several git reads (rev-parse, log …) — all now through runKillGit, so a
// wedged git on any of them cannot hang the Update loop either.
func TestUnmergedCommitWarningIsBounded(t *testing.T) {
	wt := installStragglerGitOnPath(t)

	done := make(chan struct{}, 1)
	go func() {
		_, _ = unmergedCommitWarning(wt, "dev/bounded", "deadbeef", "", true)
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-time.After(killStragglerGuard):
		t.Fatalf("unmergedCommitWarning did not return within %s — one of its git reads on the Update "+
			"loop is unbounded (#2030)", killStragglerGuard)
	}
}

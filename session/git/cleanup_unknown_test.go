package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The #1917 round-4 locks: a deadline ANYWHERE in a cleanup run must mark the run
// UNKNOWN, and no destructive act may follow one.
//
// Both bugs these cover had the same root: the state was asserted once
// (`state := CleanupSettled`) and downgraded by hand at exactly ONE of the five
// bounded git commands. These tests pin the runner-derives-the-state property, so
// they fail if anyone reintroduces hand-assigned state.

// stallGitOn puts a fake `git` on PATH that stalls for the named subcommands and
// exits 0 for the rest. A real subprocess, not a mock: the unknown state is derived
// from a real ctx deadline SIGKILLing a real process group, so a mock that merely
// returned an error would prove nothing.
func stallGitOn(t *testing.T, stallArg string, fastFailArgs string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    ` + stallArg + `) exec sleep 300 ;;
    ` + fastFailArgs + `) echo "fatal: validation failed" >&2; exit 128 ;;
  esac
done
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	prev := localGitTimeout
	localGitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { localGitTimeout = prev })
}

func worktreeForCleanup(t *testing.T) (*GitWorktree, string) {
	t.Helper()
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "work.txt"), []byte("unpushed user work"), 0o644); err != nil {
		t.Fatalf("seed work: %v", err)
	}
	gw, err := NewGitWorktreeFromStorage(root, wt, "sess", "branch", "abc123", false, true)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}
	return gw, wt
}

// TestCleanup_TimedOutRegistrationProbe_DoesNotDeleteTheDirectory is round-4
// finding (1): the fix reintroduced the bug it was removing.
//
// `worktree remove` answers FAST with "validation failed" (the #726 corrupted-
// pointer shape), and the bounded `worktree list` probe then TIMES OUT on the
// stalled mount. The probe's error used to be read as an ordinary lookup failure,
// so the string-matching fallback returned true and Cleanup called UNBOUNDED
// os.RemoveAll on a stalled path — which hangs forever inside the kill guard: the
// exact wedge this PR exists to remove.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the directory is deleted (and, on a truly
// stalled mount, the call never returns at all).
func TestCleanup_TimedOutRegistrationProbe_DoesNotDeleteTheDirectory(t *testing.T) {
	stallGitOn(t, `"list"`, `"remove"`)
	gw, wt := worktreeForCleanup(t)

	state, err := gw.Cleanup()

	if state != CleanupStateUnknown {
		t.Fatal("a timed-out registration probe reported a SETTLED cleanup: the caller then deletes " +
			"the session record, and the string fallback lets an UNBOUNDED os.RemoveAll run against " +
			"the very path that just stalled git — reintroducing the #1917 wedge")
	}
	if err == nil {
		t.Fatal("a cut-off cleanup must report an error")
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("the worktree directory was deleted after a probe that never answered: the "+
			"deletion decision was taken on a verdict we could not obtain (%v)", statErr)
	}
}

// TestCleanup_TimedOutBranchDelete_ReportsUnknown is round-4 finding (2): deadlines
// that do not mark unknown.
//
// The worktree is already absent, so the only bounded commands left are `prune` and
// `branch -D`. Neither used to touch the state, so a timed-out branch delete
// reported CleanupSettled — teardownKill logged it best-effort, finalized, and
// KillSession deleted the record, even though the branch and worktree metadata may
// still exist and there is now no record to retry from.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: state == CleanupSettled.
func TestCleanup_TimedOutBranchDelete_ReportsUnknown(t *testing.T) {
	stallGitOn(t, `"branch"`, `"__never__"`)
	root := t.TempDir()
	// No worktree on disk: skip straight past the remove branch to prune + branch -D.
	gw, err := NewGitWorktreeFromStorage(root, filepath.Join(root, "gone"), "sess", "branch", "abc123", false, true)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}

	state, cleanupErr := gw.Cleanup()

	if state != CleanupStateUnknown {
		t.Fatal("a timed-out `git branch -D` reported a SETTLED cleanup: the branch may still exist, " +
			"but the caller deletes the record on this and no retry is possible (#1917 round 4). " +
			"EVERY bounded command in the run must mark unknown, not just the worktree remove.")
	}
	if cleanupErr == nil || !errors.Is(cleanupErr, context.DeadlineExceeded) {
		t.Fatalf("the deadline must stay reachable in the error, got: %v", cleanupErr)
	}
}

// TestCleanup_HealthyRun_IsSettled is the guard in the other direction: inverting
// the default must not make every cleanup refuse. A run where every command answers
// reports Settled, so ordinary kills still drop their record.
func TestCleanup_HealthyRun_IsSettled(t *testing.T) {
	stallGitOn(t, `"__never__"`, `"__never_either__"`)
	gw, wt := worktreeForCleanup(t)

	state, err := gw.Cleanup()
	if err != nil {
		t.Fatalf("a healthy cleanup must not error: %v", err)
	}
	if state != CleanupSettled {
		t.Fatal("a run where every git command answered must report Settled, or no kill ever " +
			"completes and the inversion has made sessions undeletable")
	}
	_ = wt
}

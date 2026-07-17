package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCleanup_RetryAfterStall_StillRefusesTheUnboundedDelete is #1917 round-6
// finding (1): refusing once is not refusing.
//
// The dangerous shape: `git worktree remove` times out AFTER deregistering the
// checkout but BEFORE deleting its files. Attempt 1 correctly refuses the unbounded
// os.RemoveAll. finishUserKill then retries — and the retry's FRESH cleanupRun has
// unknown=false, while `worktree list` now answers "not registered" (it was
// deregistered!), so shouldRemoveWorktreeDir says yes and the unbounded delete runs
// against the same stalled filesystem, wedging the kill guard indefinitely.
//
// The knowledge that this filesystem stalls must outlive the attempt.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: attempt 2 deletes the directory (and against a
// genuinely stalled mount would never return at all).
func TestCleanup_RetryAfterStall_StillRefusesTheUnboundedDelete(t *testing.T) {
	dir := t.TempDir()
	// Attempt 1: `worktree remove` stalls (the deadline trips). Attempt 2: every
	// command answers fast, and `worktree list` reports the path unregistered —
	// exactly what a remove that deregistered-then-stalled leaves behind.
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    remove) if [ -f "` + dir + `/stalled" ]; then exit 128; fi; touch "` + dir + `/stalled"; exec sleep 300 ;;
    list)   exit 0 ;;
  esac
done
exit 0
`
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := localGitTimeout
	localGitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { localGitTimeout = prev })

	gw, wt := worktreeForCleanup(t)

	// Attempt 1: the remove stalls; the refusal is correct and already covered.
	state1, _ := gw.Cleanup()
	if state1 != CleanupStateUnknown {
		t.Fatalf("setup: attempt 1 must report unknown, got %v", state1)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("setup: attempt 1 must not have deleted the directory: %v", err)
	}

	// Attempt 2 — the retry finishUserKill makes, on the SAME workspace.
	state2, _ := gw.Cleanup()

	if _, err := os.Stat(wt); err != nil {
		t.Fatal("the RETRY ran the unbounded os.RemoveAll against a filesystem already known to " +
			"stall: the knowledge died with attempt 1's run, so the second attempt re-enters the " +
			"exact wedge this PR removes (#1917 round 6). Refusing once is not refusing.")
	}
	if state2 != CleanupStateUnknown {
		t.Fatal("the retry reported a SETTLED cleanup while the directory is still on disk: the " +
			"caller would drop the record and orphan it")
	}
}

// TestCleanup_StalledRemove_DoesNotDeleteTheRetainedWorkspacesBranch is #1917
// round-8 finding (1), and it is the worst outcome in this PR's history: we save
// the files and destroy the only pointer to them.
//
// `git worktree remove -f` times out AFTER deregistering the checkout but BEFORE
// deleting its files. removeDir correctly preserves the directory — and then
// `git branch -D` SUCCEEDS, precisely BECAUSE the checkout is now unregistered, so
// git raises no "branch is checked out" objection. The retained workspace loses its
// only ref and its unique commits become unreachable.
//
// Refusing the file deletion while destroying the metadata is worse than either
// alone: the workspace survives and nothing can find it.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: `branch -D` is issued for a workspace the same
// run just decided to retain.
func TestCleanup_StalledRemove_DoesNotDeleteTheRetainedWorkspacesBranch(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "issued")
	// `remove` stalls (after "deregistering"); every other verb answers instantly,
	// so branch -D would sail through. Each invocation is recorded.
	script := `#!/bin/sh
echo "$@" >> ` + log + `
for a in "$@"; do
  case "$a" in
    remove) exec sleep 300 ;;
  esac
done
exit 0
`
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := localGitTimeout
	localGitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { localGitTimeout = prev })

	gw, wt := worktreeForCleanup(t)
	state, _ := gw.Cleanup()

	if state != CleanupStateUnknown {
		t.Fatalf("setup: a stalled remove must report unknown, got %v", state)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("setup: the workspace must be retained: %v", err)
	}

	issued, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read issued log: %v", err)
	}
	if strings.Contains(string(issued), "branch -D") {
		t.Fatal("cleanup deleted the BRANCH of a workspace it had just decided to RETAIN: the " +
			"timed-out remove deregistered the checkout, so branch -D no longer errors and the " +
			"retained files lose their only ref — unique commits become unreachable. Saving the " +
			"files and destroying the pointer to them is worse than either alone (#1917 round 8). " +
			"Unknown must gate EVERY destructive act in the run, not only the file deletion.")
	}
	if strings.Contains(string(issued), "worktree prune") {
		t.Fatal("cleanup pruned worktree metadata for a workspace it had just decided to retain")
	}
}

// TestCleanup_CorruptRepoWithUnreadableRegistration_StillDeletesTheDirectory is a
// regression I introduced and caught reviewing my own diff (the GitHub bot has not
// looked at this PR since round 1).
//
// The #726 shape: `git worktree remove -f` fails FAST with "validation failed" (a
// corrupted .git pointer) AND `git worktree list` also fails FAST — an ANSWERED
// error, not a stall. Before this PR, Cleanup fell back to the conservative string
// gate and deleted the directory ("Also the path taken when `worktree list` itself
// failed"). Round 4's fix made the probe refuse on ANY unreadable registration,
// which conflated "could not ask" with "asked and got an error" — so nothing was
// deleted, yet no deadline tripped, so the run reported SETTLED and the caller
// dropped the record. The workspace is left on disk with nothing pointing at it:
// the exact orphaning this PR exists to prevent, caused by its own fix.
func TestCleanup_CorruptRepoWithUnreadableRegistration_StillDeletesTheDirectory(t *testing.T) {
	// Every git command ANSWERS with a failure, instantly. Nothing stalls.
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'fatal: validation failed' >&2\nexit 128\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := localGitTimeout
	localGitTimeout = 5 * time.Second // generous: nothing here should ever reach it
	t.Cleanup(func() { localGitTimeout = prev })

	gw, wt := worktreeForCleanup(t)
	state, _ := gw.Cleanup()

	if _, err := os.Stat(wt); err == nil {
		t.Fatal("a corrupted worktree whose registration could not be READ (an answered error, " +
			"not a stall) was left on disk while the run reported its outcome as settled — so the " +
			"caller deletes the record and orphans it. 'Could not ask' and 'asked and got an error' " +
			"are different: only the first is unknown (#726 regression).")
	}
	if state != CleanupSettled {
		t.Fatalf("nothing timed out, so the run's outcome is established: want CleanupSettled, got %v", state)
	}
}

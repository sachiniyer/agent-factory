package git

import (
	"os"
	"path/filepath"
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

package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests pin the fix for the plain-kill retained-tree cleanup wedge: the
// repo-gone twin of cleanupRetainedArchiveTrees is wrapped in a deadline-bound
// goroutine (worktree_repo_gone_cleanup.go), but the plain-kill call site ran
// it synchronously with no deadline (#3171). A retained tree can sit on a
// different, stallable filesystem than the agent-factory home, so a stalled
// syscall there wedged the kill forever under the kills-in-flight guard and the
// per-session op lock. The fix reuses the repo-gone path's deadline seam so the
// plain-kill wait is bounded by the same cleanup deadline.

// TestCleanup_RetainedTreeDeletionSharesDeadline is the plain-kill mirror of
// TestCleanupClaimedRepoGone_RetainedTreeDeletionSharesDeadline: a retained-tree
// cleanup that never answers must not run outside the kill's cleanup deadline.
// A synchronous unbounded call would block forever (the reproduced wedge); the
// bounded goroutine returns CleanupStateUnknown/DeadlineExceeded, keeps the
// worktree as the retry handle, and releases the kill's guards.
func TestCleanup_RetainedTreeDeletionSharesDeadline(t *testing.T) {
	gw, wt := worktreeForCleanup(t)
	previousCleanup := repoGoneCleanupRetainedTrees
	previousTimeout := repoGoneCleanupTimeout
	started := make(chan struct{})
	release := make(chan struct{})
	repoGoneCleanupTimeout = 25 * time.Millisecond
	repoGoneCleanupRetainedTrees = func(*GitWorktree) error {
		close(started)
		<-release
		return errors.New("retained tree remained unavailable")
	}
	t.Cleanup(func() {
		repoGoneCleanupRetainedTrees = previousCleanup
		repoGoneCleanupTimeout = previousTimeout
	})

	type result struct {
		state CleanupState
		err   error
	}
	done := make(chan result, 1)
	go func() {
		state, err := gw.Cleanup()
		done <- result{state: state, err: err}
	}()
	// The seam must be invoked at all: a plain-kill path that is not bound to the
	// deadline seam calls cleanupRetainedArchiveTrees directly, so this override
	// never runs, started never closes, and the wait fails here instead of at the
	// deadline — pinning that the bound goroutine is the path in production.
	select {
	case <-started:
	case <-time.After(deadlineObservationBudget):
		close(release)
		<-done
		t.Fatal("retained-tree cleanup seam was never invoked: the plain-kill path is not running cleanupRetainedArchiveTrees through the deadline-bound goroutine")
	}

	var observed result
	select {
	case observed = <-done:
	case <-time.After(deadlineObservationBudget):
		close(release)
		<-done
		t.Fatal("retained-tree deletion ran outside the plain kill's cleanup deadline")
	}
	if observed.state != CleanupStateUnknown || !errors.Is(observed.err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("retained-tree deadline did not return a retryable unknown result: state=%v err=%v", observed.state, observed.err)
	}
	// The cleanup returned before reaching the worktree removal, so the
	// worktree directory must remain as the durable retry handle.
	if _, err := os.Stat(wt); err != nil {
		close(release)
		t.Fatalf("cleanup deleted the worktree after a retained-tree cleanup that never finished: %v", err)
	}
	// The abandoned worker may finish in the background; releasing it must not
	// panic or race the returned handle. The buffered result channel absorbs the
	// late send without a reader.
	close(release)
}

// TestCleanup_RetainedTreeDeletionRefusalPropagatesAndKeepsRecord guards the
// non-stall failure path through the goroutine: a retained-tree cleanup that
// ANSWERS with a refusal (identity mismatch, unreadable pin) must still mark the
// run unknown, surface the error, and keep the worktree — exactly the contract
// the old synchronous call had. Wrapping the call in a goroutine must not swallow
// or reorder that error.
func TestCleanup_RetainedTreeDeletionRefusalPropagatesAndKeepsRecord(t *testing.T) {
	gw, wt := worktreeForCleanup(t)
	previousCleanup := repoGoneCleanupRetainedTrees
	errRetainedRefusal := errors.New("retained archive tree refusal sentinel")
	repoGoneCleanupRetainedTrees = func(*GitWorktree) error {
		return errRetainedRefusal
	}
	t.Cleanup(func() { repoGoneCleanupRetainedTrees = previousCleanup })

	state, err := gw.Cleanup()
	if state != CleanupStateUnknown {
		t.Fatalf("a refused retained-tree cleanup must mark the run unknown so the caller keeps the record; got state=%v", state)
	}
	if err == nil || !errors.Is(err, errRetainedRefusal) {
		t.Fatalf("the retained-tree refusal must propagate through the goroutine; got err=%v", err)
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("a refused retained-tree cleanup must not reach the worktree removal: %v", statErr)
	}
}

// TestCleanup_RemovesRetainedArchiveTreeAndSettles guards the healthy path: the
// deadline-bound goroutine must not change the success contract. A real retained
// archive tree is consumed before the ordinary worktree, the cleanup settles, and
// the report is emptied — so a regression that always trips the deadline (or
// swallows the goroutine's nil result) fails here rather than silently making
// every retained-tree kill best-effort.
func TestCleanup_RemovesRetainedArchiveTreeAndSettles(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	worktreePath := filepath.Join(t.TempDir(), "wt")
	runGit(t, repoRoot, "worktree", "add", "-b", "af-retained-healthy", worktreePath)

	retained := filepath.Join(filepath.Dir(worktreePath), ".af-source-0123456789abcdef0123456789abcdef")
	if err := os.Mkdir(retained, 0o700); err != nil {
		t.Fatalf("create retained archive tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(retained, "private-work.txt"), []byte("retained user work"), 0o600); err != nil {
		t.Fatalf("write retained archive tree: %v", err)
	}
	identity, err := inspectRelocationPathIdentity(retained)
	if err != nil {
		t.Fatalf("inspect retained archive tree: %v", err)
	}
	pin, err := inspectRelocationPathPin(retained)
	if err != nil {
		t.Fatalf("pin retained archive tree: %v", err)
	}
	gw := &GitWorktree{
		repoPath:          repoRoot,
		worktreePath:      worktreePath,
		branchName:        "af-retained-healthy",
		branchCreatedByUs: true,
	}
	gw.RestoreArchiveReport(ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		newArchiveRetainedTree(retained, identity, pin, []ArchiveSkippedEntry{
			newArchiveSkippedEntry("private-work.txt", ArchiveSkipPermissionDenied),
		}),
	}})

	// Generous: nothing here should ever stall, but keep the test fast on a slow
	// runner by shortening the local git command deadline.
	shortenLocalTimeout(t, 30*time.Second)

	state, cleanupErr := gw.Cleanup()
	if cleanupErr != nil {
		t.Fatalf("a healthy retained-tree cleanup must not error: %v", cleanupErr)
	}
	if state != CleanupSettled {
		t.Fatalf("a healthy retained-tree cleanup must establish its outcome; got state=%v", state)
	}
	if _, err := os.Stat(retained); !os.IsNotExist(err) {
		t.Fatalf("cleanup orphaned a retained archive tree after removing its durable row: %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("cleanup left the primary worktree behind: %v", err)
	}
	if report := gw.GetArchiveReport(); !report.Empty() {
		t.Fatalf("cleanup retained a report after consuming all owned trees: %+v", report)
	}
}

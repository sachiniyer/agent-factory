package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests pin the fix for the plain-kill retained-tree cleanup wedge: the
// repo-gone twin of cleanupRetainedArchiveTrees is wrapped in a deadline-bound
// goroutine (worktree_repo_gone_cleanup.go), but the plain-kill call site ran
// it synchronously with no deadline (#3171, #7928). A retained tree can sit on
// a different, stallable filesystem than the agent-factory home, so a stalled
// syscall there wedged the kill forever under the kills-in-flight guard and the
// per-session op lock.
//
// The replacement also pins the fence the closed #7937 head got wrong (review
// R1/R2): at most one cleanup worker per retained tree, decided BEFORE a worker
// starts, keyed on the durable tree path so it survives a GitWorktree rebuilt
// from storage, and owned by the abandoned worker alone — released by its own
// return, never by the deadline and never by a durable latch. None of these
// tests mounts anything: the wedge lives behind the existing function-variable
// seams and is installed before the entry point under test is called.

// cleanupResult pairs what (g *GitWorktree).Cleanup returns.
type cleanupResult struct {
	state CleanupState
	err   error
}

// cleanupAsync runs gw.Cleanup on a goroutine and delivers its result.
func cleanupAsync(gw *GitWorktree) <-chan cleanupResult {
	done := make(chan cleanupResult, 1)
	go func() {
		state, err := gw.Cleanup()
		done <- cleanupResult{state: state, err: err}
	}()
	return done
}

// awaitCleanup receives a cleanup that is already expected to have produced a
// result — the budget is a hang detector, not a work budget.
func awaitCleanup(t *testing.T, done <-chan cleanupResult, what string) cleanupResult {
	t.Helper()
	select {
	case observed := <-done:
		return observed
	case <-time.After(deadlineObservationBudget):
		t.Fatalf("timed out after %s waiting for %s", deadlineObservationBudget, what)
		return cleanupResult{}
	}
}

// retainedCleanupFixture builds a real repo, a registered worktree and a real
// retained archive tree on disk, and returns the gw whose durable report owns
// the tree plus the paths a test asserts against.
func retainedCleanupFixture(t *testing.T) (gw *GitWorktree, repoRoot, worktreePath, retained string, report ArchiveReport) {
	t.Helper()
	sandboxHome(t)
	repoRoot = createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	worktreePath = filepath.Join(t.TempDir(), "wt")
	runGit(t, repoRoot, "worktree", "add", "-b", "af-retained-deadline", worktreePath)

	retained = filepath.Join(filepath.Dir(worktreePath), ".af-source-0123456789abcdef0123456789abcdef")
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
	report = ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		newArchiveRetainedTree(retained, identity, pin, []ArchiveSkippedEntry{
			newArchiveSkippedEntry("private-work.txt", ArchiveSkipPermissionDenied),
		}),
	}}
	gw = &GitWorktree{
		repoPath:          repoRoot,
		worktreePath:      worktreePath,
		branchName:        "af-retained-deadline",
		branchCreatedByUs: true,
	}
	gw.RestoreArchiveReport(report)
	return gw, repoRoot, worktreePath, retained, report
}

// retainedCleanupWedge owns the seams a test drives. started closes on the
// FIRST repoGoneCleanupRetainedTrees invocation — by then the worker holds the
// tree fence — and every invocation blocks on release, so releasing the wedge
// is also what retires an abandoned worker. calls counts every worker start,
// which is how a test proves a retry spawned nothing.
type retainedCleanupWedge struct {
	deadlineCh  chan time.Time
	release     chan struct{}
	started     chan struct{}
	calls       *atomic.Int32
	workerError error
	// runReal makes the stub call the real cleanupRetainedArchiveTrees once
	// released, so a "slow but answering" worker performs the genuine delete.
	runReal bool
}

func newRetainedCleanupWedge(workerError error, runReal bool) *retainedCleanupWedge {
	return &retainedCleanupWedge{
		deadlineCh:  make(chan time.Time, 1),
		release:     make(chan struct{}),
		started:     make(chan struct{}),
		calls:       &atomic.Int32{},
		workerError: workerError,
		runReal:     runReal,
	}
}

// unblock releases every wedged invocation, once — registered on t.Cleanup so a
// failing assertion can never leak a worker parked on the wedge.
func (w *retainedCleanupWedge) unblock() {
	select {
	case <-w.release:
	default:
		close(w.release)
	}
}

// installRetainedCleanupStub wedges the retained-tree cleanup seam the test
// controls. Install BEFORE the entry point under test is called.
func installRetainedCleanupStub(t *testing.T, w *retainedCleanupWedge) {
	t.Helper()
	previousCleanup := repoGoneCleanupRetainedTrees
	repoGoneCleanupRetainedTrees = func(g *GitWorktree) error {
		if w.calls.Add(1) == 1 {
			close(w.started)
		}
		<-w.release
		if w.runReal {
			return g.cleanupRetainedArchiveTrees()
		}
		return w.workerError
	}
	t.Cleanup(func() { repoGoneCleanupRetainedTrees = previousCleanup })
	t.Cleanup(w.unblock)
}

// installChannelDeadline hands the test the cleanup deadline as a channel it
// fires explicitly — deterministic, unlike a short real timer.
func installChannelDeadline(t *testing.T, w *retainedCleanupWedge) {
	t.Helper()
	previousDeadline := repoGoneCleanupDeadline
	repoGoneCleanupDeadline = func() (<-chan time.Time, func()) {
		return w.deadlineCh, func() {}
	}
	t.Cleanup(func() { repoGoneCleanupDeadline = previousDeadline })
}

// awaitWorkerInside waits for the first invocation of the retained-tree seam.
// On master the plain-kill path never calls the seam — this is where every test
// in this file fails first against it — so retire the leaked synchronous call
// before reporting the missing bound.
func awaitWorkerInside(t *testing.T, w *retainedCleanupWedge, done <-chan cleanupResult) {
	t.Helper()
	select {
	case <-w.started:
	case <-time.After(deadlineObservationBudget):
		w.unblock()
		awaitCleanup(t, done, "the unbounded synchronous cleanup")
		t.Fatal("retained-tree cleanup seam was never invoked: the plain-kill path does not run cleanupRetainedArchiveTrees through the deadline-bound worker")
	}
}

// awaitRefusal asserts that a call to gw.Cleanup refuses quickly because a
// cleanup worker is still outstanding: the run reports unknown, the message
// says the cleanup is still running on a stalled or slow filesystem and that
// the kill retries automatically, and — the load-bearing part — no new worker
// was started. The 5s window detects a refusal that never comes (a stacked
// worker waits forever); it is not a speed budget.
func awaitRefusal(t *testing.T, gw *GitWorktree, calls *atomic.Int32, wantCalls int32, what string) {
	t.Helper()
	done := cleanupAsync(gw)
	select {
	case observed := <-done:
		if observed.state != CleanupStateUnknown {
			t.Fatalf("%s: a fenced retry must keep the record unknown, got state=%v err=%v", what, observed.state, observed.err)
		}
		if observed.err == nil || !strings.Contains(observed.err.Error(), "still running") ||
			!strings.Contains(observed.err.Error(), "retries automatically") {
			t.Fatalf("%s: a fenced retry must say the cleanup is still running and the kill retries automatically, got: %v",
				what, observed.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the retry did not refuse quickly — a second worker may have started (worker invocations so far: %d)",
			what, calls.Load())
	}
	if got := calls.Load(); got != wantCalls {
		t.Fatalf("%s: at most %d worker invocation(s) may ever run against one stalled tree, got %d",
			what, wantCalls, got)
	}
}

// awaitUnfencedRetry calls gw.Cleanup until an attempt is no longer refused by
// the still-running fence — i.e. the abandoned worker's own return released it —
// and returns that attempt's result.
func awaitUnfencedRetry(t *testing.T, gw *GitWorktree) cleanupResult {
	t.Helper()
	deadline := time.Now().Add(deadlineObservationBudget)
	for {
		state, err := gw.Cleanup()
		if err != nil && strings.Contains(err.Error(), "still running") {
			if time.Now().After(deadline) {
				t.Fatalf("the fence was still held %s after the worker returned — a durable latch outlived the work it fenced", deadlineObservationBudget)
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		return cleanupResult{state: state, err: err}
	}
}

// TestCleanup_RetainedTreeDeletionSharesDeadline is the plain-kill mirror of
// TestCleanupClaimedRepoGone_RetainedTreeDeletionSharesDeadline: a retained-tree
// cleanup that never answers must not run outside the kill's cleanup deadline.
// A synchronous unbounded call would block forever (the reproduced wedge); the
// bounded goroutine returns CleanupStateUnknown/DeadlineExceeded, keeps the
// worktree as the retry handle, and releases the kill's guards.
//
// The trailing CleanupRetryPending assertion is what this head adds over
// #7937's closed fc75d12: there the deadline arm wrote a durable cleanup_stalled
// latch, so this test fails on fc75d12 — the latch outlives the abandoned
// worker and refuses retries forever (review R1).
func TestCleanup_RetainedTreeDeletionSharesDeadline(t *testing.T) {
	gw, wt := worktreeForCleanup(t)
	wedge := newRetainedCleanupWedge(errors.New("retained tree remained unavailable"), false)
	installRetainedCleanupStub(t, wedge)

	// A real, short timer drives the deadline here: the bound must fire on its
	// own, not because the test pushed a channel.
	previousDeadline := repoGoneCleanupDeadline
	previousTimeout := repoGoneCleanupTimeout
	repoGoneCleanupTimeout = 25 * time.Millisecond
	repoGoneCleanupDeadline = func() (<-chan time.Time, func()) {
		timer := time.NewTimer(repoGoneCleanupTimeout)
		return timer.C, func() { timer.Stop() }
	}
	t.Cleanup(func() {
		repoGoneCleanupDeadline = previousDeadline
		repoGoneCleanupTimeout = previousTimeout
	})

	done := cleanupAsync(gw)
	awaitWorkerInside(t, wedge, done)

	observed := awaitCleanup(t, done, "the retained-tree deadline")
	if observed.state != CleanupStateUnknown || !errors.Is(observed.err, context.DeadlineExceeded) {
		t.Fatalf("retained-tree deadline did not return a retryable unknown result: state=%v err=%v", observed.state, observed.err)
	}
	// The cleanup returned before reaching the worktree removal, so the
	// worktree directory must remain as the durable retry handle.
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("cleanup deleted the worktree after a retained-tree cleanup that never finished: %v", err)
	}
	// The fence is process-local and owned by the abandoned worker — never a
	// durable cleanup_stalled latch. On #7937's fc75d12 this is true, and every
	// later attempt refuses "restart the daemon" even after the worker deletes
	// the tree (review R1).
	if gw.CleanupRetryPending() {
		t.Fatal("the deadline arm wrote a durable cleanup_stalled latch: a slow but healthy tree would now need a daemon restart (review R1)")
	}
	// The abandoned worker may finish in the background; releasing it must not
	// panic or race the returned handle. The buffered result channel absorbs the
	// late send without a reader.
	wedge.unblock()
}

// TestCleanup_RetainedTreeDeletionRefusalPropagatesAndKeepsRecord guards the
// non-stall failure path through the goroutine: a retained-tree cleanup that
// ANSWERS with a refusal (identity mismatch, unreadable pin) must still mark the
// run unknown, surface the error, and keep the worktree — exactly the contract
// the old synchronous call had. Wrapping the call in a goroutine must not swallow
// or reorder that error.
func TestCleanup_RetainedTreeDeletionRefusalPropagatesAndKeepsRecord(t *testing.T) {
	gw, wt := worktreeForCleanup(t)
	errRetainedRefusal := errors.New("retained archive tree refusal sentinel")
	wedge := newRetainedCleanupWedge(errRetainedRefusal, false)
	installRetainedCleanupStub(t, wedge)
	wedge.unblock() // the refusal answers immediately

	observed := awaitCleanup(t, cleanupAsync(gw), "the retained-tree refusal")
	if observed.state != CleanupStateUnknown {
		t.Fatalf("an answered retained-tree refusal must keep the run unknown, got state=%v", observed.state)
	}
	if !errors.Is(observed.err, errRetainedRefusal) {
		t.Fatalf("the refusal must reach the caller through the worker, got: %v", observed.err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("cleanup deleted the worktree after a refused retained-tree cleanup: %v", err)
	}
}

// TestCleanup_RetainedTreeRetryRefusesWhileWorkerOutstanding pins the fence the
// #7937 head never had (review R2 + the Codex P1): while the abandoned worker is
// still inside the stalled tree, a retry decides BEFORE spawning that a worker
// is outstanding and refuses quickly — it does not stack a second worker (and
// its stuck OS thread) onto the same tree. On master the seam is never invoked,
// so the fixture's first wait fails; on fc75d12 the second call spawns worker #2
// and blocks past the refusal window.
func TestCleanup_RetainedTreeRetryRefusesWhileWorkerOutstanding(t *testing.T) {
	gw, _, _, _, _ := retainedCleanupFixture(t)
	wedge := newRetainedCleanupWedge(errors.New("retained tree remained unavailable"), false)
	installRetainedCleanupStub(t, wedge)
	installChannelDeadline(t, wedge)

	done := cleanupAsync(gw)
	awaitWorkerInside(t, wedge, done)
	// The worker now holds the tree's fence inside the wedged call. Fire the
	// deadline: attempt one must return unknown + DeadlineExceeded while the
	// worker — and its fence — stay outstanding.
	wedge.deadlineCh <- time.Now()
	first := awaitCleanup(t, done, "the retained-tree deadline")
	if first.state != CleanupStateUnknown || !errors.Is(first.err, context.DeadlineExceeded) {
		t.Fatalf("the deadline arm must return unknown + DeadlineExceeded, got state=%v err=%v", first.state, first.err)
	}
	if report := gw.GetArchiveReport(); report.Empty() {
		t.Fatal("the deadline arm dropped the retained row: a retry would lose the tree's durable handle")
	}
	if gw.CleanupRetryPending() {
		t.Fatal("the deadline arm wrote a durable cleanup_stalled latch — the #7937 defect (review R1)")
	}

	// Attempt two is what finishUserKill's paced retry or a manual re-kill
	// calls. It must refuse quickly, name the still-running cleanup and the
	// automatic retry, and start NO worker: calls must stay at 1 forever while
	// the abandoned worker is outstanding.
	awaitRefusal(t, gw, wedge.calls, 1, "the paced retry")
	// And again: a permanent stall holds the fence, it is not a one-shot.
	awaitRefusal(t, gw, wedge.calls, 1, "a later paced retry")

	// Nothing else clears the fence — only the worker's own return. Release it,
	// let it answer failure, and the next attempt starts fresh.
	wedge.unblock()
	retried := awaitUnfencedRetry(t, gw)
	if retried.state != CleanupStateUnknown || retried.err == nil ||
		!strings.Contains(retried.err.Error(), "retained tree remained unavailable") {
		t.Fatalf("after the worker's failure the retry must surface that failure, got state=%v err=%v", retried.state, retried.err)
	}
	if got := wedge.calls.Load(); got != 2 {
		t.Fatalf("after the abandoned worker returned, the retry must start a FRESH worker, got %d invocation(s)", got)
	}
}

// TestCleanup_RetainedTreeLateWorkerSuccessLetsRetrySettle pins the other half
// of the worker-owned fence (review R1): when the abandoned worker returns
// SUCCESS after the caller's deadline — a slow but healthy tree on NFS — the
// fence drops with it and the next paced retry settles. On #7937's fc75d12 the
// durable latch refuses that retry forever, and CleanupRetryPending proves the
// mechanism.
func TestCleanup_RetainedTreeLateWorkerSuccessLetsRetrySettle(t *testing.T) {
	gw, _, worktreePath, retained, _ := retainedCleanupFixture(t)
	wedge := newRetainedCleanupWedge(nil, true)
	installRetainedCleanupStub(t, wedge)
	installChannelDeadline(t, wedge)

	done := cleanupAsync(gw)
	awaitWorkerInside(t, wedge, done)
	wedge.deadlineCh <- time.Now()
	first := awaitCleanup(t, done, "the retained-tree deadline")
	if first.state != CleanupStateUnknown || !errors.Is(first.err, context.DeadlineExceeded) {
		t.Fatalf("the deadline arm must return unknown + DeadlineExceeded, got state=%v err=%v", first.state, first.err)
	}
	// While the worker is outstanding, a retry refuses with no new worker.
	awaitRefusal(t, gw, wedge.calls, 1, "the retry between deadline and worker return")

	// The abandoned worker now finishes: the stub calls the real
	// cleanupRetainedArchiveTrees, which deletes the real tree, drops the row,
	// and — crucially — releases the fence on its own return.
	wedge.unblock()
	settled := awaitUnfencedRetry(t, gw)
	if settled.state != CleanupSettled || settled.err != nil {
		t.Fatalf("the retry after a late worker success must settle, got state=%v err=%v", settled.state, settled.err)
	}
	for path, what := range map[string]string{
		retained:     "retained archive tree",
		worktreePath: "primary worktree",
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the settled cleanup left the %s behind at %s: %v", what, path, err)
		}
	}
	if report := gw.GetArchiveReport(); !report.Empty() {
		t.Fatalf("the settled cleanup left a retained row behind: %+v", report)
	}
}

// TestCleanup_RetainedTreeLateWorkerFailureLetsRetryStartFresh pins that a
// worker's FAILURE releases the fence too: the next retry starts a fresh
// worker rather than reusing or refencing the dead one's claim.
func TestCleanup_RetainedTreeLateWorkerFailureLetsRetryStartFresh(t *testing.T) {
	gw, _, _, _, _ := retainedCleanupFixture(t)
	sentinel := errors.New("retained tree removal refused by the tree itself")
	wedge := newRetainedCleanupWedge(sentinel, false)
	installRetainedCleanupStub(t, wedge)
	installChannelDeadline(t, wedge)

	done := cleanupAsync(gw)
	awaitWorkerInside(t, wedge, done)
	wedge.deadlineCh <- time.Now()
	first := awaitCleanup(t, done, "the retained-tree deadline")
	if first.state != CleanupStateUnknown || !errors.Is(first.err, context.DeadlineExceeded) {
		t.Fatalf("the deadline arm must return unknown + DeadlineExceeded, got state=%v err=%v", first.state, first.err)
	}
	awaitRefusal(t, gw, wedge.calls, 1, "the retry while the worker is outstanding")

	// The abandoned worker now answers FAILURE. Its fence releases on return,
	// so the next attempt starts a fresh worker — which hits the same refusal
	// and fails the same way, without any latch reading "restart the daemon".
	wedge.unblock()
	retried := awaitUnfencedRetry(t, gw)
	if retried.state != CleanupStateUnknown || !errors.Is(retried.err, sentinel) {
		t.Fatalf("the fresh retry must surface the worker's own failure, got state=%v err=%v", retried.state, retried.err)
	}
	if got := wedge.calls.Load(); got != 2 {
		t.Fatalf("a fresh retry must spawn worker #2 after the first returned, got %d invocation(s)", got)
	}
	if gw.CleanupRetryPending() {
		t.Fatal("a worker failure wrote a durable latch — nothing but the worker's return may clear the fence")
	}
}

// TestCleanup_RetainedTreeFenceVisibleAcrossWorktreeRebuild pins the fence's
// key (review R2): it is the retained tree's durable path, not anything on the
// GitWorktree object, so a handle rebuilt from storage — the ghost path's
// per-kill rebuild, or a restored session's — sees a fence its earlier
// incarnation set and refuses instead of stacking a second stuck worker.
func TestCleanup_RetainedTreeFenceVisibleAcrossWorktreeRebuild(t *testing.T) {
	gw, repoRoot, worktreePath, _, report := retainedCleanupFixture(t)
	wedge := newRetainedCleanupWedge(errors.New("retained tree remained unavailable"), false)
	installRetainedCleanupStub(t, wedge)
	installChannelDeadline(t, wedge)

	done := cleanupAsync(gw)
	awaitWorkerInside(t, wedge, done)
	wedge.deadlineCh <- time.Now()
	first := awaitCleanup(t, done, "the retained-tree deadline")
	if first.state != CleanupStateUnknown {
		t.Fatalf("attempt one must return unknown at the deadline, got %v", first.state)
	}

	// Rebuild the worktree handle from storage exactly as ghostCleanupWorktree
	// does, restoring the same durable report. The rebuilt handle's retry must
	// see the outstanding worker's fence.
	rebuilt, err := NewGitWorktreeFromStorage(
		repoRoot, worktreePath, "sess", "af-retained-deadline", "", false, true,
	)
	if err != nil {
		t.Fatalf("rebuild the worktree handle: %v", err)
	}
	rebuilt.RestoreArchiveReport(report)
	awaitRefusal(t, rebuilt, wedge.calls, 1, "the ghost-path rebuild's retry")
}

// TestCleanup_SlowRetainedTreeSettlesWithoutDaemonRestart is the review's R1
// scenario end to end: a retained tree whose recursive delete takes longer than
// the cleanup deadline on a slow but HEALTHY filesystem — the 100k-file tree on
// NFS. Master deletes it in one attempt. The replacement must still delete it:
// the abandoned worker's own return releases the fence, and the next paced
// retry settles — no daemon restart, no durable latch.
func TestCleanup_SlowRetainedTreeSettlesWithoutDaemonRestart(t *testing.T) {
	gw, _, worktreePath, retained, _ := retainedCleanupFixture(t)
	wedge := newRetainedCleanupWedge(nil, true)
	installRetainedCleanupStub(t, wedge)
	installChannelDeadline(t, wedge)

	done := cleanupAsync(gw)
	awaitWorkerInside(t, wedge, done)
	// The worker is inside the tree — the deadline fires while the delete is
	// still genuinely making progress.
	wedge.deadlineCh <- time.Now()
	first := awaitCleanup(t, done, "the retained-tree deadline")
	if first.state != CleanupStateUnknown || !errors.Is(first.err, context.DeadlineExceeded) {
		t.Fatalf("the over-deadline attempt must return unknown + DeadlineExceeded, got state=%v err=%v", first.state, first.err)
	}
	if gw.CleanupRetryPending() {
		t.Fatal("a durable latch would refuse this healthy tree's retries forever — the #7937 defect (review R1)")
	}

	// The worker's own return — real removal, real row drop — is what frees the
	// next retry. That retry must settle the whole kill without any restart.
	wedge.unblock()
	settled := awaitUnfencedRetry(t, gw)
	if settled.state != CleanupSettled || settled.err != nil {
		t.Fatalf("the paced retry after the slow worker finished must settle, got state=%v err=%v", settled.state, settled.err)
	}
	for path, what := range map[string]string{
		retained:     "retained archive tree",
		worktreePath: "primary worktree",
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the settled kill left the %s behind at %s: %v", what, path, err)
		}
	}
	if report := gw.GetArchiveReport(); !report.Empty() {
		t.Fatalf("the settled kill left a retained row behind: %+v", report)
	}
}

// TestCleanup_RemovesRetainedArchiveTreeAndSettles is the healthy-path guard:
// a kill whose retained-tree cleanup answers in time behaves exactly as it did
// before the bound — retained tree, worktree and branch removed, report
// emptied — the goroutine wrapper changes nothing the caller observes.
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

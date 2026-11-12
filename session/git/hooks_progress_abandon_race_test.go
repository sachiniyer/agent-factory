//go:build linux

package git

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// publishHookProgressForTest publishes a fresh hook journal for the given
// worktree/session and generation, returning the decoded snapshot. It is the
// same construction a same-session rebuild performs at the same path: identical
// SessionID/Worktree/Prefix, new Directory and Generation.
func publishHookProgressForTest(t *testing.T, tree, sessionID, generation string) *hookProgress {
	t.Helper()
	p, err := newHookProgress(
		hookRun{worktreePath: tree, scopeSessionID: sessionID},
		[]string{"true"}, "af-hook-"+sessionID, generation,
	)
	if err != nil {
		t.Fatalf("publish hook progress gen=%s: %v", generation, err)
	}
	return p
}

// finishedMarker returns the path of the terminal marker inside a journal's
// receipt directory.
func finishedMarker(p *hookProgress) string {
	return filepath.Join(p.Directory, "finished")
}

// assertRebuiltJournalResumable fails the test if the rebuilt journal was
// terminalized or treated as conclusively non-resumable — the silent
// command-loss class the journal subsystem exists to prevent.
func assertRebuiltJournalResumable(t *testing.T, tree, sessionID string, rebuilt *hookProgress) {
	t.Helper()
	if rebuilt.finished() {
		t.Fatal("abandonment terminalized the rebuilt journal")
	}
	pending, pendingErr := readPendingHookProgress(tree, sessionID)
	if pending == nil || errors.Is(pendingErr, os.ErrNotExist) {
		t.Fatalf("rebuilt journal was treated as non-resumable: pending=%v err=%v", pending, pendingErr)
	}
}

// TestAbandonHookProgressSnapshotTerminalizesMatchingJournal confirms the
// locked abandonment still terminalizes a journal whose immutable identity
// matches the snapshot — the no-regression guarantee for the synchronous path.
func TestAbandonHookProgressSnapshotTerminalizesMatchingJournal(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	p := publishHookProgressForTest(t, tree, "owner", "gen1")
	done, captured, err := abandonHookProgressSnapshot(tree, "owner", p)
	if err != nil {
		t.Fatalf("abandonHookProgressSnapshot: %v", err)
	}
	if !done {
		t.Fatal("matching journal was not terminalized conclusively")
	}
	if captured == nil || captured.Directory != p.Directory || captured.Generation != p.Generation {
		t.Fatalf("captured identity = %+v; want the matching journal", captured)
	}
	if !p.finished() {
		t.Fatal("matching journal was not terminalized")
	}
}

// TestAbandonHookProgressSnapshotSkipsReplacementJournal confirms the locked
// abandonment refuses to terminalize a replacement journal a same-session
// rebuild published at the same path while the abandonment was deferred. This is
// the core fix: the immutable identity check (Directory/SessionID/Generation)
// gates the mutation under the .progress lock.
func TestAbandonHookProgressSnapshotSkipsReplacementJournal(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	first := publishHookProgressForTest(t, tree, "owner", "gen1")
	// Same-session rebuild: new Generation, new entries- directory, identical
	// SessionID/Worktree/Prefix.
	second := publishHookProgressForTest(t, tree, "owner", "gen2")
	if second.Directory == first.Directory || second.Generation == first.Generation {
		t.Fatalf("rebuild did not produce a distinct identity: dir=%s gen=%s", second.Directory, second.Generation)
	}
	done, captured, err := abandonHookProgressSnapshot(tree, "owner", first)
	if err != nil {
		t.Fatalf("abandonHookProgressSnapshot: %v", err)
	}
	if !done {
		t.Fatal("replacement journal did not conclude the abandonment (skip must be conclusive)")
	}
	if captured == nil || captured.Directory != second.Directory || captured.Generation != second.Generation {
		t.Fatalf("captured identity = %+v; want the replacement journal", captured)
	}
	assertRebuiltJournalResumable(t, tree, "owner", second)
}

// TestAbandonHookProgressSnapshotCapturesIdentityWhenNoneProvided models the
// p == nil installation: the shared restore read missed its budget before
// decoding a journal, so the first locked attempt must capture the identity of
// whatever owned journal it reads. A replacement published after that first
// read is then left to its own runner on every later attempt.
func TestAbandonHookProgressSnapshotCapturesIdentityWhenNoneProvided(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	first := publishHookProgressForTest(t, tree, "owner", "gen1")

	originalMark := hookProgressMarkFinished
	t.Cleanup(func() { hookProgressMarkFinished = originalMark })
	hookProgressMarkFinished = func(path string, data []byte, mode os.FileMode) error {
		if path == finishedMarker(first) {
			return errors.New("marker storage unavailable")
		}
		return originalMark(path, data, mode)
	}

	// First attempt with no snapshot: read the owned journal, capture its
	// identity, fail to terminalize (transient storage).
	done, captured, err := abandonHookProgressSnapshot(tree, "owner", nil)
	if done || err == nil {
		t.Fatalf("first attempt = done %v, err %v; want inconclusive failure capturing identity", done, err)
	}
	if captured == nil || captured.Directory != first.Directory || captured.Generation != first.Generation {
		t.Fatalf("first attempt did not capture the owned journal identity: %+v", captured)
	}

	// Same-session rebuild publishes a replacement at the same path while the
	// abandonment is deferred.
	second := publishHookProgressForTest(t, tree, "owner", "gen2")

	// Restore marker storage so a regression would actually terminalize the
	// replacement journal.
	hookProgressMarkFinished = originalMark

	done, _, err = abandonHookProgressSnapshot(tree, "owner", captured)
	if err != nil {
		t.Fatalf("second abandonment attempt: %v", err)
	}
	if !done {
		t.Fatal("replacement journal did not conclude the abandonment (skip must be conclusive)")
	}
	assertRebuiltJournalResumable(t, tree, "owner", second)
}

// TestRetryHookProgressAbandonmentLeavesRebuiltJournalPending exercises the
// synchronous-failure arm (readErr == nil, p != nil): AbandonHookProgress reads
// a journal, fails to terminalize it, and schedules retryHookProgressAbandonment
// with the snapshot. A same-session rebuild publishing a replacement during the
// retry window must not be terminalized by a later attempt.
func TestRetryHookProgressAbandonmentLeavesRebuiltJournalPending(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	tree := g.worktreePath
	first := publishHookProgressForTest(t, tree, "owner", "gen1")

	originalMark := hookProgressMarkFinished
	firstMark := make(chan struct{})
	var firstOnce sync.Once
	t.Cleanup(func() { hookProgressMarkFinished = originalMark })
	hookProgressMarkFinished = func(path string, data []byte, mode os.FileMode) error {
		firstOnce.Do(func() { close(firstMark) })
		if path == finishedMarker(first) {
			return errors.New("marker storage unavailable")
		}
		return originalMark(path, data, mode)
	}

	g.AbandonHookProgress()
	if g.hooksRetirementDone == nil {
		t.Fatal("failed abandonment did not schedule a retry")
	}
	<-firstMark // synchronous terminalization of the original journal failed

	// Same-session rebuild publishes a replacement journal at the same path
	// while the abandonment retry is pending.
	second := publishHookProgressForTest(t, tree, "owner", "gen2")

	// Restore marker storage so a regression would terminalize the replacement.
	hookProgressMarkFinished = originalMark

	waitForClosed(t, g.hooksRetirementDone, 15*time.Second, "abandonment retry did not finish")
	assertRebuiltJournalResumable(t, tree, "owner", second)
}

// TestRetryHookProgressAbandonmentFromBudgetTimeoutLeavesRebuiltJournalPending
// exercises the p == nil arm: the shared restore read missed its budget before
// decoding a journal, so installHookProgressAbandonment schedules the retry with
// no snapshot. The first locked attempt must capture the original journal's
// identity; a replacement published afterwards must be left to its own runner.
func TestRetryHookProgressAbandonmentFromBudgetTimeoutLeavesRebuiltJournalPending(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	tree := g.worktreePath
	first := publishHookProgressForTest(t, tree, "owner", "gen1")

	originalMark := hookProgressMarkFinished
	firstMark := make(chan struct{})
	var firstOnce sync.Once
	t.Cleanup(func() { hookProgressMarkFinished = originalMark })
	hookProgressMarkFinished = func(path string, data []byte, mode os.FileMode) error {
		firstOnce.Do(func() { close(firstMark) })
		if path == finishedMarker(first) {
			return errors.New("marker storage unavailable")
		}
		return originalMark(path, data, mode)
	}

	// Exact production input the timed-out batch read produces: p == nil with a
	// non-noResumable readErr.
	g.installHookProgressAbandonment(tree, "owner", nil, errors.New("shared hook journal restore budget expired: context deadline exceeded"))
	if g.hooksRetirementDone == nil {
		t.Fatal("budget-timeout abandonment did not schedule a retry")
	}
	<-firstMark // first locked attempt captured the original identity and failed

	// Same-session rebuild publishes a replacement journal at the same path
	// after the first attempt captured the original identity.
	second := publishHookProgressForTest(t, tree, "owner", "gen2")

	// Restore marker storage so a regression would terminalize the replacement.
	hookProgressMarkFinished = originalMark

	waitForClosed(t, g.hooksRetirementDone, 15*time.Second, "abandonment retry did not finish")
	assertRebuiltJournalResumable(t, tree, "owner", second)
}

// TestRetryHookProgressAbandonmentStillTerminalizesOriginalJournal confirms the
// retry still terminalizes the original journal when no rebuild happens — the
// no-regression guarantee for the bounded retry.
func TestRetryHookProgressAbandonmentStillTerminalizesOriginalJournal(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	tree := g.worktreePath
	first := publishHookProgressForTest(t, tree, "owner", "gen1")

	originalMark := hookProgressMarkFinished
	var failed atomic.Bool
	t.Cleanup(func() { hookProgressMarkFinished = originalMark })
	hookProgressMarkFinished = func(path string, data []byte, mode os.FileMode) error {
		if !failed.Swap(true) {
			return errors.New("marker storage unavailable")
		}
		return originalMark(path, data, mode)
	}

	g.AbandonHookProgress()
	if g.hooksRetirementDone == nil {
		t.Fatal("failed abandonment did not schedule a retry")
	}
	waitForClosed(t, g.hooksRetirementDone, 15*time.Second, "abandonment retry did not finish")
	if !first.finished() {
		t.Fatal("abandonment retry did not terminalize the original journal")
	}
	path, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	pruneHookProgress(filepath.Dir(path), time.Now().Add(48*time.Hour))
	for _, stale := range []string{path, first.Directory} {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatalf("abandoned journal was not reclaimable: %s: %v", stale, err)
		}
	}
}

// holdFirstAbandonmentRetry parks the first abandonment retry attempt before it
// takes the lock, and returns a channel closed once it is parked plus a release
// function. Later attempts do not wait. The cleanup releases the hold and joins
// the retry before restoring the seam, which the retry reads on every attempt.
func holdFirstAbandonmentRetry(t *testing.T, g *GitWorktree) (<-chan struct{}, func()) {
	t.Helper()
	originalWait := hookProgressAbandonmentRetryWait
	parked := make(chan struct{})
	release := make(chan struct{})
	var parkOnce, releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() { hookProgressAbandonmentRetryWait = originalWait })
	hookProgressAbandonmentRetryWait = func(time.Duration) {
		parkOnce.Do(func() {
			close(parked)
			<-release
		})
	}
	t.Cleanup(func() {
		releaseFn()
		if g.hooksRetirementDone != nil {
			waitForClosed(t, g.hooksRetirementDone, 15*time.Second, "abandonment retry did not drain")
		}
	})
	return parked, releaseFn
}

// TestRetryHookProgressAbandonmentWithoutSnapshotLeavesEarlyRebuildPending is
// the window before the first retry attempt. The shared restore read missed its
// budget, so abandonment is armed with no snapshot, and a same-session rebuild
// republishes the journal before that attempt takes the lock. Nothing ties the
// journal the attempt then reads to the one abandonment was armed for, so the
// attempt must leave it to its own runner instead of terminalizing it.
func TestRetryHookProgressAbandonmentWithoutSnapshotLeavesEarlyRebuildPending(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	tree := g.worktreePath
	publishHookProgressForTest(t, tree, "owner", "gen1")
	parked, release := holdFirstAbandonmentRetry(t, g)

	// Exact production input the timed-out batch read produces: p == nil with a
	// non-noResumable readErr.
	g.installHookProgressAbandonment(tree, "owner", nil, errors.New("shared hook journal restore budget expired: context deadline exceeded"))
	if g.hooksRetirementDone == nil {
		t.Fatal("budget-timeout abandonment did not schedule a retry")
	}
	waitForClosed(t, parked, 5*time.Second, "abandonment retry did not reach its first attempt")

	rebuilt := publishHookProgressForTest(t, tree, "owner", "gen2")
	release()

	waitForClosed(t, g.hooksRetirementDone, 15*time.Second, "abandonment retry did not finish")
	assertRebuiltJournalResumable(t, tree, "owner", rebuilt)
}

// TestAbandonHookProgressReadFailureLeavesEarlyRebuildPending is the same
// window on the synchronous arm: AbandonHookProgress cannot read the journal at
// all, so its retry also starts with no snapshot.
func TestAbandonHookProgressReadFailureLeavesEarlyRebuildPending(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	tree := g.worktreePath
	publishHookProgressForTest(t, tree, "owner", "gen1")

	// The retry reads this seam too. Registered before the hold below, so LIFO
	// restores it only after that hold's cleanup has joined the retry.
	originalRead := hookProgressReadFile
	var failed atomic.Bool
	t.Cleanup(func() { hookProgressReadFile = originalRead })
	hookProgressReadFile = func(path string) ([]byte, error) {
		if !failed.Swap(true) {
			return nil, errors.New("journal storage unavailable")
		}
		return originalRead(path)
	}
	parked, release := holdFirstAbandonmentRetry(t, g)

	g.AbandonHookProgress()
	if g.hooksRetirementDone == nil {
		t.Fatal("unreadable journal did not schedule an abandonment retry")
	}
	waitForClosed(t, parked, 5*time.Second, "abandonment retry did not reach its first attempt")

	rebuilt := publishHookProgressForTest(t, tree, "owner", "gen2")
	release()

	waitForClosed(t, g.hooksRetirementDone, 15*time.Second, "abandonment retry did not finish")
	assertRebuiltJournalResumable(t, tree, "owner", rebuilt)
}

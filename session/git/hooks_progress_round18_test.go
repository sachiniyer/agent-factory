//go:build linux

package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/hooklog"
)

func TestHookProgressStorageBailoutKeepsCompletionPending(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	fastHookAdoptionPoll(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	marker := filepath.Join(tree, "second")
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, []string{
		"true",
		"echo second > " + shellQuoteForShim(marker),
	}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}

	originalOpen, originalWrite := openHookLog, hookProgressWriteFile
	originalProbe := runningHookPrefixesForResume
	var openFailed atomic.Bool
	openHookLog = func(kind hooklog.Kind) (*os.File, error) {
		if !openFailed.Swap(true) {
			return nil, errors.New("hook log unavailable")
		}
		return originalOpen(kind)
	}
	markerFailed := make(chan struct{})
	var markerFailedOnce sync.Once
	hookProgressWriteFile = func(path string, data []byte, mode os.FileMode) error {
		if filepath.Base(path) == "launch-failed" {
			markerFailedOnce.Do(func() { close(markerFailed) })
			return errors.New("receipt storage unavailable")
		}
		return originalWrite(path, data, mode)
	}
	runningHookPrefixesForResume = func(...string) ([]string, error) { return nil, errors.New("manager unavailable") }

	ctx, cancel := context.WithCancel(context.Background())
	done := runPostWorktreeHooks(ctx, hookRun{worktreePath: tree, progress: p})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("hook runner did not stop during cleanup")
		}
		openHookLog, hookProgressWriteFile = originalOpen, originalWrite
		runningHookPrefixesForResume = originalProbe
	})
	select {
	case <-markerFailed:
	case <-time.After(5 * time.Second):
		t.Fatal("launch-failure receipt write was not attempted")
	}
	select {
	case <-done:
		t.Fatal("resumable storage bailout reported HooksDone")
	case <-time.After(100 * time.Millisecond):
	}
	if p.finished() {
		t.Fatal("resumable storage bailout marked the journal finished")
	}
	// This is the lifecycle's meaningful behavior: its bounded wait expires
	// while work remains owed, so it must leave the worktree in place.
	select {
	case <-done:
		t.Fatal("lifecycle wait observed false completion")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	waitForClosed(t, done, 5*time.Second, "cancelled pending runner did not close HooksDone")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unvisited suffix ran after a storage bailout: %v", err)
	}
	if p.finished() {
		t.Fatal("cancelling the pending completion waiter finished the resumable journal")
	}
}

func TestHookProgressSuccessorWaitsForWinningReceiptExit(t *testing.T) {
	p := &hookProgress{Directory: t.TempDir()}
	started := filepath.Join(t.TempDir(), "started")
	release := filepath.Join(t.TempDir(), "release")
	command := "touch " + shellQuoteForShim(started) + "; while [ ! -f " + shellQuoteForShim(release) + " ]; do sleep 0.01; done"

	winner := exec.Command("sh", p.command(0, command)...)
	if err := winner.Start(); err != nil {
		t.Fatal(err)
	}
	winnerDone := make(chan error, 1)
	go func() { winnerDone <- winner.Wait() }()
	winnerFinished := false
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0600)
		if winnerFinished {
			return
		}
		select {
		case <-winnerDone:
		case <-time.After(5 * time.Second):
			_ = winner.Process.Kill()
		}
	})
	waitForPath(t, started, 5*time.Second)

	successor := exec.Command("sh", p.command(0, command)...)
	if err := successor.Start(); err != nil {
		t.Fatal(err)
	}
	successorDone := make(chan error, 1)
	go func() { successorDone <- successor.Wait() }()
	select {
	case err := <-successorDone:
		t.Fatalf("successor returned before the winning receipt had an exit marker: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-winnerDone; err != nil {
		t.Fatalf("winning wrapper: %v", err)
	}
	winnerFinished = true
	if err := <-successorDone; err != nil {
		t.Fatalf("successor wrapper: %v", err)
	}
}

func TestHookProgressPreviousJournalReadDoesNotWedgePublicationLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	initial, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, nil, "af-hook-owner", "first")
	if err != nil {
		t.Fatal(err)
	}
	path, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	blocked := make(chan struct{})
	var blockedOnce sync.Once
	restoreRead := SetArchiveReadFileForTest(path, func(path string) ([]byte, error) {
		blockedOnce.Do(func() { close(blocked) })
		<-release
		return os.ReadFile(path)
	})
	useRelocationIdentityTimeoutForTest(t, 60*time.Millisecond)
	result := make(chan error, 1)
	publisherDone := make(chan struct{})
	go func() {
		_, publishErr := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, nil, "af-hook-owner", "second")
		result <- publishErr
		close(publisherDone)
	}()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-publisherDone:
		case <-time.After(5 * time.Second):
			t.Error("blocked publisher did not drain")
		}
		waitForBoundedReadFlightToDrain(t, path)
		restoreRead()
	})
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("previous journal read did not reach the storage seam")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("bounded optional read prevented publication: %v", err)
		}
	case <-time.After(relocationTimeoutObservationBudget):
		t.Fatal("stalled previous-journal read wedged publication past the observation watchdog")
	}
	releaseOnce.Do(func() { close(release) })
	waitForBoundedReadFlightToDrain(t, path)
	acquired, err := config.TryWithFileLock(filepath.Join(filepath.Dir(path), ".progress"), func() error { return nil })
	if err != nil || !acquired {
		t.Fatalf("publication lock remained held: acquired=%v err=%v", acquired, err)
	}
	if _, err := os.Stat(initial.Directory); err != nil {
		t.Fatalf("inconclusive optional cleanup removed previous receipts: %v", err)
	}
}

func TestHookProgressPruneUsesOneReadDeadlineForStalledJournals(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const count = 5
	paths := make([]string, 0, count)
	for index := 0; index < count; index++ {
		p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner-" + string(rune('a'+index))}, nil, "af-hook-owner-"+string(rune('a'+index)), "test")
		if err != nil {
			t.Fatal(err)
		}
		path, _ := hookProgressPath(p.Worktree)
		paths = append(paths, path)
	}

	blocked := make(map[string]bool, len(paths))
	for _, path := range paths {
		blocked[path] = true
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	boundedReadFileFlights.Lock()
	originalRead := archiveReadFile
	archiveReadFile = func(path string) ([]byte, error) {
		if blocked[path] {
			<-release
		}
		return originalRead(path)
	}
	boundedReadFileFlights.Unlock()
	useRelocationIdentityTimeoutForTest(t, 70*time.Millisecond)
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		for _, path := range paths {
			waitForBoundedReadFlightToDrain(t, path)
		}
		boundedReadFileFlights.Lock()
		archiveReadFile = originalRead
		boundedReadFileFlights.Unlock()
	})

	started := time.Now()
	pruneHookProgress(filepath.Dir(paths[0]), time.Now().Add(48*time.Hour))
	elapsed := time.Since(started)
	if elapsed >= 3*relocationIdentityTimeout {
		t.Fatalf("%d stalled journals consumed serial read deadlines: %s", count, elapsed)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("inconclusive journal was removed: %s: %v", path, err)
		}
	}
	releaseOnce.Do(func() { close(release) })
}

func TestHookProgressSamePathReplacementStaysPending(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	fastHookAdoptionPoll(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo, tree := linkedHookWorktree(t)
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath, g.branchName = repo, tree, "hook-resume"
	marker := filepath.Join(tree, "must-not-run")
	p, err := newHookProgress(hookRun{repoPath: repo, worktreePath: tree, scopeSessionID: "owner"}, []string{
		"touch " + shellQuoteForShim(marker),
	}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repo, "worktree", "remove", "--force", tree).CombinedOutput(); err != nil {
		t.Fatalf("remove original worktree: %v: %s", err, output)
	}
	if output, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", "hook-replacement", tree).CombinedOutput(); err != nil {
		t.Fatalf("create replacement worktree: %v: %s", err, output)
	}
	replacementIdentity, err := recordHookWorktreeIdentity(repo, tree)
	if err != nil {
		t.Fatal(err)
	}
	if p.WorktreeIdentity.same(replacementIdentity) {
		t.Fatal("replacement fixture reused the original .git pointer identity")
	}

	AdoptRunningHooks([]*GitWorktree{g})
	t.Cleanup(func() {
		g.hooksCancel()
		waitForClosed(t, g.HooksDone(), 5*time.Second, "replacement identity watcher did not stop")
	})
	time.Sleep(2 * time.Second)
	requireOpen(t, g.HooksDone(), "replacement identity mismatch reported hooks complete")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("saved command ran in a different linked worktree at the same path")
	}
	if p.claimed(0) || p.finished() {
		t.Fatal("same-path replacement changed the pending journal")
	}
}

func waitForPath(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("path was not created within %s: %s", within, path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForBoundedReadFlightToDrain(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		boundedReadFileFlights.Lock()
		flight := boundedReadFileFlights.byPath[path]
		boundedReadFileFlights.Unlock()
		if flight == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bounded read flight did not drain: %s", path)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForHookTestCondition(t *testing.T, within time.Duration, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(time.Millisecond)
	}
}

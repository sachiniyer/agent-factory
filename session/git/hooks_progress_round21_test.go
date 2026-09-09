//go:build linux

package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestHookProgressExpiredClaimWaitKeepsSuffixPending(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	marker := filepath.Join(tree, "second")
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, []string{
		"true", "touch " + shellQuoteForShim(marker),
	}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.receipt(0), 0700); err != nil {
		t.Fatal(err)
	}
	previousTimeout := hookStopTimeout
	hookStopTimeout = 100 * time.Millisecond
	originalProbe := runningHookPrefixesForResume
	runningHookPrefixesForResume = func(...string) ([]string, error) { return []string{p.Prefix}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := runPostWorktreeHooks(ctx, hookRun{worktreePath: tree, progress: p})
	t.Cleanup(func() {
		cancel()
		waitForClosed(t, done, 5*time.Second, "pending claim waiter did not stop")
		hookStopTimeout = previousTimeout
		runningHookPrefixesForResume = originalProbe
	})
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("later entry ran while the winning claim had no exit receipt: %v", err)
	}
	requireOpen(t, done, "expired claim wait reported completion")
	if p.finished() {
		t.Fatal("expired claim wait marked the journal finished")
	}
}

func TestHookProgressLaunchFailureWaitsForCompetingClaimExit(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	fastHookAdoptionPoll(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := filepath.Join(t.TempDir(), "missing")
	marker := filepath.Join(tree, "second")
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, []string{
		"true", "touch " + shellQuoteForShim(marker),
	}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.receipt(0), 0700); err != nil {
		t.Fatal(err)
	}
	previousTimeout := hookStopTimeout
	hookStopTimeout = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := runPostWorktreeHooks(ctx, hookRun{worktreePath: tree, progress: p})
	t.Cleanup(func() {
		cancel()
		waitForClosed(t, done, 5*time.Second, "competing-claim runner did not stop")
		hookStopTimeout = previousTimeout
	})
	time.Sleep(100 * time.Millisecond)
	requireOpen(t, done, "start failure advanced before the competing claim finished")
	if err := os.Mkdir(tree, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.receipt(0), "exit"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	requireOpen(t, done, "start failure accepted an invalid competing exit receipt")
	if err := config.AtomicWriteFile(filepath.Join(p.receipt(0), "exit"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	waitForClosed(t, done, 5*time.Second, "runner did not continue after the competing exit receipt")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("suffix did not run after the competing claim finished: %v", err)
	}
}

func TestHookProgressNestedCompletionRetriesFinishedRead(t *testing.T) {
	claimDaemonProcess(t)
	logPath := installScopeShim(t)
	shimDir := filepath.Dir(logPath)
	first := filepath.Join(t.TempDir(), "first-launch")
	launcher := "#!/bin/sh\nif [ \"${1:-}\" = \"--help\" ]; then printf '%s\\n' '    --expand-environment=BOOL'; exit 0; fi\n" +
		"if [ ! -f '" + first + "' ]; then touch '" + first + "'; exit 0; fi\n" +
		"while [ \"$#\" -gt 0 ]; do case \"$1\" in --user|--scope|--quiet|--collect|--expand-environment=no|--unit=*|--property=*) shift ;; --) shift; break ;; *) break ;; esac; done\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "systemd-run"), []byte(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	originalStop, originalProbe := stopHookScopeUnits, runningHookPrefixesForResume
	originalLstat := hookProgressFinishedLstat
	stopHookScopeUnits = func(...string) error { return errors.New("manager unavailable") }
	runningHookPrefixesForResume = func(...string) ([]string, error) { return nil, nil }
	var failed atomic.Bool
	hookProgressFinishedLstat = func(path string) (os.FileInfo, error) {
		if path == filepath.Join(p.Directory, "finished") && !failed.Swap(true) {
			return nil, errors.New("transient finished-marker read")
		}
		return originalLstat(path)
	}
	previousPoll := hookAdoptionPollInterval
	hookAdoptionPollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var done <-chan struct{}
	t.Cleanup(func() {
		cancel()
		if done != nil {
			waitForClosed(t, done, 5*time.Second, "nested completion runner did not stop")
		}
		stopHookScopeUnits, runningHookPrefixesForResume = originalStop, originalProbe
		hookProgressFinishedLstat = originalLstat
		hookAdoptionPollInterval = previousPoll
	})
	done = runPostWorktreeHooks(ctx, hookRun{worktreePath: tree, progress: p})
	waitForClosed(t, done, 2*time.Second, "nested completion did not recover from a transient finished-marker read")
	if !failed.Load() {
		t.Fatal("finished-marker failure seam was not exercised")
	}
}

func TestHookProgressSupersededCleanupDoesNotHoldPublicationLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	previous, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, nil, "af-hook-owner", "first")
	if err != nil {
		t.Fatal(err)
	}
	previous.finish()
	path, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	originalRemove := supersededHookProgressRemoveAll
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	supersededHookProgressRemoveAll = func(path string) error {
		if path == previous.Directory {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}
		return originalRemove(path)
	}
	publisherDone := make(chan error, 1)
	publisherFinished := make(chan struct{})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		<-publisherFinished
		supersededHookProgressRemoveAll = originalRemove
	})
	go func() {
		_, publishErr := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, nil, "af-hook-owner", "second")
		publisherDone <- publishErr
		close(publisherFinished)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("superseded receipt cleanup did not start")
	}
	acquired, err := config.TryWithFileLock(filepath.Join(filepath.Dir(path), ".progress"), func() error { return nil })
	if err != nil || !acquired {
		t.Fatalf("superseded receipt cleanup retained the publication lock: acquired=%v err=%v", acquired, err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-publisherDone; err != nil {
		t.Fatal(err)
	}
}

func TestHookProgressRelocationBlockedAdoptionRetriesAfterResolution(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	fastHookAdoptionPoll(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo, tree := linkedHookWorktree(t)
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath, g.branchName = repo, tree, "hook-resume"
	marker := filepath.Join(tree, "ran")
	p, err := newHookProgress(hookRun{repoPath: repo, worktreePath: tree, scopeSessionID: "owner"}, []string{
		"touch " + shellQuoteForShim(marker),
	}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.RestoreRelocationRecovery(RelocationRecovery{State: RelocationRecoveryStalled}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		g.hooksCancel()
		if g.HooksDone() != nil {
			waitForClosed(t, g.HooksDone(), 5*time.Second, "relocation-blocked watcher did not stop")
		}
	})
	AdoptRunningHooks([]*GitWorktree{g})
	requireOpen(t, g.HooksDone(), "relocation-blocked journal was reported complete")
	select {
	case <-g.HooksDone():
		t.Fatal("task lifecycle could terminalize a relocation-blocked journal")
	case <-time.After(100 * time.Millisecond):
	}
	if p.claimed(0) || p.finished() {
		t.Fatal("relocation-blocked adoption changed the pending journal")
	}
	claim, err := g.ClaimRelocationSource()
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SettleRelocationClaim(claim); err != nil {
		t.Fatal(err)
	}
	waitForClosed(t, g.HooksDone(), 5*time.Second, "resolved relocation did not retry hook adoption")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("resolved relocation did not run the pending suffix: %v", err)
	}
}

func TestHookProgressJournalResolutionIsBounded(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	originalResolve := boundedResolvePath
	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 50 * time.Millisecond
	entered, release, workerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	boundedResolvePath = func(path string) string {
		enteredOnce.Do(func() { close(entered) })
		<-release
		close(workerDone)
		return originalResolve(path)
	}
	done := make(chan error, 1)
	resultReceived := false
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		<-workerDone
		if !resultReceived {
			<-done
		}
		boundedResolvePath = originalResolve
		relocationIdentityTimeout = previousTimeout
	})
	go func() {
		_, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("journal path resolution did not start")
	}
	select {
	case err := <-done:
		resultReceived = true
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("bounded journal resolution error = %v, want deadline exceeded", err)
		}
	case <-time.After(3 * relocationIdentityTimeout):
		t.Fatal("journal path resolution exceeded the shared identity deadline")
	}
}

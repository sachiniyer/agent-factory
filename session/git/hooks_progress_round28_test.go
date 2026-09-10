//go:build linux

package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestHookProgressFailedPublicationRollbackReleasesProgressLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo, tree := linkedHookWorktree(t)
	journal, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	originalSync := hookProgressSyncDirectory
	originalDiscard := hookProgressDiscardPrepared
	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 50 * time.Millisecond
	hookProgressSyncDirectory = func(string) error { return errors.New("journal sync failed") }
	rollbackEntered := make(chan struct{})
	releaseRollback := make(chan struct{})
	rollbackDrained := make(chan struct{})
	var releaseOnce sync.Once
	hookProgressDiscardPrepared = func(prepared *preparedHookProgress, path string, renamed bool) {
		close(rollbackEntered)
		<-releaseRollback
		originalDiscard(prepared, path, renamed)
		close(rollbackDrained)
	}
	// Registered BEFORE the draining cleanup below, so LIFO runs it AFTER.
	// A t.Fatal in that drain calls runtime.Goexit and skips the rest of
	// ITS OWN body, but cannot skip a separately registered cleanup (#4160).
	t.Cleanup(func() {
		hookProgressSyncDirectory = originalSync
		hookProgressDiscardPrepared = originalDiscard
		relocationIdentityTimeout = previousTimeout
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseRollback) })
		select {
		case <-rollbackDrained:
		case <-time.After(5 * time.Second):
			t.Fatal("publication rollback did not drain")
		}
	})

	result := make(chan error, 1)
	go func() {
		_, err := newHookProgress(
			hookRun{repoPath: repo, worktreePath: tree, scopeSessionID: "owner"},
			[]string{"true"}, "af-hook-owner", "test",
		)
		result <- err
	}()
	select {
	case <-rollbackEntered:
	case <-time.After(time.Second):
		t.Fatal("failed publication did not reach rollback")
	}
	acquired, err := config.TryWithFileLock(filepath.Join(filepath.Dir(journal), ".progress"), func() error { return nil })
	if err != nil || !acquired {
		t.Fatalf("publication rollback retained progress lock: acquired=%v err=%v", acquired, err)
	}
	releaseOnce.Do(func() { close(releaseRollback) })
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("failed journal sync reported publication success")
		}
	case <-time.After(time.Second):
		t.Fatal("publication did not return after rollback drained")
	}
}

func TestHookProgressTerminalMarkerWriteIsBoundedBeforeHooksDone(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	p, err := newHookProgress(
		hookRun{worktreePath: tree, scopeSessionID: "owner"}, nil,
		"af-hook-owner", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	originalMark := hookProgressMarkFinished
	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 50 * time.Millisecond
	entered := make(chan struct{})
	release := make(chan struct{})
	drained := make(chan struct{})
	var releaseOnce sync.Once
	hookProgressMarkFinished = func(path string, data []byte, mode os.FileMode) error {
		close(entered)
		<-release
		defer close(drained)
		return originalMark(path, data, mode)
	}
	// Registered BEFORE the draining cleanup below, so LIFO runs it AFTER.
	// A t.Fatal in that drain calls runtime.Goexit and skips the rest of
	// ITS OWN body, but cannot skip a separately registered cleanup (#4160).
	t.Cleanup(func() {
		hookProgressMarkFinished = originalMark
		relocationIdentityTimeout = previousTimeout
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Fatal("terminal-marker write did not drain")
		}
	})

	done := runPostWorktreeHooks(context.Background(), hookRun{worktreePath: tree, progress: p})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("runner did not reach terminal-marker write")
	}
	waitForClosed(t, done, 3*relocationIdentityTimeout, "terminal-marker write held HooksDone past its bound")
	if p.finished() {
		t.Fatal("inconclusive terminal-marker write reported a finished journal")
	}
}

func TestHookProgressTimedOutLeaseCloseDoesNotHoldFlightMutex(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.lock")
	secondPath := filepath.Join(dir, "second.lock")
	originalOpen := hookProgressOpenLeaseFile
	originalClose := hookProgressCloseLeaseFile
	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 50 * time.Millisecond
	openEntered := make(chan struct{})
	releaseOpen := make(chan struct{})
	closeEntered := make(chan struct{})
	releaseClose := make(chan struct{})
	closeDrained := make(chan struct{})
	var openOnce, closeOnce, releaseOpenOnce, releaseCloseOnce sync.Once
	hookProgressOpenLeaseFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
		if path == firstPath {
			openOnce.Do(func() { close(openEntered) })
			<-releaseOpen
		}
		return originalOpen(path, flags, mode)
	}
	hookProgressCloseLeaseFile = func(file *os.File) error {
		if file.Name() == firstPath {
			closeOnce.Do(func() { close(closeEntered) })
			<-releaseClose
			defer close(closeDrained)
		}
		return originalClose(file)
	}
	// Registered BEFORE the draining cleanup below, so LIFO runs it AFTER.
	// A t.Fatal in that drain calls runtime.Goexit and skips the rest of
	// ITS OWN body, but cannot skip a separately registered cleanup (#4160).
	t.Cleanup(func() {
		hookProgressOpenLeaseFile = originalOpen
		hookProgressCloseLeaseFile = originalClose
		relocationIdentityTimeout = previousTimeout
	})
	t.Cleanup(func() {
		releaseOpenOnce.Do(func() { close(releaseOpen) })
		releaseCloseOnce.Do(func() { close(releaseClose) })
		select {
		case <-closeDrained:
		case <-time.After(5 * time.Second):
			t.Fatal("timed-out lease close did not drain")
		}
	})

	firstResult := make(chan error, 1)
	go func() {
		_, err := boundedOpenHookProgressLease(firstPath, os.O_CREATE|os.O_RDWR, 0600)
		firstResult <- err
	}()
	select {
	case <-openEntered:
	case <-time.After(time.Second):
		t.Fatal("lease open did not reach stalled seam")
	}
	select {
	case err := <-firstResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first lease open error = %v, want deadline", err)
		}
	case <-time.After(3 * relocationIdentityTimeout):
		t.Fatal("first lease open did not time out")
	}
	releaseOpenOnce.Do(func() { close(releaseOpen) })
	select {
	case <-closeEntered:
	case <-time.After(time.Second):
		t.Fatal("timed-out lease descriptor did not reach close")
	}

	type openResult struct {
		file *os.File
		err  error
	}
	secondResult := make(chan openResult, 1)
	go func() {
		file, err := boundedOpenHookProgressLease(secondPath, os.O_CREATE|os.O_RDWR, 0600)
		secondResult <- openResult{file: file, err: err}
	}()
	select {
	case result := <-secondResult:
		if result.err != nil {
			t.Fatal(result.err)
		}
		_ = result.file.Close()
	case <-time.After(3 * relocationIdentityTimeout):
		t.Fatal("timed-out descriptor close held the lease-flight mutex")
	}
	releaseCloseOnce.Do(func() { close(releaseClose) })
}

func TestHookProgressLockFileOpenIsBounded(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	journal, err := hookProgressPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(journal)
	if err := config.MkdirAllUnderAFHome(dir, 0700); err != nil {
		t.Fatal(err)
	}
	originalOpen := hookProgressOpenLockFile
	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 50 * time.Millisecond
	entered := make(chan struct{})
	release := make(chan struct{})
	drained := make(chan struct{})
	var releaseOnce sync.Once
	hookProgressOpenLockFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
		close(entered)
		<-release
		defer close(drained)
		return originalOpen(path, flags, mode)
	}
	// Registered BEFORE the draining cleanup below, so LIFO runs it AFTER.
	// A t.Fatal in that drain calls runtime.Goexit and skips the rest of
	// ITS OWN body, but cannot skip a separately registered cleanup (#4160).
	t.Cleanup(func() {
		hookProgressOpenLockFile = originalOpen
		relocationIdentityTimeout = previousTimeout
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Fatal("lock-file open did not drain")
		}
	})

	callbackRan := false
	result := make(chan error, 1)
	go func() {
		result <- withHookProgressLock(dir, func(string, os.FileInfo) error {
			callbackRan = true
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("progress lock did not reach stalled lock-file open")
	}
	select {
	case err := <-result:
		if !errors.Is(err, config.ErrLockTimeout) {
			t.Fatalf("progress lock error = %v, want lock timeout", err)
		}
	case <-time.After(3 * relocationIdentityTimeout):
		releaseOnce.Do(func() { close(release) })
		<-result
		t.Fatal("stalled lock-file open exceeded the progress-lock bound")
	}
	if callbackRan {
		t.Fatal("progress callback ran after lock-file open timed out")
	}
}

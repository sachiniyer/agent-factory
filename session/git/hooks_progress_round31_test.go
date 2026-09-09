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
)

func TestHookProgressTimedOutPreparationUsesCapturedIO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "progress.json")
	originalPrepare := hookProgressPrepare
	originalOpen := hookProgressOpenLeaseFile
	originalClose := hookProgressCloseLeaseFile
	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 50 * time.Millisecond
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	hookProgressPrepare = func(run hookRun, commands []string, prefix, generation, path string, identity *hookWorktreeIdentity, resumeDisabled bool, io hookProgressPrepareIO) (*preparedHookProgress, error) {
		close(entered)
		<-release
		return originalPrepare(run, commands, prefix, generation, path, identity, resumeDisabled, io)
	}
	var beforeOpen, afterOpen, beforeClose, afterClose atomic.Int32
	hookProgressOpenLeaseFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
		beforeOpen.Add(1)
		return originalOpen(path, flags, mode)
	}
	hookProgressCloseLeaseFile = func(file *os.File) error {
		beforeClose.Add(1)
		return originalClose(file)
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		waitForHookProgressPrepareFlight(t, dir)
		waitForHookTestCondition(t, 5*time.Second, func() bool {
			return beforeClose.Load()+afterClose.Load() == 2
		}, "timed-out preparation did not close its private files")
		hookProgressPrepare = originalPrepare
		hookProgressOpenLeaseFile = originalOpen
		hookProgressCloseLeaseFile = originalClose
		relocationIdentityTimeout = previousTimeout
	})

	result := make(chan error, 1)
	go func() {
		_, err := boundedPrepareHookProgress(
			hookRun{worktreePath: dir, scopeSessionID: "owner", leaseProgress: true},
			[]string{"true"}, "af-hook-owner", "test", path, nil, true,
		)
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("journal preparation did not reach the stalled seam")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("preparation error = %v, want deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("journal preparation did not return at its bound")
	}

	// The caller has returned. A worker that captured its dependencies must not
	// consult either replacement while it finishes and cleans its private files.
	hookProgressOpenLeaseFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
		afterOpen.Add(1)
		return originalOpen(path, flags, mode)
	}
	hookProgressCloseLeaseFile = func(file *os.File) error {
		afterClose.Add(1)
		return originalClose(file)
	}
	releaseOnce.Do(func() { close(release) })
	waitForHookProgressPrepareFlight(t, dir)
	waitForHookTestCondition(t, 5*time.Second, func() bool {
		return beforeClose.Load()+afterClose.Load() == 2
	}, "timed-out preparation did not close its private files")
	if got := afterOpen.Load(); got != 0 {
		t.Errorf("orphaned preparation used a replacement open seam %d time(s)", got)
	}
	if got := afterClose.Load(); got != 0 {
		t.Errorf("orphaned preparation used a replacement close seam %d time(s)", got)
	}
	if got := beforeOpen.Load(); got != 1 {
		t.Errorf("captured open seam calls = %d, want 1", got)
	}
	if got := beforeClose.Load(); got != 2 {
		t.Errorf("captured close seam calls = %d, want 2", got)
	}
}

func waitForHookProgressPrepareFlight(t *testing.T, directory string) {
	t.Helper()
	waitForHookTestCondition(t, 5*time.Second, func() bool {
		hookProgressPrepareFlights.Lock()
		active := hookProgressPrepareFlights.byDirectory[directory]
		hookProgressPrepareFlights.Unlock()
		return active == nil
	}, "hook progress preparation flight did not drain")
}

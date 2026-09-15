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

func TestHookProgressDirectorySyncIsBoundedUnderProgressLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo, tree := linkedHookWorktree(t)
	originalSync := hookProgressSyncDirectory
	useRelocationIdentityTimeoutForTest(t, 50*time.Millisecond)
	entered := make(chan struct{})
	release := make(chan struct{})
	drained := make(chan struct{})
	var enterOnce, releaseOnce, drainOnce sync.Once
	hookProgressSyncDirectory = func(path string) error {
		enterOnce.Do(func() { close(entered) })
		<-release
		drainOnce.Do(func() { close(drained) })
		return originalSync(path)
	}
	// Registered BEFORE the draining cleanup below, so LIFO runs it AFTER.
	// A t.Fatal in that drain calls runtime.Goexit and skips the rest of
	// ITS OWN body, but cannot skip a separately registered cleanup (#4160).
	t.Cleanup(func() {
		hookProgressSyncDirectory = originalSync
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Fatal("blocked directory sync did not drain")
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
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("journal publication did not reach the stalled directory sync")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("publication error = %v, want deadline exceeded", err)
		}
	case <-time.After(3 * relocationIdentityTimeout):
		releaseOnce.Do(func() { close(release) })
		<-result
		t.Fatal("stalled directory sync retained the progress lock past its bound")
	}

	journal, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("inconclusively synced journal remained publishable: %v", err)
	}
	acquired, err := config.TryWithFileLock(filepath.Join(filepath.Dir(journal), ".progress"), func() error { return nil })
	if err != nil || !acquired {
		t.Fatalf("progress lock remained held after sync timeout: acquired=%v err=%v", acquired, err)
	}
}

func TestHookProgressRetirementSyncIsBoundedUnderProgressLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	p, err := newHookProgress(
		hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner"},
		[]string{"true"}, "af-hook-owner", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := hookProgressPath(p.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	originalSync := hookProgressSyncDirectory
	useRelocationIdentityTimeoutForTest(t, 50*time.Millisecond)
	entered := make(chan struct{})
	release := make(chan struct{})
	drained := make(chan struct{})
	var enterOnce, releaseOnce, drainOnce sync.Once
	hookProgressSyncDirectory = func(path string) error {
		enterOnce.Do(func() { close(entered) })
		<-release
		drainOnce.Do(func() { close(drained) })
		return originalSync(path)
	}
	// Registered BEFORE the draining cleanup below, so LIFO runs it AFTER.
	// A t.Fatal in that drain calls runtime.Goexit and skips the rest of
	// ITS OWN body, but cannot skip a separately registered cleanup (#4160).
	t.Cleanup(func() {
		hookProgressSyncDirectory = originalSync
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Fatal("blocked retirement sync did not drain")
		}
	})

	result := make(chan error, 1)
	go func() {
		result <- withHookProgressLock(filepath.Dir(journal), func(string, os.FileInfo) error {
			return removeHookProgress(journal, p)
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("retirement did not reach the stalled directory sync")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("retirement error = %v, want deadline exceeded", err)
		}
	case <-time.After(3 * relocationIdentityTimeout):
		releaseOnce.Do(func() { close(release) })
		<-result
		t.Fatal("stalled retirement sync retained the progress lock past its bound")
	}
	if _, err := os.Stat(p.Directory); err != nil {
		t.Fatalf("receipts were removed after inconclusive retirement sync: %v", err)
	}
	acquired, err := config.TryWithFileLock(filepath.Join(filepath.Dir(journal), ".progress"), func() error { return nil })
	if err != nil || !acquired {
		t.Fatalf("progress lock remained held after retirement timeout: acquired=%v err=%v", acquired, err)
	}
}

func TestHookProgressPreparationIsBoundedUnderProgressLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo, tree := linkedHookWorktree(t)
	journal, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	journalDirectory := filepath.Dir(journal)
	originalPrepare := hookProgressPrepare
	useRelocationIdentityTimeoutForTest(t, 50*time.Millisecond)
	entered := make(chan struct{})
	release := make(chan struct{})
	drained := make(chan struct{})
	var releaseOnce sync.Once
	hookProgressPrepare = func(run hookRun, commands []string, prefix, generation, path string, identity *hookWorktreeIdentity, resumeDisabled bool, io hookProgressPrepareIO) (*preparedHookProgress, error) {
		close(entered)
		<-release
		defer close(drained)
		return originalPrepare(run, commands, prefix, generation, path, identity, resumeDisabled, io)
	}
	// Registered BEFORE the draining cleanup below, so LIFO runs it AFTER.
	// A t.Fatal in that drain calls runtime.Goexit and skips the rest of
	// ITS OWN body, but cannot skip a separately registered cleanup (#4160).
	t.Cleanup(func() {
		hookProgressPrepare = originalPrepare
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Fatal("blocked journal preparation did not drain")
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			hookProgressPrepareFlights.Lock()
			active := hookProgressPrepareFlights.byDirectory[journalDirectory]
			hookProgressPrepareFlights.Unlock()
			if active == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("journal preparation flight did not drain")
			}
			time.Sleep(10 * time.Millisecond)
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
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("journal publication did not reach the stalled preparation")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("publication error = %v, want deadline exceeded", err)
		}
	case <-time.After(3 * relocationIdentityTimeout):
		releaseOnce.Do(func() { close(release) })
		<-result
		t.Fatal("stalled journal preparation retained the progress lock past its bound")
	}

	acquired, err := config.TryWithFileLock(filepath.Join(journalDirectory, ".progress"), func() error { return nil })
	if err != nil || !acquired {
		t.Fatalf("progress lock remained held after preparation timeout: acquired=%v err=%v", acquired, err)
	}
}

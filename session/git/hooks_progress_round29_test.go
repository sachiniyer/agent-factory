//go:build linux

package git

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestHookProgressRetirementBoundsProgressLockOpen(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	p, err := newHookProgress(
		hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner"},
		nil, "af-hook-owner", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	path, err := hookProgressPath(p.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	originalOpen := hookProgressOpenLockFile
	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 50 * time.Millisecond
	entered := make(chan struct{})
	release := make(chan struct{})
	drained := make(chan struct{})
	var enterOnce, releaseOnce, drainOnce sync.Once
	hookProgressOpenLockFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
		enterOnce.Do(func() { close(entered) })
		<-release
		defer drainOnce.Do(func() { close(drained) })
		return originalOpen(path, flags, mode)
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-entered:
			select {
			case <-drained:
			case <-time.After(5 * time.Second):
				t.Error("teardown lock-file open did not drain")
			}
		default:
		}
		hookProgressOpenLockFile = originalOpen
		relocationIdentityTimeout = previousTimeout
	})

	type result struct {
		acquired bool
		err      error
	}
	results := make(chan result, 1)
	go func() {
		acquired, err := retireHookProgressSnapshot(p, path)
		results <- result{acquired: acquired, err: err}
	}()
	select {
	case <-entered:
	case result := <-results:
		t.Fatalf("teardown bypassed the bounded progress-lock opener: acquired=%v err=%v", result.acquired, result.err)
	case <-time.After(time.Second):
		t.Fatal("teardown did not reach progress-lock open")
	}
	select {
	case result := <-results:
		if result.acquired || !errors.Is(result.err, config.ErrLockTimeout) {
			t.Fatalf("teardown lock result = acquired %v, err %v; want bounded timeout", result.acquired, result.err)
		}
	case <-time.After(3 * relocationIdentityTimeout):
		t.Fatal("teardown progress-lock open exceeded its bound")
	}
	releaseOnce.Do(func() { close(release) })
}

func TestFailedPublicationJournalCannotResumeAfterRollbackRenameIsLost(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo, tree := linkedHookWorktree(t)
	path, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	originalSync := hookProgressSyncDirectory
	injected := errors.New("journal directory sync failed")
	hookProgressSyncDirectory = func(string) error { return injected }
	t.Cleanup(func() { hookProgressSyncDirectory = originalSync })

	_, err = newHookProgress(
		hookRun{repoPath: repo, worktreePath: tree, scopeSessionID: "owner"},
		[]string{"true"}, "af-hook-owner", "test",
	)
	if !errors.Is(err, injected) {
		t.Fatalf("publication error = %v, want %v", err, injected)
	}
	retired, err := filepath.Glob(filepath.Join(filepath.Dir(path), "retired-entries-*.json"))
	if err != nil || len(retired) != 1 {
		t.Fatalf("retired rollback journals = %v, %v; want one", retired, err)
	}
	// Model a host crash losing the unsynced rollback rename: the original
	// resumable name is what recovery sees.
	if err := os.Rename(retired[0], path); err != nil {
		t.Fatal(err)
	}
	p, err := readPendingHookProgress(tree, "owner")
	if !errors.Is(err, errHookProgressResumeDisabled) || p == nil {
		t.Fatalf("reappearing failed publication = progress %v, err %v; want explicit unresumable journal", p, err)
	}
}

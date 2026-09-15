//go:build linux

package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/hooklog"
	"github.com/sachiniyer/agent-factory/internal/systemdunit"
)

func TestHookProgressResumableBailoutKeepsLeaseUntilRecoveryEnds(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner", leaseProgress: true}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	originalOpen, originalWrite := openHookLog, hookProgressWriteFile
	originalProbe := runningHookPrefixesForResume
	openHookLog = func(hooklog.Kind) (*os.File, error) { return nil, errors.New("hook log unavailable") }
	runningHookPrefixesForResume = func(...string) ([]string, error) { return nil, errors.New("manager unavailable") }
	var failed atomic.Bool
	hookProgressWriteFile = func(path string, data []byte, mode os.FileMode) error {
		if filepath.Base(path) == "launch-failed" && !failed.Swap(true) {
			return errors.New("marker storage unavailable")
		}
		return originalWrite(path, data, mode)
	}
	t.Cleanup(func() {
		openHookLog, hookProgressWriteFile = originalOpen, originalWrite
		runningHookPrefixesForResume = originalProbe
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := runPostWorktreeHooks(ctx, hookRun{worktreePath: tree, progress: p})
	waitForHookTestCondition(t, 5*time.Second, failed.Load, "resumable receipt failure was not reached")
	requireOpen(t, done, "resumable bailout reported completion")
	if p.finished() {
		t.Fatal("resumable bailout marked the journal finished")
	}
	lease, err := os.OpenFile(filepath.Join(p.Directory, "runner.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("recovery owner did not retain the runner lease: %v", err)
	}
	cancel()
	waitForClosed(t, done, 5*time.Second, "cancelled resumable bailout did not close")
	if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("runner lease was not released after recovery ended: %v", err)
	}
	if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	path, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	pruneHookProgress(filepath.Dir(path), time.Now().Add(48*time.Hour))
	for _, stale := range []string{path, p.Directory} {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatalf("unleased ownerless journal was not pruned: %s: %v", stale, err)
		}
	}
}

func TestAdoptRunningHooksSharesOneJournalReadBudget(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	fastHookAdoptionPoll(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	type candidate struct {
		worktree *GitWorktree
		progress *hookProgress
		path     string
		cancel   context.CancelFunc
	}
	var candidates []candidate
	for i := 0; i < 4; i++ {
		repo, tree := linkedHookWorktree(t)
		g := &GitWorktree{repoPath: repo, worktreePath: tree, branchName: "hook-resume"}
		id := "batch-owner-" + string(rune('a'+i))
		g.SetHookScopeSessionID(id)
		ctx, cancel := context.WithCancel(context.Background())
		g.hooksCtx, g.hooksCancel = ctx, cancel
		prefix := systemdunit.HookScopeUnitPrefix(id)
		p, err := newHookProgress(hookRun{repoPath: repo, worktreePath: tree, scopeSessionID: id}, []string{"true"}, prefix, "test")
		if err != nil {
			t.Fatal(err)
		}
		path, err := hookProgressPath(g.worktreePath)
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate{worktree: g, progress: p, path: path, cancel: cancel})
	}
	originalRead := hookProgressReadFile
	blocked := make(map[string]bool)
	for _, candidate := range candidates[:3] {
		blocked[candidate.path] = true
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	hookProgressReadFile = func(path string) ([]byte, error) {
		if blocked[path] {
			<-release
		}
		return originalRead(path)
	}
	useRelocationIdentityTimeoutForTest(t, 80*time.Millisecond)
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		for _, candidate := range candidates {
			candidate.cancel()
			if candidate.worktree.HooksDone() != nil {
				waitForClosed(t, candidate.worktree.HooksDone(), 5*time.Second, "adoption watcher did not stop")
			}
		}
		hookProgressReadFile = originalRead
	})
	worktrees := make([]*GitWorktree, 0, len(candidates))
	for _, candidate := range candidates {
		worktrees = append(worktrees, candidate.worktree)
	}
	started := time.Now()
	AdoptRunningHooks(worktrees)
	elapsed := time.Since(started)
	if elapsed > 3*relocationIdentityTimeout {
		t.Fatalf("four adoption reads consumed serial budgets: %s", elapsed)
	}
	for i, candidate := range candidates[:3] {
		requireOpen(t, candidate.worktree.HooksDone(), "stalled journal did not remain pending")
		if candidate.progress.finished() || candidate.progress.claimed(0) {
			t.Fatalf("stalled journal %d was changed during restore", i)
		}
	}
	if candidates[3].worktree.HooksDone() == nil {
		t.Fatal("journal that completed within the shared budget was not adopted")
	}
	releaseOnce.Do(func() { close(release) })
}

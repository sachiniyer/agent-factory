//go:build linux

package git

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/hooklog"
)

func TestHookProgressNestedResumeRetainsCreateLease(t *testing.T) {
	claimDaemonProcess(t)
	logPath := installScopeShim(t)
	shimDir := filepath.Dir(logPath)
	first := filepath.Join(t.TempDir(), "first-launch")
	launcher := "#!/bin/sh\nif [ \"${1:-}\" = \"--help\" ]; then printf '%s\\n' '    --expand-environment=BOOL'; exit 0; fi\n" +
		"if [ ! -f '" + first + "' ]; then touch '" + first + "'; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(filepath.Join(shimDir, "systemd-run"), []byte(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	g := &GitWorktree{worktreePath: tree}
	g.BeginHookCreatePersistence()
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner", leaseProgress: true}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	g.retainHookProgressForCreate(p)
	originalOpen, originalWrite := openHookLog, hookProgressWriteFile
	originalStop, originalProbe := stopHookScopeUnits, runningHookPrefixesForResume
	var opens atomic.Int32
	var receiptAttempted atomic.Bool
	openHookLog = func(kind hooklog.Kind) (*os.File, error) {
		if opens.Add(1) > 1 {
			return nil, errors.New("nested hook log unavailable")
		}
		return originalOpen(kind)
	}
	hookProgressWriteFile = func(path string, data []byte, mode os.FileMode) error {
		receiptAttempted.Store(true)
		return errors.New("nested receipt unavailable")
	}
	stopHookScopeUnits = func(...string) error { return errors.New("manager unavailable") }
	runningHookPrefixesForResume = func(...string) ([]string, error) { return nil, nil }
	t.Cleanup(func() {
		openHookLog, hookProgressWriteFile = originalOpen, originalWrite
		stopHookScopeUnits, runningHookPrefixesForResume = originalStop, originalProbe
		g.SettleHookCreatePersistence()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := runPostWorktreeHooks(ctx, hookRun{worktreePath: tree, progress: p})
	waitForHookTestCondition(t, 5*time.Second, receiptAttempted.Load, "nested receipt failure was not reached")
	requireOpen(t, done, "nested resumable bailout reported completion")
	if p.finished() || p.claimed(0) {
		t.Fatal("nested resumable bailout terminalized the journal")
	}
	assertHookProgressLeaseLocked(t, p.Directory)
	var holds int
	waitForHookTestCondition(t, 5*time.Second, func() bool {
		p.leaseMu.Lock()
		defer p.leaseMu.Unlock()
		holds = p.leaseHolds
		return holds == 2
	}, "nested runner did not release its own lease hold")
	if holds != 2 {
		t.Fatalf("nested bailout left %d lease holds, want the waiting outer runner and create owner", holds)
	}
	cancel()
	waitForClosed(t, done, 5*time.Second, "cancelled nested bailout did not close")
	waitForHookTestCondition(t, 5*time.Second, func() bool {
		p.leaseMu.Lock()
		defer p.leaseMu.Unlock()
		holds = p.leaseHolds
		return holds == 1
	}, "outer runner did not release its lease after cancellation")
}

func TestHookProgressFailedClaimPublishesAtomically(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	originalWrite, originalRemove := hookProgressWriteFile, hookProgressRemoveAll
	hookProgressWriteFile = func(string, []byte, os.FileMode) error { return errors.New("marker unavailable") }
	hookProgressRemoveAll = func(string) error { return errors.New("cleanup unavailable") }
	t.Cleanup(func() {
		hookProgressWriteFile, hookProgressRemoveAll = originalWrite, originalRemove
		_ = os.RemoveAll(p.Directory)
	})
	if p.recordLaunchFailure(t.Context(), 0, errors.New("launch failed")) {
		t.Fatal("failed marker write published a claim")
	}
	if p.claimed(0) {
		t.Fatal("temporary failed-launch receipt satisfied claimed")
	}
	entries, err := os.ReadDir(p.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".launch-failed-") {
		t.Fatalf("failed receipt was not isolated under a temporary name: %v", entries)
	}
}

func TestHookProgressTerminalOwnersPermitUnfinishedReclamation(t *testing.T) {
	for _, owner := range []string{"absent", "archived", "tombstoned", "active"} {
		t.Run(owner, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			installScopeShim(t)
			rows := `[]`
			switch owner {
			case "archived":
				rows = `[{"id":"owner","liveness":5}]`
			case "tombstoned":
				rows = `[{"id":"owner","user_killed":true}]`
			case "active":
				rows = `[{"id":"owner"}]`
			}
			if err := config.SaveRepoInstances("round16-owner", json.RawMessage(rows)); err != nil {
				t.Fatal(err)
			}
			p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
			if err != nil {
				t.Fatal(err)
			}
			path, _ := hookProgressPath(p.Worktree)
			pruneHookProgress(filepath.Dir(path), time.Now().Add(2*progressGraceAge))
			_, err = os.Stat(path)
			if owner == "active" && err != nil {
				t.Fatalf("active owner lost unfinished journal: %v", err)
			}
			if owner != "active" && !os.IsNotExist(err) {
				t.Fatalf("%s owner retained unfinished journal: %v", owner, err)
			}
		})
	}
}

func TestTerminalHookAbandonmentSharesRestoreBudget(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	type candidate struct {
		worktree *GitWorktree
		progress *hookProgress
		path     string
	}
	var candidates []candidate
	blocked := make(map[string]bool)
	for i := 0; i < 3; i++ {
		id := "terminal-" + string(rune('a'+i))
		g := &GitWorktree{worktreePath: t.TempDir(), hooksResumeDisabled: true}
		g.SetHookScopeSessionID(id)
		p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: id}, []string{"true"}, "af-hook-"+id, "test")
		if err != nil {
			t.Fatal(err)
		}
		path, _ := hookProgressPath(g.worktreePath)
		blocked[path] = true
		candidates = append(candidates, candidate{worktree: g, progress: p, path: path})
	}
	originalRead := hookProgressReadFile
	release := make(chan struct{})
	var releaseOnce sync.Once
	hookProgressReadFile = func(path string) ([]byte, error) {
		if blocked[path] {
			<-release
		}
		return originalRead(path)
	}
	originalBatchReadFinished := hookProgressBatchReadFinished
	batchReadFinished := make(chan struct{}, len(candidates))
	hookProgressBatchReadFinished = func() { batchReadFinished <- struct{}{} }
	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 80 * time.Millisecond
	// Registered BEFORE the draining cleanup below, so LIFO runs it AFTER.
	// A t.Fatal in that drain calls runtime.Goexit and skips the rest of
	// ITS OWN body, but cannot skip a separately registered cleanup (#4160).
	t.Cleanup(func() {
		hookProgressReadFile = originalRead
		hookProgressBatchReadFinished = originalBatchReadFinished
		relocationIdentityTimeout = previousTimeout
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		for _, candidate := range candidates {
			if candidate.worktree.hooksRetirementDone != nil {
				waitForClosed(t, candidate.worktree.hooksRetirementDone, 5*time.Second, "terminal abandonment did not drain")
			}
		}
		// The shared startup budget returns before its outstanding reads. Join
		// those reads before restoring timeout and storage seams they consult.
		for range candidates {
			select {
			case <-batchReadFinished:
			case <-time.After(5 * time.Second):
				t.Fatal("terminal restore journal reads did not drain")
			}
		}
	})
	worktrees := make([]*GitWorktree, 0, len(candidates))
	for _, candidate := range candidates {
		worktrees = append(worktrees, candidate.worktree)
	}
	started := time.Now()
	AdoptRunningHooks(worktrees)
	if elapsed := time.Since(started); elapsed > 3*relocationIdentityTimeout {
		t.Fatalf("terminal abandonment consumed serial restore budgets: %s", elapsed)
	}
	for _, candidate := range candidates {
		if candidate.worktree.hooksRetirementDone == nil || candidate.progress.finished() {
			t.Fatal("terminal restore did not install asynchronous abandonment")
		}
	}
	releaseOnce.Do(func() { close(release) })
	for _, candidate := range candidates {
		waitForClosed(t, candidate.worktree.hooksRetirementDone, 5*time.Second, "terminal abandonment did not finish")
		if !candidate.progress.finished() {
			t.Fatal("terminal abandonment did not mark the journal finished")
		}
	}
}

func TestHookProgressPruneBoundsReceiptMetadata(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installScopeShim(t)
	p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	path, _ := hookProgressPath(p.Worktree)
	blockedPath := p.receipt(0)
	originalLstat := boundedLstatPath
	release := make(chan struct{})
	var releaseOnce sync.Once
	boundedLstatPath = func(path string) (os.FileInfo, error) {
		if path == blockedPath {
			<-release
		}
		return originalLstat(path)
	}
	previousTimeout := relocationIdentityTimeout
	relocationIdentityTimeout = 60 * time.Millisecond
	// Registered BEFORE the draining cleanup below, so LIFO runs it AFTER.
	// A t.Fatal in that drain calls runtime.Goexit and skips the rest of
	// ITS OWN body, but cannot skip a separately registered cleanup (#4160).
	t.Cleanup(func() {
		boundedLstatPath = originalLstat
		relocationIdentityTimeout = previousTimeout
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		deadline := time.Now().Add(5 * time.Second)
		for {
			boundedLstatFlights.Lock()
			flight := boundedLstatFlights.byPath[blockedPath]
			boundedLstatFlights.Unlock()
			if flight == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("blocked metadata flight did not drain")
			}
			time.Sleep(time.Millisecond)
		}
	})
	started := time.Now()
	pruneHookProgress(filepath.Dir(path), time.Now().Add(48*time.Hour))
	if elapsed := time.Since(started); elapsed > 3*relocationIdentityTimeout {
		t.Fatalf("receipt metadata stalled pruning for %s", elapsed)
	}
	for _, retained := range []string{path, p.Directory} {
		if _, err := os.Stat(retained); err != nil {
			t.Fatalf("inconclusive metadata removed %s: %v", retained, err)
		}
	}
	acquired, err := config.TryWithFileLock(filepath.Join(filepath.Dir(path), ".progress"), func() error { return nil })
	if err != nil || !acquired {
		t.Fatalf("prune did not release progress lock: acquired=%v err=%v", acquired, err)
	}
	releaseOnce.Do(func() { close(release) })
}

func TestHookProgressPublicationPinsSymlinkedHome(t *testing.T) {
	parent := t.TempDir()
	first, second := filepath.Join(parent, "first"), filepath.Join(parent, "second")
	if err := os.Mkdir(first, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(second, 0700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(parent, "home")
	if err := os.Symlink(first, home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_FACTORY_HOME", home)
	tree := t.TempDir()
	initialPath, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	originalHook := hookProgressLockAcquired
	hookProgressLockAcquired = func() {
		if err := os.Remove(home); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(second, home); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { hookProgressLockAcquired = originalHook })
	p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(first, "logs", "hooks")
	if filepath.Dir(p.Directory) != want {
		t.Fatalf("receipts escaped pinned home: got %s want parent %s", p.Directory, want)
	}
	if _, err := os.Stat(filepath.Join(want, filepath.Base(initialPath))); err != nil {
		t.Fatalf("journal was not published in pinned home: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, "logs", "hooks", filepath.Base(initialPath))); !os.IsNotExist(err) {
		t.Fatalf("journal crossed into repointed home: %v", err)
	}
}

func assertHookProgressLeaseLocked(t *testing.T, dir string) {
	t.Helper()
	lease, err := os.OpenFile(filepath.Join(dir, "runner.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("hook progress lease is not held: %v", err)
	}
}

//go:build linux

package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestHookProgressBailoutRetriesWithoutRestart(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	fastHookAdoptionPoll(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo, tree := linkedHookWorktree(t)
	first := filepath.Join(tree, "first")
	second := filepath.Join(tree, "second")
	p, err := newHookProgress(hookRun{repoPath: repo, worktreePath: tree, scopeSessionID: "owner"}, []string{
		"touch " + shellQuoteForShim(first),
		"touch " + shellQuoteForShim(second),
	}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	originalLstat := boundedLstatPath
	var failed atomic.Bool
	boundedLstatPath = func(path string) (os.FileInfo, error) {
		if path == p.receipt(0) && !failed.Swap(true) {
			return nil, errors.New("transient receipt storage error")
		}
		return originalLstat(path)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		boundedLstatPath = originalLstat
	})
	done := runPostWorktreeHooks(ctx, hookRun{repoPath: repo, worktreePath: tree, progress: p})
	waitForClosed(t, done, 3*time.Second, "transient bailout did not retry while the daemon stayed alive")
	for _, marker := range []string{first, second} {
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("resumed suffix did not run %s: %v", marker, err)
		}
	}
}

func TestHookProgressCommandSyncsClaimBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	p := &hookProgress{Directory: dir}
	bin := t.TempDir()
	syncRecord := filepath.Join(t.TempDir(), "sync-record")
	if err := os.WriteFile(filepath.Join(bin, "sync"), []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" > \"$SYNC_RECORD\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	executed := filepath.Join(t.TempDir(), "executed")
	command := "test -f " + shellQuoteForShim(syncRecord) + " && touch " + shellQuoteForShim(executed)
	args := p.command(0, command)
	cmd := exec.Command("sh", args...)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SYNC_RECORD="+syncRecord)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("operator command ran before its claim was synced: %v: %s", err, output)
	}
	data, err := os.ReadFile(syncRecord)
	if err != nil {
		t.Fatalf("claim parent was not synced: %v", err)
	}
	if strings.TrimSpace(string(data)) != dir {
		t.Fatalf("synced directory = %q, want receipt parent %q", strings.TrimSpace(string(data)), dir)
	}
	if _, err := os.Stat(executed); err != nil {
		t.Fatalf("operator command did not execute after claim sync: %v", err)
	}
}

func TestHookProgressNeverResumesClaimedEntryAfterPreCommandCrash(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	fastHookAdoptionPoll(t)
	dir := t.TempDir()
	first := filepath.Join(t.TempDir(), "first-must-not-run")
	second := filepath.Join(t.TempDir(), "second-ran")
	p := &hookProgress{
		Commands: []string{"touch " + shellQuoteForShim(first), "touch " + shellQuoteForShim(second)},
		Worktree: t.TempDir(), Prefix: "af-hook-owner", Generation: "test", Directory: dir,
	}
	bin := t.TempDir()
	syncEntered := filepath.Join(t.TempDir(), "sync-entered")
	if err := os.WriteFile(filepath.Join(bin, "sync"), []byte("#!/bin/sh\ntouch \"$SYNC_ENTERED\"\nwhile :; do sleep 1; done\n"), 0755); err != nil {
		t.Fatal(err)
	}
	claim := exec.Command("sh", p.command(0, p.Commands[0])...)
	claim.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SYNC_ENTERED="+syncEntered)
	claim.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := claim.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, syncEntered, 5*time.Second)
	if state, err := p.entryState(0); err != nil || state != hookEntryStarted {
		t.Fatalf("pre-command crash fixture state = %v, %v; want durable started claim", state, err)
	}
	_ = syscall.Kill(-claim.Process.Pid, syscall.SIGKILL)
	_ = claim.Wait()

	originalProbe := runningHookPrefixesForResume
	runningHookPrefixesForResume = func(...string) ([]string, error) { return nil, nil }
	t.Cleanup(func() { runningHookPrefixesForResume = originalProbe })
	done := runPostWorktreeHooks(t.Context(), hookRun{worktreePath: p.Worktree, progress: p})
	waitForClosed(t, done, 5*time.Second, "runner did not step over the terminalized abandoned claim")
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("claimed entry was replayed after recovery: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("unclaimed suffix did not run after terminal failure: %v", err)
	}
	if state, err := p.entryState(0); err != nil || state != hookEntryFinished {
		t.Fatalf("abandoned claim state = %v, %v; want terminal failure", state, err)
	}
	if _, err := os.Stat(filepath.Join(p.receipt(0), "launch-failed")); err != nil {
		t.Fatalf("abandoned claim has no terminal-failure marker: %v", err)
	}
}

func TestHookProgressPublicationRequiresDirectorySync(t *testing.T) {
	claimDaemonProcess(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo, tree := linkedHookWorktree(t)
	originalSync := hookProgressSyncDirectory
	injected := errors.New("journal directory sync failed")
	var calls atomic.Int32
	hookProgressSyncDirectory = func(string) error {
		calls.Add(1)
		return injected
	}
	t.Cleanup(func() { hookProgressSyncDirectory = originalSync })
	_, err := newHookProgress(hookRun{repoPath: repo, worktreePath: tree, scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
	if !errors.Is(err, injected) {
		t.Fatalf("publication error = %v, want directory sync failure", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("journal directory sync calls = %d, want 1", calls.Load())
	}
}

func TestHookProgressLeaseOpenIsBoundedUnderProgressLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	path, err := hookProgressPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := config.MkdirAllUnderAFHome(dir, 0700); err != nil {
		t.Fatal(err)
	}
	receipt, err := os.MkdirTemp(dir, "entries-")
	if err != nil {
		t.Fatal(err)
	}
	leasePath := filepath.Join(receipt, "runner.lock")
	if err := os.WriteFile(leasePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	originalOpen := hookProgressOpenLeaseFile
	release := make(chan struct{})
	entered := make(chan struct{})
	var releaseOnce sync.Once
	hookProgressOpenLeaseFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
		if path == leasePath {
			close(entered)
			<-release
		}
		return originalOpen(path, flags, mode)
	}
	useRelocationIdentityTimeoutForTest(t, 50*time.Millisecond)
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		hookProgressOpenLeaseFile = originalOpen
	})
	result := make(chan error, 1)
	go func() {
		result <- withHookProgressLock(dir, func(string, os.FileInfo) error {
			_, leaseErr := withInactiveHookProgressLease(receipt, func() error { return nil })
			return leaseErr
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("lease open did not reach the stalled seam")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("bounded lease error = %v, want deadline exceeded", err)
		}
	case <-time.After(3 * relocationIdentityTimeout):
		releaseOnce.Do(func() { close(release) })
		<-result
		t.Fatal("stalled lease open retained the progress lock past its bound")
	}
	acquired, err := config.TryWithFileLock(filepath.Join(dir, ".progress"), func() error { return nil })
	if err != nil || !acquired {
		t.Fatalf("progress lock was not released after bounded lease timeout: acquired=%v err=%v", acquired, err)
	}
	releaseOnce.Do(func() { close(release) })
}

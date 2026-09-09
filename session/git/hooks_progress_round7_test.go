//go:build linux

package git

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestHookProgressTransientReadRetries(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	fastHookAdoptionPoll(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath = linkedHookWorktree(t)
	g.branchName = "hook-resume"
	marker := filepath.Join(g.worktreePath, "ran")
	p, err := newHookProgress(hookRun{repoPath: g.repoPath, worktreePath: g.worktreePath, scopeSessionID: "owner"}, []string{"true", "echo second >> " + shellQuoteForShim(marker)}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.receipt(0), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.receipt(0), "exit"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	original := hookProgressReadFile
	var calls atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	hookProgressReadFile = func(path string) ([]byte, error) {
		if calls.Add(1) == 1 {
			return nil, &os.PathError{Op: "read", Path: path, Err: syscall.EIO}
		}
		<-release
		return original(path)
	}
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		g.hooksCancel()
		if g.HooksDone() != nil {
			waitForClosed(t, g.HooksDone(), 5*time.Second, "watcher did not stop")
		}
		hookProgressReadFile = original
	})
	AdoptRunningHooks([]*GitWorktree{g})
	if g.HooksDone() == nil {
		t.Fatal("transient journal read fell back to no hooks")
	}
	requireOpen(t, g.HooksDone(), "transient journal read reported completion")
	once.Do(func() { close(release) })
	waitForClosed(t, g.HooksDone(), 5*time.Second, "journal read was not retried")
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "second\n" {
		t.Fatalf("remaining commands = %q, err=%v", raw, err)
	}
	if !p.completed() {
		t.Fatal("retried journal did not complete")
	}
}

func TestHookProgressAbsentReadUsesLegacyImmediately(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	if g.adoptHookProgress() {
		t.Fatal("absent journal started a retry watcher")
	}
	if g.HooksDone() != nil {
		t.Fatal("absent journal reports pending provisioning")
	}
}

func TestHookProgressLiveCreateLeaseProtectsLaunchGap(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, ".git"), []byte("gitdir: test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tree, "ran")
	repo := freshRepoConfig(t, []string{"true", "echo second >> " + shellQuoteForShim(marker)})
	ctx, cancel := context.WithCancel(context.Background())
	launched, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	done := runPostWorktreeHooks(ctx, hookRun{repoPath: repo, worktreePath: tree, scopeSessionID: "owner", onScopeLaunched: func(string) { close(launched); <-release }})
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		cancel()
		waitForClosed(t, done, 5*time.Second, "runner did not stop")
	})
	waitForClosed(t, launched, 5*time.Second, "first hook did not launch")
	path, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	p, err := readHookProgress(path)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(p.receipt(0), "exit")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first hook did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The runner is paused between entries with no persisted owner and no scope
	// or launcher. Even an arbitrarily long scheduler gap must retain its state.
	pruneHookProgress(filepath.Dir(path), time.Now().Add(48*time.Hour))
	if _, err := os.Stat(p.Directory); err != nil {
		t.Fatalf("live create lost its receipt directory: %v", err)
	}
	once.Do(func() { close(release) })
	waitForClosed(t, done, 5*time.Second, "live create did not finish")
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "second\n" {
		t.Fatalf("remaining command = %q, err=%v", raw, err)
	}
	// Completion releases the lease, so normal retention can reclaim it.
	pruneHookProgress(filepath.Dir(path), time.Now().Add(15*24*time.Hour))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("completed lease retained journal: %v", err)
	}
}

func TestHookProgressCrashedLeaseIsReclaimable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installScopeShim(t)
	p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner", leaseProgress: true}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.lease.Close() })
	path, _ := hookProgressPath(p.Worktree)
	later := time.Now().Add(48 * time.Hour)
	pruneHookProgress(filepath.Dir(path), later)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live lease lost journal: %v", err)
	}
	// Closing the descriptor has the same kernel lease-release effect as death,
	// without writing finished (there is no runner left to write it on a crash).
	if err := p.lease.Close(); err != nil {
		t.Fatal(err)
	}
	pruneHookProgress(filepath.Dir(path), later)
	for _, file := range []string{path, p.Directory} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("dead ownerless lease retained %s: %v", file, err)
		}
	}
}

func TestHookProgressInvalidReadUsesLegacy(t *testing.T) {
	for _, kind := range []string{"bad-json", "not-regular", "foreign-directory", "wrong-owner"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			g := worktreeWithRecordedScope(t, "af-hook-owner")
			g.SetHookScopeSessionID("owner")
			p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
			if err != nil {
				t.Fatal(err)
			}
			path, _ := hookProgressPath(p.Worktree)
			switch kind {
			case "bad-json":
				err = os.WriteFile(path, []byte("{"), 0600)
			case "not-regular":
				if err = os.Remove(path); err == nil {
					err = os.Mkdir(path, 0700)
				}
			case "foreign-directory":
				p.Directory = filepath.Join(t.TempDir(), filepath.Base(p.Directory))
				var raw []byte
				raw, err = json.Marshal(p)
				if err == nil {
					err = os.WriteFile(path, raw, 0600)
				}
			case "wrong-owner":
				g.SetHookScopeSessionID("someone-else")
			}
			if err != nil {
				t.Fatal(err)
			}
			if g.adoptHookProgress() {
				t.Fatal("invalid journal started a retry watcher")
			}
		})
	}
}

func TestHookProgressUnreferencedLeaseProtectsRunner(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installScopeShim(t)
	p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner", leaseProgress: true}, nil, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.lease.Close() })
	path, _ := hookProgressPath(p.Worktree)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(48 * time.Hour)
	pruneHookProgress(filepath.Dir(path), later)
	if _, err := os.Stat(p.Directory); err != nil {
		t.Fatalf("unreferenced live lease lost receipts: %v", err)
	}
	if err := p.lease.Close(); err != nil {
		t.Fatal(err)
	}
	pruneHookProgress(filepath.Dir(path), later)
	if _, err := os.Stat(p.Directory); !os.IsNotExist(err) {
		t.Fatalf("abandoned receipts retained: %v", err)
	}
}

//go:build linux

package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestHookProgressCancelWaitsForReadableJournal(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	fastHookAdoptionPoll(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, []string{"touch must-not-run"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	original := hookProgressReadFile
	var recovered atomic.Bool
	hookProgressReadFile = func(path string) ([]byte, error) {
		if !recovered.Load() {
			return nil, &os.PathError{Op: "read", Path: path, Err: syscall.EIO}
		}
		return original(path)
	}
	timeout := hookStopTimeout
	hookStopTimeout = 80 * time.Millisecond
	t.Cleanup(func() {
		recovered.Store(true)
		g.hooksCancel()
		if g.HooksDone() != nil {
			waitForClosed(t, g.HooksDone(), 5*time.Second, "cancel watcher did not stop")
		}
		hookProgressReadFile = original
		hookStopTimeout = timeout
	})
	AdoptRunningHooks([]*GitWorktree{g})
	if err := g.cancelAndWaitHooks(); err == nil {
		t.Error("teardown succeeded while cancelled journal was unreadable")
	}
	requireOpen(t, g.HooksDone(), "cancelled unreadable journal reported completion")
	recovered.Store(true)
	waitForClosed(t, g.HooksDone(), 5*time.Second, "cancelled journal never became terminal")
	if !p.finished() {
		t.Fatal("cancelled journal was not marked finished")
	}
	if p.claimed(0) {
		t.Fatal("cancelled pending command ran")
	}
	if err := g.cancelAndWaitHooks(); err != nil {
		t.Fatalf("recovered teardown: %v", err)
	}
}

func TestHookProgressRetireUnreadableRefusesTeardown(t *testing.T) {
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	_, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	original := hookProgressReadFile
	hookProgressReadFile = func(string) ([]byte, error) { return nil, syscall.EIO }
	t.Cleanup(func() { hookProgressReadFile = original })
	if err := g.cancelAndWaitHooks(); err == nil {
		t.Fatal("retirement hid unreadable journal from teardown")
	}
}

func TestHookProgressAbsentJournalAllowsTeardown(t *testing.T) {
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	if err := g.cancelAndWaitHooks(); err != nil {
		t.Fatal(err)
	}
}

func TestHookProgressSameWorktreeBranchDriftResumes(t *testing.T) {
	for _, detach := range []bool{false, true} {
		t.Run(map[bool]string{false: "branch", true: "detached"}[detach], func(t *testing.T) {
			claimDaemonProcess(t)
			installScopeShim(t)
			fastHookAdoptionPoll(t)
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			g := worktreeWithRecordedScope(t, "af-hook-owner")
			g.SetHookScopeSessionID("owner")
			g.repoPath, g.worktreePath = linkedHookWorktree(t)
			g.branchName = "hook-resume"
			p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, []string{"true", "echo second >> ran"}, "af-hook-owner", "test")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p.receipt(0), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p.receipt(0), "exit"), []byte("0\n"), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"-C", g.worktreePath, "checkout", "-b", "hook-changed"}
			if detach {
				args = []string{"-C", g.worktreePath, "checkout", "--detach"}
			}
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("checkout: %s: %v", out, err)
			}
			t.Cleanup(func() {
				g.hooksCancel()
				if g.HooksDone() != nil {
					waitForClosed(t, g.HooksDone(), 5*time.Second, "watcher did not stop")
				}
			})
			AdoptRunningHooks([]*GitWorktree{g})
			waitForClosed(t, g.HooksDone(), 5*time.Second, "drifted worktree did not finish")
			raw, err := os.ReadFile(filepath.Join(g.worktreePath, "ran"))
			if err != nil || string(raw) != "second\n" {
				t.Fatalf("remaining command = %q, err=%v", raw, err)
			}
			if !p.completed() {
				t.Fatal("drifted worktree journal did not complete")
			}
		})
	}
}

func TestHookProgressMalformedJournalDoesNotBlockValidPruning(t *testing.T) {
	for _, kind := range []string{"truncated", "non-regular", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			installScopeShim(t)
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			bad, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "bad"}, nil, "af-hook-bad", "test")
			if err != nil {
				t.Fatal(err)
			}
			good, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "good"}, nil, "af-hook-good", "test")
			if err != nil {
				t.Fatal(err)
			}
			good.finish()
			badPath, _ := hookProgressPath(bad.Worktree)
			goodPath, _ := hookProgressPath(good.Worktree)
			switch kind {
			case "truncated":
				err = os.WriteFile(badPath, []byte("{"), 0600)
			case "non-regular":
				if err = os.Remove(badPath); err == nil {
					err = os.Mkdir(badPath, 0700)
				}
			case "unreadable":
				t.Cleanup(SetArchiveReadFileForTest(badPath, func(string) ([]byte, error) { return nil, syscall.EIO }))
			}
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				pruneHookProgress(filepath.Dir(goodPath), time.Now().Add(15*24*time.Hour))
				if _, err := os.Stat(bad.Directory); err != nil {
					t.Fatalf("ambiguous receipt directory was removed: %v", err)
				}
			}
			if _, err := os.Stat(goodPath); !os.IsNotExist(err) {
				t.Fatalf("valid old journal was not pruned: %v", err)
			}
			if _, err := os.Stat(good.Directory); !os.IsNotExist(err) {
				t.Fatalf("valid old receipts were not pruned: %v", err)
			}
		})
	}
}

func TestHookProgressFinishedWriteFailureRefusesTeardown(t *testing.T) {
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, nil, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(p.Directory, "finished")
	if err := os.Mkdir(marker, 0700); err != nil {
		t.Fatal(err)
	}
	if err := g.cancelAndWaitHooks(); err == nil {
		t.Fatal("failed terminal marker allowed teardown")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := g.cancelAndWaitHooks(); err != nil {
		t.Fatal(err)
	}
}

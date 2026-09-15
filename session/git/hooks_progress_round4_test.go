//go:build linux

package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookProgressVerificationTimeoutRetries(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	fastHookAdoptionPoll(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath = linkedHookWorktree(t)
	g.branchName = "hook-resume"
	marker := filepath.Join(g.worktreePath, "ran")
	p, err := newHookProgress(hookRun{repoPath: g.repoPath, worktreePath: g.worktreePath, scopeSessionID: "owner"}, []string{"touch " + shellQuoteForShim(marker)}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	first, second, release := filepath.Join(dir, "first"), filepath.Join(dir, "second"), filepath.Join(dir, "release")
	shim := fmt.Sprintf(`#!/bin/sh
case "$*" in *"worktree list"*)
 if [ ! -f %q ]; then touch %q; sleep 60; fi
 touch %q
 while [ ! -f %q ]; do sleep 0.01; done
;; esac
exec %q "$@"
`, first, first, second, release, realGit)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(SetLocalGitTimeoutForTest(time.Second))
	t.Cleanup(func() {
		g.hooksCancel()
		if g.HooksDone() != nil {
			waitForClosed(t, g.HooksDone(), 5*time.Second, "watcher did not stop")
		}
	})
	if !g.adoptHookProgress() {
		t.Fatal("journal was not adopted")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(second); err == nil {
			break
		}
		select {
		case <-g.HooksDone():
			t.Fatal("timeout closed HooksDone before remaining commands ran")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("verification was not retried")
		}
		time.Sleep(10 * time.Millisecond)
	}
	requireOpen(t, g.HooksDone(), "hooks finished before verification recovered")
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitForClosed(t, g.HooksDone(), 10*time.Second, "hooks did not finish after verification recovered")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("remaining command did not run: %v", err)
	}
	if !p.completed() {
		t.Fatal("resumed journal did not complete")
	}
}

func TestHookProgressRelocationRecoveryLeavesJournalPending(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath = linkedHookWorktree(t)
	g.branchName = "hook-resume"
	p, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.RestoreRelocationRecovery(RelocationRecovery{State: RelocationRecoveryStalled}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		g.hooksCancel()
		if g.HooksDone() != nil {
			waitForClosed(t, g.HooksDone(), 5*time.Second, "watcher did not stop")
		}
	})
	AdoptRunningHooks([]*GitWorktree{g})
	requireOpen(t, g.HooksDone(), "unresolved relocation was not represented as pending hook work")
	if p.claimed(0) || p.finished() {
		t.Error("unresolved relocation changed pending journal")
	}
}

func TestHookProgressPruneFinalProbeIsBatched(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	var paths []string
	for i := 0; i < 25; i++ {
		p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: fmt.Sprintf("owner%d", i)}, nil, fmt.Sprintf("af-hook-owner%d", i), "test")
		if err != nil {
			t.Fatal(err)
		}
		p.finish()
		path, _ := hookProgressPath(p.Worktree)
		paths = append(paths, path)
	}
	counter := filepath.Join(t.TempDir(), "first")
	calls := installSurvivorSystemctl(t, fmt.Sprintf("if [ -f %q ]; then echo unavailable >&2; exit 1; fi\ntouch %q\nexit 0\n", counter, counter))
	pruneHookProgress(filepath.Dir(paths[0]), time.Now().Add(15*24*time.Hour))
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "list-units"); n != 2 {
		t.Errorf("prune used %d manager probes for 25 candidates, want 2", n)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("failed final probe removed journal: %v", err)
		}
	}
}

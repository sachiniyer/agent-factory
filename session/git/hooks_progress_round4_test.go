//go:build linux

package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
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
 %s
;; esac
exec %q "$@"
`, first, first, second, testguard.BoundedGateWait(release, 10*time.Millisecond, 5*time.Minute), realGit)
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
	// The claim under test is that prune consults the liveness oracle ONCE for
	// all candidates and once for the final re-check — never once per journal.
	// Stub the oracle itself rather than counting shimmed systemctl calls: a
	// launcher scan that transiently refuses an unrelated same-user process
	// (#4217) used to keep the manager oracle from running at all, which this
	// test read as a missing shim log — an ordering flake, not a verdict.
	var probes atomic.Int32
	originalProbe := runningHookPrefixesForPrune
	runningHookPrefixesForPrune = func(...string) ([]string, error) {
		if probes.Add(1) == 1 {
			return nil, nil
		}
		return nil, errors.New("manager unavailable")
	}
	t.Cleanup(func() { runningHookPrefixesForPrune = originalProbe })
	pruneHookProgress(filepath.Dir(paths[0]), time.Now().Add(15*24*time.Hour))
	if n := probes.Load(); n != 2 {
		t.Errorf("prune used %d manager probes for 25 candidates, want 2", n)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("failed final probe removed journal: %v", err)
		}
	}
}

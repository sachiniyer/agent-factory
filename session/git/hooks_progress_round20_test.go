//go:build linux

package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestHookProgressMainCheckoutRunsAndRecordsIdentity(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := createGitRepo(t)
	marker := filepath.Join(repo, "hook-ran")
	writeLegacyRepoConfig(t, config.RepoIDFromRoot(repo), &config.RepoConfig{
		PostWorktreeCommands: []string{"touch " + shellQuoteForShim(marker)},
	})
	info, err := os.Stat(filepath.Join(repo, ".git"))
	if err != nil || !info.IsDir() {
		t.Fatalf("main-checkout fixture does not have a .git directory: %v", err)
	}

	done := RunPostWorktreeHooksAsyncWithEnvironment(context.Background(), repo, repo, nil)
	waitForClosed(t, done, 5*time.Second, "main-checkout hook did not finish")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("main-checkout hook did not run: %v", err)
	}
	p := readPublishedHookProgress(t, repo)
	if p.WorktreeIdentity == nil {
		t.Fatal("main-checkout journal did not record its .git directory identity")
	}
}

func TestHookProgressMissingGitIdentityDoesNotBlockRun(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	repo := freshRepoConfig(t, nil)
	tree := t.TempDir()
	marker := filepath.Join(tree, "hook-ran")
	writeLegacyRepoConfig(t, config.RepoIDFromRoot(repo), &config.RepoConfig{
		PostWorktreeCommands: []string{"touch " + shellQuoteForShim(marker)},
	})
	if _, err := os.Lstat(filepath.Join(tree, ".git")); !os.IsNotExist(err) {
		t.Fatalf("identity-free fixture unexpectedly has .git: %v", err)
	}

	done := RunPostWorktreeHooksAsyncWithEnvironment(context.Background(), repo, tree, nil)
	waitForClosed(t, done, 5*time.Second, "identity-free hook did not finish")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("identity-free hook did not run: %v", err)
	}
	p := readPublishedHookProgress(t, tree)
	if p.WorktreeIdentity != nil {
		t.Fatalf("identity-free journal recorded an identity: %+v", p.WorktreeIdentity)
	}
	if !p.ResumeDisabled {
		t.Fatal("identity-free journal still claimed that its suffix was resumable")
	}
}

func TestIdentityFreeHookProgressDoesNotHangAfterRestart(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	fastHookAdoptionPoll(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := createGitRepo(t)
	tree := t.TempDir()
	marker := filepath.Join(tree, "must-not-run")
	p, err := newHookProgress(hookRun{repoPath: repo, worktreePath: tree, scopeSessionID: "owner"}, []string{
		"touch " + shellQuoteForShim(marker),
	}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	if p.WorktreeIdentity != nil {
		t.Fatalf("identity failure fixture unexpectedly recorded an identity: %+v", p.WorktreeIdentity)
	}
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath = repo, tree
	t.Cleanup(func() {
		g.hooksCancel()
		if done := g.HooksDone(); done != nil {
			waitForClosed(t, done, 5*time.Second, "identity-free adoption watcher did not stop")
		}
	})

	AdoptRunningHooks([]*GitWorktree{g})
	if done := g.HooksDone(); done != nil {
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
			t.Fatal("identity-free journal installed a permanently pending adoption watcher")
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("identity-free journal resumed without authorization: %v", err)
	}
}

func readPublishedHookProgress(t *testing.T, worktree string) *hookProgress {
	t.Helper()
	path, err := hookProgressPath(worktree)
	if err != nil {
		t.Fatal(err)
	}
	p, err := readHookProgress(path)
	if err != nil {
		t.Fatalf("read published hook journal: %v", err)
	}
	if !p.finished() {
		t.Fatal("completed hook journal is not marked finished")
	}
	return p
}

//go:build linux

package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

func TestHookListResumesAfterRestart(t *testing.T) {
	if os.Getenv("AF_TEST_LIST_HELPER") == "1" {
		claimDaemonProcess(t)
		t.Setenv("AGENT_FACTORY_HOME", os.Getenv("AF_TEST_LIST_HOME"))
		g := &GitWorktree{repoPath: os.Getenv("AF_TEST_LIST_REPO"), worktreePath: os.Getenv("AF_TEST_LIST_TREE"), hooksCtx: context.Background()}
		g.SetHookScopeSessionID("resume4014")
		<-g.runHooks()
		return
	}
	fastHookAdoptionPoll(t)
	installScopeShim(t)
	claimDaemonProcess(t)
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(dir, "home"))
	pid := filepath.Join(dir, "pid")
	release := filepath.Join(dir, "release")
	order := filepath.Join(dir, "order")
	gone := filepath.Join(dir, "gone")
	repo := freshRepoConfig(t, []string{
		fmt.Sprintf("echo $$ > %q; while [ ! -f %q ]; do sleep 0.02; done; echo first >> %q; echo first-output", pid, release, order),
		fmt.Sprintf("echo second >> %q; echo second-output; exit 23", order),
	})
	home, _ := config.GetConfigDir()
	tree := t.TempDir()
	runner := exec.Command(os.Args[0], "-test.run=^TestHookListResumesAfterRestart$")
	runner.Env = append(os.Environ(), "AF_TEST_LIST_HELPER=1", "AF_TEST_LIST_REPO="+repo, "AF_TEST_LIST_TREE="+tree, "AF_TEST_LIST_HOME="+home)
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Process.Kill() })
	hookPID := waitForPidFile(t, pid, 5*time.Second)
	t.Cleanup(func() { _ = killProcessGroup(hookPID) })
	_ = runner.Process.Kill()
	_ = runner.Wait()
	// Resume the saved list, even if the operator edits configuration meanwhile.
	writeLegacyRepoConfig(t, config.RepoIDFromRoot(repo), &config.RepoConfig{PostWorktreeCommands: []string{"echo changed >> " + shellQuoteForShim(order)}})
	installSurvivorSystemctl(t, fmt.Sprintf(`case "$*" in
 *list-units*) if [ ! -f %q ]; then echo 'af-hook-resume4014-g0-0.scope loaded active running Hook'; fi ;;
 esac
 exit 0
 `, gone))
	g := worktreeWithRecordedScope(t, "af-hook-resume4014")
	g.SetHookScopeSessionID("resume4014")
	g.repoPath, g.worktreePath = repo, tree
	AdoptRunningHooks([]*GitWorktree{g})
	requireOpen(t, g.HooksDone(), "survivor must be reported running")
	if _, err := os.Stat(order); !os.IsNotExist(err) {
		t.Fatal("next entry ran before survivor")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if !waitForProcessExit(hookPID, 5*time.Second) {
		t.Fatal("survivor did not finish")
	}
	if err := os.WriteFile(gone, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitForClosed(t, g.HooksDone(), 10*time.Second, "resumed list never finished")
	raw, _ := os.ReadFile(order)
	if string(raw) != "first\nsecond\n" {
		t.Fatalf("hook order = %q, want first then second exactly once", raw)
	}
	logs, _ := filepath.Glob(filepath.Join(home, "logs", "hooks", "*.log"))
	found := false
	for _, path := range logs {
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), "second-output") {
			found = true
			if strings.Contains(string(raw), "first-output") {
				t.Fatal("entries share output log")
			}
		}
	}
	if !found {
		t.Fatal("resumed entry has no own output log")
	}
	restored := worktreeWithRecordedScope(t, "af-hook-resume4014")
	restored.SetHookScopeSessionID("resume4014")
	restored.repoPath, restored.worktreePath = repo, tree
	AdoptRunningHooks([]*GitWorktree{restored})
	if restored.HooksDone() != nil {
		waitForClosed(t, restored.HooksDone(), 5*time.Second, "completed run restarted")
	}
	raw, _ = os.ReadFile(order)
	if string(raw) != "first\nsecond\n" {
		t.Fatalf("completed list reran: %q", raw)
	}
}

func TestHookListResumeWaitsForReadableManager(t *testing.T) {
	fastHookAdoptionPoll(t)
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	g := worktreeWithRecordedScope(t, "af-hook-resume4014")
	g.SetHookScopeSessionID("resume4014")
	marker := filepath.Join(t.TempDir(), "ran")
	progress, err := newHookProgress(hookRun{worktreePath: g.worktreePath, scopeSessionID: "resume4014"}, []string{"echo ran > " + shellQuoteForShim(marker)}, "af-hook-resume4014", "test")
	if err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(t.TempDir(), "readable")
	installSurvivorSystemctl(t, "if [ ! -f "+shellQuoteForShim(release)+" ]; then exit 1; fi; exit 0\n")
	AdoptRunningHooks([]*GitWorktree{g})
	time.Sleep(100 * time.Millisecond)
	requireOpen(t, g.HooksDone(), "unreadable manager prematurely completed list")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("resumed without proving survivor gone")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitForClosed(t, g.HooksDone(), 5*time.Second, "list never resumed after manager recovered")
	if !progress.claimed(0) {
		t.Fatal("entry never started")
	}
}

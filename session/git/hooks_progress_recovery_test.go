//go:build linux

package git

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func linkedHookWorktree(t *testing.T) (string, string) {
	t.Helper()
	repo := createGitRepo(t)
	if output, err := exec.Command("git", "-C", repo, "-c", "user.name=Hook Test", "-c", "user.email=hook@example.com", "commit", "--allow-empty", "-m", "initial").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, output)
	}
	tree := filepath.Join(t.TempDir(), "linked")
	if output, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", "hook-resume", tree).CombinedOutput(); err != nil {
		t.Fatalf("create linked worktree: %v: %s", err, output)
	}
	return repo, tree
}

func TestHookProgressReplacementStaysPending(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo, tree := linkedHookWorktree(t)
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath, g.branchName = repo, tree, "hook-resume"
	marker := filepath.Join(tree, "must-not-run")
	p, err := newHookProgress(hookRun{repoPath: repo, worktreePath: tree, scopeSessionID: "owner"}, []string{"touch " + shellQuoteForShim(marker)}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	originalIdentity := p.WorktreeIdentity
	if originalIdentity == nil {
		t.Fatal("original journal has no checkout token")
	}
	if output, err := exec.Command("git", "-C", repo, "worktree", "remove", "--force", tree).CombinedOutput(); err != nil {
		t.Fatalf("remove original worktree: %v: %s", err, output)
	}
	if output, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", "hook-replacement", tree).CombinedOutput(); err != nil {
		t.Fatalf("create replacement worktree: %v: %s", err, output)
	}
	replacementIdentity, err := recordHookWorktreeIdentity(repo, tree)
	if err != nil {
		t.Fatalf("record replacement identity: %v", err)
	}
	// Model the allocator reusing every old filesystem number. Tokens remain
	// distinct because checkout creation never derives them from that allocator.
	originalIdentity.Device, originalIdentity.Inode = 7, 11
	replacementIdentity.Device, replacementIdentity.Inode = 7, 11
	if originalIdentity.same(replacementIdentity) {
		t.Fatal("replacement checkout reproduced the original identity through inode reuse")
	}
	AdoptRunningHooks([]*GitWorktree{g})
	requireOpen(t, g.HooksDone(), "missing replacement identity was treated as completion")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("saved command ran in the replacement directory")
	}
	if _, err := os.Stat(filepath.Join(p.Directory, "finished")); !os.IsNotExist(err) {
		t.Error("replacement journal must remain pending for recovery")
	}
	if p.claimed(0) {
		t.Error("replacement entry was claimed")
	}
	g.hooksCancel()
	waitForClosed(t, g.HooksDone(), 5*time.Second, "replacement watcher did not stop after cancellation")
}

func TestLegacyHookProgressWithoutIdentityStaysPending(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo, tree := linkedHookWorktree(t)
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath, g.branchName = repo, tree, "hook-resume"
	marker := filepath.Join(tree, "must-not-run")
	p, err := newHookProgress(hookRun{repoPath: repo, worktreePath: tree, scopeSessionID: "owner"}, []string{
		"touch " + shellQuoteForShim(marker),
	}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	p.WorktreeIdentity = nil
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := hookProgressPath(tree)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	AdoptRunningHooks([]*GitWorktree{g})
	requireOpen(t, g.HooksDone(), "pre-identity journal was authorized to resume")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("pre-identity journal ran without positive worktree identity")
	}
	if p.claimed(0) || p.finished() {
		t.Fatal("pre-identity journal did not remain pending")
	}
	g.hooksCancel()
	waitForClosed(t, g.HooksDone(), 5*time.Second, "legacy journal watcher did not stop after cancellation")
}

func TestLegacyInodeHookProgressStaysPending(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo, tree := linkedHookWorktree(t)
	g := worktreeWithRecordedScope(t, "af-hook-owner")
	g.SetHookScopeSessionID("owner")
	g.repoPath, g.worktreePath, g.branchName = repo, tree, "hook-resume"
	marker := filepath.Join(tree, "must-not-run")
	p, err := newHookProgress(hookRun{repoPath: repo, worktreePath: tree, scopeSessionID: "owner"}, []string{
		"touch " + shellQuoteForShim(marker),
	}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	p.WorktreeIdentity = &hookWorktreeIdentity{Device: 7, Inode: 11}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := hookProgressPath(tree)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	AdoptRunningHooks([]*GitWorktree{g})
	requireOpen(t, g.HooksDone(), "inode-only journal was authorized to resume")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("inode-only journal ran without a non-reproducible checkout identity")
	}
	if p.claimed(0) || p.finished() {
		t.Fatal("inode-only journal did not remain pending")
	}
	g.hooksCancel()
	waitForClosed(t, g.HooksDone(), 5*time.Second, "inode-only journal watcher did not stop after cancellation")
}

func TestHookProgressPublicationFailureRemovesReceipts(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tree := t.TempDir()
	path, err := hookProgressPath(tree)
	if err != nil {
		t.Fatal(err)
	}
	// A directory at the final JSON path forces publication's rename to fail.
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test"); err == nil {
		t.Fatal("expected publication to fail")
	}
	entries, err := filepath.Glob(filepath.Join(filepath.Dir(path), "entries-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unpublished receipt directories leaked: %v", entries)
	}
}

func TestHookProgressPrunesUnreferencedReceiptDirectories(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	p, err := newHookProgress(hookRun{worktreePath: t.TempDir(), scopeSessionID: "owner"}, []string{"true"}, "af-hook-owner", "test")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(p.Directory)
	old := filepath.Join(dir, "entries-unpublished")
	recent := filepath.Join(dir, "entries-recent")
	for _, path := range []string{old, recent} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().Add(-time.Minute)
	for _, path := range []string{old, p.Directory} {
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	// A prior process may have addressed this same AF home through a symlink.
	alias := filepath.Join(t.TempDir(), "hook-logs")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	journalPath, _ := hookProgressPath(p.Worktree)
	snapshot := *p
	snapshot.Directory = filepath.Join(alias, filepath.Base(p.Directory))
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	pruneHookProgress(dir, time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old unpublished receipt directory was not pruned")
	}
	for _, path := range []string{recent, p.Directory} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("recent or referenced receipts removed: %s: %v", path, err)
		}
	}
}

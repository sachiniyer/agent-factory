package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

func setupOriginMasterRepo(t *testing.T) string {
	t.Helper()
	repoPath := setupControlRepo(t)
	runGitTest(t, repoPath, "branch", "-M", "master")

	originPath := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", originPath).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", originPath, "symbolic-ref", "HEAD", "refs/heads/master").CombinedOutput(); err != nil {
		t.Fatalf("git origin HEAD: %v (%s)", err, out)
	}
	runGitTest(t, repoPath, "remote", "add", "origin", originPath)
	runGitTest(t, repoPath, "push", "-u", "origin", "master")
	runGitTest(t, repoPath, "fetch", "origin")
	runGitTest(t, repoPath, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")
	return repoPath
}

func setupOriginMainRepoWithStaleMaster(t *testing.T) string {
	t.Helper()
	repoPath := setupControlRepo(t)
	runGitTest(t, repoPath, "branch", "-M", "main")

	originPath := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", originPath).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v (%s)", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", originPath, "symbolic-ref", "HEAD", "refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("git origin HEAD: %v (%s)", err, out)
	}
	runGitTest(t, repoPath, "remote", "add", "origin", originPath)
	runGitTest(t, repoPath, "push", "-u", "origin", "main")
	runGitTest(t, repoPath, "push", "origin", "main:master")
	runGitTest(t, repoPath, "fetch", "origin")
	runGitTest(t, repoPath, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	return repoPath
}

func createRealKillGuardSession(t *testing.T, title string) (*Manager, config.RepoContext, session.InstanceData) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	return createRealKillGuardSessionFromRepo(t, title, setupOriginMasterRepo(t))
}

func createRealKillGuardSessionFromRepo(t *testing.T, title, repoPath string) (*Manager, config.RepoContext, session.InstanceData) {
	t.Helper()
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{"claude": "sh -c 'echo agent-ready; exec sleep 600'"}
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	data, err := manager.CreateSession(CreateSessionRequest{
		Title:    title,
		RepoPath: repoPath,
		Program:  "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if data.Worktree.WorktreePath == "" || data.Worktree.BranchName == "" {
		t.Fatalf("created session missing worktree metadata: %+v", data.Worktree)
	}
	t.Cleanup(func() {
		_ = manager.KillSession(KillSessionRequest{Title: title, RepoID: repo.ID, Force: true})
	})
	return manager, *repo, data
}

func commitInWorktree(t *testing.T, worktreePath, filename string) {
	t.Helper()
	path := filepath.Join(worktreePath, filename)
	if err := os.WriteFile(path, []byte("work\n"), 0644); err != nil {
		t.Fatalf("write worktree file: %v", err)
	}
	runGitTest(t, worktreePath, "add", filename)
	runGitTest(t, worktreePath, "commit", "-m", "session work")
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %s: %v (%s)", dir, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func assertNoSessionRecord(t *testing.T, repoID, title string) {
	t.Helper()
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var stored []session.InstanceData
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal stored instances: %v", err)
	}
	for _, inst := range stored {
		if inst.Title == title {
			t.Fatalf("session %q still present after kill: %+v", title, inst)
		}
	}
}

func assertBranchGone(t *testing.T, repoPath, branch string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err := cmd.Run(); err == nil {
		t.Fatalf("branch %s still exists after kill", branch)
	}
}

func TestKillSessionRefusesUnmergedBranchWithoutForce(t *testing.T) {
	manager, repo, data := createRealKillGuardSession(t, "unmerged-work")
	commitInWorktree(t, data.Worktree.WorktreePath, "work.txt")

	err := manager.KillSession(KillSessionRequest{Title: data.Title, RepoID: repo.ID})
	if err == nil {
		t.Fatal("KillSession should refuse an unmerged branch without --force")
	}
	msg := err.Error()
	for _, want := range []string{
		"session unmerged-work has unmerged work",
		data.Worktree.BranchName,
		"1 commits not on master",
		"af sessions archive unmerged-work",
		"af sessions kill unmerged-work --force",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q missing %q", msg, want)
		}
	}
	if _, err := os.Stat(data.Worktree.WorktreePath); err != nil {
		t.Fatalf("refused kill must leave worktree intact: %v", err)
	}
	if rec := recordFor(t, repo.ID, data.Title); rec == nil {
		t.Fatal("refused kill must leave the session record intact")
	}
}

func TestKillSessionRefusesDirtyWorktreeWithoutForce(t *testing.T) {
	manager, repo, data := createRealKillGuardSession(t, "dirty-work")
	if err := os.WriteFile(filepath.Join(data.Worktree.WorktreePath, "dirty.txt"), []byte("dirty\n"), 0644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	err := manager.KillSession(KillSessionRequest{Title: data.Title, RepoID: repo.ID})
	if err == nil {
		t.Fatal("KillSession should refuse a dirty worktree without --force")
	}
	msg := err.Error()
	for _, want := range []string{
		"session dirty-work has unmerged work",
		data.Worktree.BranchName,
		"uncommitted changes",
		"af sessions archive dirty-work",
		"af sessions kill dirty-work --force",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q missing %q", msg, want)
		}
	}
	if _, err := os.Stat(data.Worktree.WorktreePath); err != nil {
		t.Fatalf("refused kill must leave dirty worktree intact: %v", err)
	}
	if rec := recordFor(t, repo.ID, data.Title); rec == nil {
		t.Fatal("refused kill must leave the session record intact")
	}
}

func TestKillSessionRefusesDetachedDirtyWorktreeWithoutForce(t *testing.T) {
	manager, repo, data := createRealKillGuardSession(t, "detached-dirty")
	runGitTest(t, data.Worktree.WorktreePath, "checkout", "--detach")
	if err := os.WriteFile(filepath.Join(data.Worktree.WorktreePath, "dirty.txt"), []byte("dirty\n"), 0644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	err := manager.KillSession(KillSessionRequest{Title: data.Title, RepoID: repo.ID})
	if err == nil {
		t.Fatal("KillSession should refuse a detached dirty worktree without --force")
	}
	msg := err.Error()
	for _, want := range []string{
		"session detached-dirty has unmerged work",
		"uncommitted changes",
		"af sessions archive detached-dirty",
		"af sessions kill detached-dirty --force",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q missing %q", msg, want)
		}
	}
	if _, err := os.Stat(data.Worktree.WorktreePath); err != nil {
		t.Fatalf("refused kill must leave detached dirty worktree intact: %v", err)
	}
}

func TestKillSessionRefusesDetachedCommittedWorktreeWithoutForce(t *testing.T) {
	manager, repo, data := createRealKillGuardSession(t, "detached-committed")
	runGitTest(t, data.Worktree.WorktreePath, "checkout", "--detach")
	commitInWorktree(t, data.Worktree.WorktreePath, "detached.txt")

	err := manager.KillSession(KillSessionRequest{Title: data.Title, RepoID: repo.ID})
	if err == nil {
		t.Fatal("KillSession should refuse detached committed work without --force")
	}
	msg := err.Error()
	for _, want := range []string{
		"session detached-committed has unmerged work",
		"detached HEAD",
		"1 commits not on master",
		"af sessions archive detached-committed",
		"af sessions kill detached-committed --force",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q missing %q", msg, want)
		}
	}
	if _, err := os.Stat(data.Worktree.WorktreePath); err != nil {
		t.Fatalf("refused kill must leave detached committed worktree intact: %v", err)
	}
}

func TestKillGuardRefusesStoredHeadBranch(t *testing.T) {
	_, _, data := createRealKillGuardSession(t, "stored-head")
	data.Worktree.BranchName = "HEAD"

	err := guardKillRecoverableWork(data.Title, nil, &data)
	if err == nil {
		t.Fatal("kill guard should refuse stored HEAD branch metadata")
	}
	msg := err.Error()
	for _, want := range []string{
		"session stored-head may have unmerged work",
		"stored branch name is \"HEAD\"",
		"af sessions archive stored-head",
		"af sessions kill stored-head --force",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q missing %q", msg, want)
		}
	}
}

func TestKillSessionForceDestroysUnmergedBranch(t *testing.T) {
	manager, repo, data := createRealKillGuardSession(t, "force-destroys")
	commitInWorktree(t, data.Worktree.WorktreePath, "work.txt")

	if err := manager.KillSession(KillSessionRequest{Title: data.Title, RepoID: repo.ID, Force: true}); err != nil {
		t.Fatalf("KillSession --force: %v", err)
	}
	if _, err := os.Stat(data.Worktree.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("force kill should remove worktree, stat err = %v", err)
	}
	assertBranchGone(t, data.Worktree.RepoPath, data.Worktree.BranchName)
	assertNoSessionRecord(t, repo.ID, data.Title)
}

func TestKillSessionCleanBranchProceedsWithoutForce(t *testing.T) {
	manager, repo, data := createRealKillGuardSession(t, "clean-proceeds")

	if err := manager.KillSession(KillSessionRequest{Title: data.Title, RepoID: repo.ID}); err != nil {
		t.Fatalf("clean KillSession without force: %v", err)
	}
	if _, err := os.Stat(data.Worktree.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("clean kill should remove worktree, stat err = %v", err)
	}
	assertBranchGone(t, data.Worktree.RepoPath, data.Worktree.BranchName)
	assertNoSessionRecord(t, repo.ID, data.Title)
}

func TestKillSessionMergedToMainIgnoresStaleOriginMaster(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupOriginMainRepoWithStaleMaster(t)
	manager, repo, data := createRealKillGuardSessionFromRepo(t, "merged-main", repoPath)
	commitInWorktree(t, data.Worktree.WorktreePath, "merged.txt")
	runGitTest(t, data.Worktree.RepoPath, "merge", "--ff-only", data.Worktree.BranchName)
	runGitTest(t, data.Worktree.RepoPath, "push", "origin", "main")

	if got := strings.TrimSpace(runGitTest(t, data.Worktree.RepoPath, "rev-list", "--count", "origin/master..refs/heads/"+data.Worktree.BranchName)); got != "1" {
		t.Fatalf("test setup expected stale origin/master to be behind branch by 1 commit, got %s", got)
	}
	if got := strings.TrimSpace(runGitTest(t, data.Worktree.RepoPath, "rev-list", "--count", "origin/HEAD..refs/heads/"+data.Worktree.BranchName)); got != "0" {
		t.Fatalf("test setup expected origin/HEAD (main) to contain branch, got %s commits ahead", got)
	}

	if err := manager.KillSession(KillSessionRequest{Title: data.Title, RepoID: repo.ID}); err != nil {
		t.Fatalf("merged-to-main KillSession without force: %v", err)
	}
	if _, err := os.Stat(data.Worktree.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("merged-to-main kill should remove worktree, stat err = %v", err)
	}
	assertBranchGone(t, data.Worktree.RepoPath, data.Worktree.BranchName)
	assertNoSessionRecord(t, repo.ID, data.Title)
}

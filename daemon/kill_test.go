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

func createRealKillSession(t *testing.T, title string) (*Manager, config.RepoContext, session.InstanceData) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoPath := setupOriginMasterRepo(t)
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
		_ = manager.KillSession(KillSessionRequest{Title: title, RepoID: repo.ID})
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

func assertWorktreeGone(t *testing.T, worktreePath string) {
	t.Helper()
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("kill should remove worktree %s, stat err = %v", worktreePath, err)
	}
}

// The unmerged-work kill guard was dropped by owner decision (#1579): it
// over-refused ordinary cleanup — most notably squash-merged branches (whose
// landed commits are not ancestors of the base) and worktrees checked out on a
// different branch than the stored session branch. These tests pin the new
// contract: kill destroys the session in every one of those cases WITHOUT
// requiring --force. `af sessions archive` remains the restorable alternative.

func TestKillSessionDestroysUnmergedBranchWithoutForce(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "unmerged-work")
	// Commit work that never lands on the base branch — the exact shape the old
	// guard refused ("N commits not on master").
	commitInWorktree(t, data.Worktree.WorktreePath, "work.txt")

	if err := manager.KillSession(KillSessionRequest{Title: data.Title, RepoID: repo.ID}); err != nil {
		t.Fatalf("kill of unmerged branch without --force should succeed, got: %v", err)
	}
	assertWorktreeGone(t, data.Worktree.WorktreePath)
	assertNoSessionRecord(t, repo.ID, data.Title)
}

func TestKillSessionDestroysDirtyWorktreeWithoutForce(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "dirty-work")
	if err := os.WriteFile(filepath.Join(data.Worktree.WorktreePath, "dirty.txt"), []byte("dirty\n"), 0644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	if err := manager.KillSession(KillSessionRequest{Title: data.Title, RepoID: repo.ID}); err != nil {
		t.Fatalf("kill of dirty worktree without --force should succeed, got: %v", err)
	}
	assertWorktreeGone(t, data.Worktree.WorktreePath)
	assertNoSessionRecord(t, repo.ID, data.Title)
}

func TestKillSessionDestroysBranchMismatchWorktreeWithoutForce(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "branch-mismatch")
	// Check the worktree out on a different branch than the stored session
	// branch — the real-world trigger from #1579 where the guard failed with
	// "worktree is on branch X but the stored session branch is Y".
	runGitTest(t, data.Worktree.WorktreePath, "checkout", "-b", "some-other-branch")

	if err := manager.KillSession(KillSessionRequest{Title: data.Title, RepoID: repo.ID}); err != nil {
		t.Fatalf("kill of branch-mismatched worktree without --force should succeed, got: %v", err)
	}
	assertWorktreeGone(t, data.Worktree.WorktreePath)
	assertNoSessionRecord(t, repo.ID, data.Title)
}

func TestKillSessionCleanBranchProceedsWithoutForce(t *testing.T) {
	manager, repo, data := createRealKillSession(t, "clean-proceeds")

	if err := manager.KillSession(KillSessionRequest{Title: data.Title, RepoID: repo.ID}); err != nil {
		t.Fatalf("clean KillSession without force: %v", err)
	}
	assertWorktreeGone(t, data.Worktree.WorktreePath)
	assertBranchGone(t, data.Worktree.RepoPath, data.Worktree.BranchName)
	assertNoSessionRecord(t, repo.ID, data.Title)
}

// TestKillSessionInPlaceDoesNotDeleteRepoRoot pins the worktree-OWNERSHIP
// safety that survives the #1579 guard drop: killing an in-place (`--here`) /
// external-worktree session must NEVER delete the user's real checkout. That
// protection lives in GitWorktree.Cleanup() (externalWorktree → no-op; branch
// deletion gated on branchCreatedByUs), independent of the removed unmerged-work
// refusal — so dropping the guard must not endanger the user's repo root.
func TestKillSessionInPlaceDoesNotDeleteRepoRoot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
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

	// A committed sentinel in the user's real checkout, plus the current branch.
	sentinel := filepath.Join(repoPath, "user-file.txt")
	if err := os.WriteFile(sentinel, []byte("precious\n"), 0644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	runGitTest(t, repoPath, "add", "user-file.txt")
	runGitTest(t, repoPath, "commit", "-m", "user work")
	branch := strings.TrimSpace(runGitTest(t, repoPath, "rev-parse", "--abbrev-ref", "HEAD"))

	data, err := manager.CreateSession(CreateSessionRequest{
		Title:    "inplace",
		RepoPath: repoPath,
		Program:  "claude",
		InPlace:  true,
	})
	if err != nil {
		t.Fatalf("CreateSession --here: %v", err)
	}
	if !data.Worktree.ExternalWorktree {
		t.Fatalf("in-place session must record ExternalWorktree=true, got %+v", data.Worktree)
	}
	// Leave uncommitted work in the tree: the exact state the old guard would
	// have refused. Ownership safety, not the guard, must protect it.
	if err := os.WriteFile(filepath.Join(repoPath, "scratch.txt"), []byte("wip\n"), 0644); err != nil {
		t.Fatalf("write scratch: %v", err)
	}

	if err := manager.KillSession(KillSessionRequest{Title: "inplace", RepoID: repo.ID}); err != nil {
		t.Fatalf("kill of in-place session: %v", err)
	}

	// The user's real checkout must be entirely intact.
	if _, err := os.Stat(repoPath); err != nil {
		t.Fatalf("kill must not remove the repo root %s: %v", repoPath, err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "precious\n" {
		t.Fatalf("kill must not touch the user's committed file (got %q, err %v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(repoPath, "scratch.txt")); err != nil {
		t.Fatalf("kill must not delete the user's uncommitted work: %v", err)
	}
	// The user's branch must survive — the session never created it.
	runGitTest(t, repoPath, "rev-parse", "--verify", "refs/heads/"+branch)
	assertNoSessionRecord(t, repo.ID, "inplace")
}

func TestKillSessionForceStillDestroys(t *testing.T) {
	// --force is now a no-op but must remain accepted so existing
	// `af sessions kill --force` invocations keep working (#1579).
	manager, repo, data := createRealKillSession(t, "force-destroys")
	commitInWorktree(t, data.Worktree.WorktreePath, "work.txt")

	if err := manager.KillSession(KillSessionRequest{Title: data.Title, RepoID: repo.ID, Force: true}); err != nil {
		t.Fatalf("KillSession --force: %v", err)
	}
	assertWorktreeGone(t, data.Worktree.WorktreePath)
	assertBranchGone(t, data.Worktree.RepoPath, data.Worktree.BranchName)
	assertNoSessionRecord(t, repo.ID, data.Title)
}

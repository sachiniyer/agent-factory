package git

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// renameFixture builds a repo with one linked worktree on `dev/foo`, which is the
// shape archiving leaves behind (#2013 relocates the worktree; the branch stays
// checked out in it).
func renameFixture(t *testing.T) (repo, wt string, gw *GitWorktree) {
	t.Helper()
	repo = t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "config", "user.email", "probe@example.com")
	gitRun(t, repo, "config", "user.name", "probe")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "base")

	wt = filepath.Join(t.TempDir(), "archived")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "dev/foo", wt)

	gw, err := NewGitWorktreeFromStorage(repo, wt, "foo", "dev/foo", "", false, true)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}
	return repo, wt, gw
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestGitRenamesBranchCheckedOutInLinkedWorktree is the premise the whole
// archived-branch reclaim rests on (#2127), asked of git directly rather than
// assumed: the archived session's branch is CHECKED OUT in its relocated
// worktree, and git historically refused to rename a branch checked out anywhere.
// If that refusal still applied, moving the branch aside with the title would not
// be implementable and the design would have to change.
//
// It also pins the half that makes the rename safe to do on the user's behalf:
// the linked worktree's HEAD must FOLLOW the rename, leaving no worktree pointing
// at a branch that no longer exists.
//
// Deliberately driving raw git rather than GitWorktree — this is a statement
// about the tool, and routing it through our own wrapper would let a wrapper bug
// masquerade as a git guarantee.
func TestGitRenamesBranchCheckedOutInLinkedWorktree(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "config", "user.email", "probe@example.com")
	gitRun(t, repo, "config", "user.name", "probe")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "base")

	wt := filepath.Join(t.TempDir(), "archived")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "dev/foo", wt)
	if got := gitRun(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "dev/foo" {
		t.Fatalf("premise not met: linked worktree is on %q, not dev/foo", got)
	}

	cmd := exec.Command("git", "branch", "-m", "dev/foo", "dev/foo-archived")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git refuses to rename a branch checked out in a linked worktree — "+
			"the #2127 rename-aside design is not implementable as written: %v\n%s", err, out)
	}
	if got := gitRun(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "dev/foo-archived" {
		t.Fatalf("the linked worktree did not follow the branch rename; HEAD is %q", got)
	}
	if out := gitRun(t, repo, "branch", "--list", "dev/foo"); out != "" {
		t.Fatalf("the old branch name still exists after the rename: %q", out)
	}
}

// The reclaim path: the branch moves aside, the worktree follows it, and the
// recorded name moves with it so the persisted record and git do not disagree.
func TestRenameBranchMovesTheWorktreeWithIt(t *testing.T) {
	repo, wt, gw := renameFixture(t)

	if err := gw.RenameBranch("dev/foo-archived"); err != nil {
		t.Fatalf("RenameBranch: %v", err)
	}

	if got := gw.GetBranchName(); got != "dev/foo-archived" {
		t.Fatalf("recorded branch is %q; a stale record would persist a branch git no longer has", got)
	}
	if got := gitRun(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "dev/foo-archived" {
		t.Fatalf("the archived worktree is on %q, not the renamed branch", got)
	}
	// The old name is free — which is the entire point: the new session derives it.
	if out := gitRun(t, repo, "branch", "--list", "dev/foo"); out != "" {
		t.Fatalf("dev/foo is still taken after the rename (%q), so the reclaim did not happen", out)
	}
}

// Renaming to the name it already has is something the caller can legitimately
// derive; git errors on it, so it is absorbed rather than surfaced.
func TestRenameBranchToSameNameIsANoOp(t *testing.T) {
	_, wt, gw := renameFixture(t)

	if err := gw.RenameBranch("dev/foo"); err != nil {
		t.Fatalf("same-name rename must be a no-op, got: %v", err)
	}
	if got := gitRun(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "dev/foo" {
		t.Fatalf("same-name rename disturbed the worktree; HEAD is %q", got)
	}
}

// BranchIsPublished is tri-state, and the POLARITY is the point: a probe that
// cannot answer must not report "local", because "local" is what authorizes the
// rename. Each case is asserted on both returns.
func TestBranchIsPublishedIsTriState(t *testing.T) {
	repo, _, gw := renameFixture(t)

	t.Run("local branch with no upstream", func(t *testing.T) {
		published, known := gw.BranchIsPublished()
		if !known {
			t.Fatal("an existing local branch is an answerable question")
		}
		if published {
			t.Fatal("a branch with no upstream must not read as published")
		}
	})

	t.Run("branch with an upstream", func(t *testing.T) {
		// A real upstream, built without a network: a second repo added as a
		// remote, fetched, and set as the branch's tracking ref.
		remote := t.TempDir()
		gitRun(t, remote, "init", "-q", "--bare", "-b", "main")
		gitRun(t, repo, "remote", "add", "origin", remote)
		gitRun(t, repo, "push", "-q", "origin", "dev/foo")
		gitRun(t, repo, "branch", "--set-upstream-to=origin/dev/foo", "dev/foo")

		published, known := gw.BranchIsPublished()
		if !known {
			t.Fatal("a branch with a configured upstream is an answerable question")
		}
		if !published {
			t.Fatal("a pushed, tracking branch must read as published — renaming it desyncs it from its remote")
		}
	})

	t.Run("missing branch is UNKNOWN, never local", func(t *testing.T) {
		absent, err := NewGitWorktreeFromStorage(repo, t.TempDir(), "gone", "dev/does-not-exist", "", false, true)
		if err != nil {
			t.Fatalf("NewGitWorktreeFromStorage: %v", err)
		}
		published, known := absent.BranchIsPublished()
		if known {
			t.Fatalf("a branch that does not exist must be UNKNOWN, not a confident answer (published=%v)", published)
		}
	})
}

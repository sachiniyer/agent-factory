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

// BranchExists sees a plain, idle branch — the collision the checked-out map
// misses (the P3 on #2465): `git branch -m` refuses to rename onto ANY existing
// branch, not only a checked-out one.
func TestBranchExistsSeesUncheckedOutBranches(t *testing.T) {
	repo, _, gw := renameFixture(t)

	// A branch that exists but is checked out nowhere.
	gitRun(t, repo, "branch", "dev/idle")

	exists, ok := gw.BranchExists("dev/idle")
	if !ok {
		t.Fatal("BranchExists could not answer for an existing branch")
	}
	if !exists {
		t.Fatal("a plain, un-checked-out branch must still read as existing — it still blocks a rename onto it")
	}

	absent, ok := gw.BranchExists("dev/never")
	if !ok {
		t.Fatal("BranchExists could not answer for an absent branch")
	}
	if absent {
		t.Fatal("a name no branch holds must read as free")
	}
}

// The failure this all exists to keep out of the caller's error message: renaming
// ONTO an existing branch is a real git error, and it must not be mistaken for a
// worktree-relocation failure upstream.
func TestRenameBranchOntoExistingBranchFailsWithACauseNotAMask(t *testing.T) {
	repo, _, gw := renameFixture(t)
	gitRun(t, repo, "branch", "dev/taken")

	err := gw.RenameBranch("dev/taken")
	if err == nil {
		t.Fatal("renaming onto an existing branch must fail, not silently clobber it")
	}
	if !strings.Contains(err.Error(), "dev/taken") {
		t.Fatalf("the error must name the branch that blocked the rename, got: %v", err)
	}
	// The recorded name must NOT have moved on a failed rename, or the record would
	// claim a branch the repo does not have under that name.
	if got := gw.GetBranchName(); got != "dev/foo" {
		t.Fatalf("a failed rename must leave the recorded branch untouched, got %q", got)
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

	// A branch pushed WITHOUT -u has a remote ref but no upstream config. It is
	// exactly as published — a rename desyncs its local name from the remote and
	// any open PR — but `%(upstream:short)` is empty for it, so an
	// upstream-config-only probe returns "local", the polarity this whole function
	// exists to get right. `git push origin dev/foo`, `git push origin HEAD`, and
	// several push.default settings all produce this state.
	t.Run("branch pushed without -u is still published", func(t *testing.T) {
		repo2, _, gw2 := renameFixture(t)
		remote := t.TempDir()
		gitRun(t, remote, "init", "-q", "--bare", "-b", "main")
		gitRun(t, repo2, "remote", "add", "origin", remote)
		// No -u, and no --set-upstream-to: the remote ref exists, the tracking
		// config does not.
		gitRun(t, repo2, "push", "-q", "origin", "dev/foo")
		if out := gitRun(t, repo2, "for-each-ref", "--format=%(upstream:short)", "refs/heads/dev/foo"); out != "" {
			t.Fatalf("fixture is wrong: dev/foo has upstream config %q, so this does not test the -u-less case", out)
		}
		if out := gitRun(t, repo2, "show-ref", "--verify", "refs/remotes/origin/dev/foo"); out == "" {
			t.Fatal("fixture is wrong: no remote ref was created, so there is nothing published to detect")
		}

		published, known := gw2.BranchIsPublished()
		if !known {
			t.Fatal("a branch with a remote ref is an answerable question")
		}
		if !published {
			t.Fatal("a branch pushed without -u still has a remote ref (and maybe an open PR) — " +
				"renaming it is exactly the desync this guard promises not to cause")
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

package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWorkSummaryCountsDirtyFilesOnUnbornBranch(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, "first.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	worktree := &GitWorktree{worktreePath: root, branchName: "unborn-work"}
	summary, err := worktree.WorkSummary()
	if err != nil {
		t.Fatalf("WorkSummary: %v", err)
	}
	if summary.HeadSHA != "" || summary.Commits != 0 || summary.DirtyFiles != 1 {
		t.Fatalf("unborn dirty summary = %+v, want no HEAD/commits and one dirty file", summary)
	}
}

// TestWorkSummaryCountsUntrackedWhenConfigHidesThem is the #2502 regression.
//
// WorkSummary feeds the handoff brief. Bare `git status --porcelain` honours the
// repo's status.showUntrackedFiles, and a worktree shares .git/config with its
// main repo, so a user who set it to `no` made WorkSummary read a tree full of
// untracked work as DirtyFiles=0 — and the brief then told the receiving agent
// "0 uncommitted files" (or, on an unborn branch, "the working tree is clean")
// while it was not. --untracked-files=all overrides the config.
//
// `all`, not `normal`: the brief renders DirtyFiles as an EXACT number, and the
// hostile-config scenario naturally involves an untracked DIRECTORY (a new
// package), which `normal` collapses to one `?? dir/` entry. This puts several
// untracked files under the config to prove the count is per-file, not per-dir.
func TestWorkSummaryCountsUntrackedWhenConfigHidesThem(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	// The exact hostile config: hide untracked files from `git status`. Local, in
	// this repo's .git/config, which a worktree would share.
	runGit("config", "status.showUntrackedFiles", "no")
	// A new untracked package of three files — the case `normal` would report as
	// "1 uncommitted file".
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir pkg: %v", err)
	}
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		if err := os.WriteFile(filepath.Join(root, "pkg", name), []byte("package pkg\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	worktree := &GitWorktree{worktreePath: root, branchName: "unborn-work"}
	summary, err := worktree.WorkSummary()
	if err != nil {
		t.Fatalf("WorkSummary: %v", err)
	}
	if summary.DirtyFiles != 3 {
		t.Fatalf("DirtyFiles=%d with status.showUntrackedFiles=no and 3 untracked files, want 3.\n\n"+
			"Bare `git status --porcelain` hides them entirely (0); `--untracked-files=normal` "+
			"collapses the directory to one entry (1). The brief renders this as an exact "+
			"'N uncommitted files', so it must be `all` (#2502).", summary.DirtyFiles)
	}
	if summary.Empty() {
		t.Fatal("WorkSummary reports Empty() with untracked files present; the brief would omit " +
			"the 'Review uncommitted work' guidance")
	}
}

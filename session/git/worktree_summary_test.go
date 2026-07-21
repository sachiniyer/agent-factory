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

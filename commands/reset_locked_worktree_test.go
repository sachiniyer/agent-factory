package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	git "github.com/sachiniyer/agent-factory/session/git"
)

// TestFactoryReset_LockedWorktreeIsRecoverable is the #2110 end-to-end lock.
//
// `git worktree prune` exits 0 while refusing to prune a locked worktree's
// metadata, so the reset used to believe the registration was gone, fail the
// branch delete, and print "re-run `af reset` to finish" — after having deleted
// every session record, which left the re-run with nothing to plan and the
// branch blocked forever.
//
// The fix must make the printed recovery TRUE: report the failure, keep the
// blocked session's record (and only that one), and let
// `git worktree unlock` + a re-run finish the job.
func TestFactoryReset_LockedWorktreeIsRecoverable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Chdir(t.TempDir())

	repo, liveWT, reusedWT := seedMockRepo(t, home)
	seedAFState(t, home, repo, liveWT, reusedWT)
	repoID := config.RepoIDFromRoot(repo)

	// The user (or a tool) locked the live session's worktree.
	runGit(t, repo, "worktree", "lock", liveWT, "--reason", "still in use")

	plan, err := planFactoryReset()
	if err != nil {
		t.Fatalf("planFactoryReset: %v", err)
	}

	summary, err := executeFactoryReset(plan)
	if err == nil {
		t.Fatal("executeFactoryReset: want an error for the locked worktree, got nil")
	}

	// --- The failure is surfaced, classified, and actionable ---
	if !errors.Is(err, git.ErrWorktreeStillRegistered) {
		t.Errorf("error = %v, want it to wrap ErrWorktreeStillRegistered", err)
	}
	if !strings.Contains(err.Error(), "worktree unlock") || !strings.Contains(err.Error(), liveWT) {
		t.Errorf("error must name the unlock recovery for %s, got: %v", liveWT, err)
	}
	if summary.blocked != 1 {
		t.Errorf("summary.blocked = %d, want 1", summary.blocked)
	}

	// --- Ownership safety: git still owns that path, so AF left it alone ---
	if _, statErr := os.Stat(liveWT); statErr != nil {
		t.Errorf("locked worktree directory must survive, stat: %v", statErr)
	}
	if !branchExists(repo, "af-session-1") {
		t.Error("the blocked session's branch must survive so the recovery has something to finish")
	}

	// --- Everything NOT blocked was still reset (resilience is preserved) ---
	if _, statErr := os.Stat(reusedWT); !os.IsNotExist(statErr) {
		t.Errorf("unblocked worktree %s should still have been removed, stat: %v", reusedWT, statErr)
	}
	if branchExists(repo, "af-session-2") {
		t.Error("unblocked AF branch af-session-2 should still have been deleted")
	}
	if !branchExists(repo, "reused-linked") {
		t.Error("a reused user branch must never be deleted")
	}

	// --- ONLY the blocked session's record is retained ---
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var kept []session.InstanceData
	if err := json.Unmarshal(raw, &kept); err != nil {
		t.Fatalf("unmarshal retained records: %v", err)
	}
	if len(kept) != 1 {
		t.Fatalf("retained %d records, want exactly 1 (the blocked session): %+v", len(kept), kept)
	}
	if kept[0].Worktree.WorktreePath != liveWT {
		t.Errorf("retained record worktree = %s, want the blocked %s", kept[0].Worktree.WorktreePath, liveWT)
	}

	// The summary must tell the user what to do about it.
	var out bytes.Buffer
	printResetSummary(&out, summary)
	if !strings.Contains(out.String(), "Needs attention") || !strings.Contains(out.String(), "recovery command") {
		t.Errorf("summary must flag the blocked worktree and point at the unlock recovery, got:\n%s", out.String())
	}

	// --- RECOVERY: follow the printed advice; the re-run must finish the job ---
	runGit(t, repo, "worktree", "unlock", liveWT)

	plan2, err := planFactoryReset()
	if err != nil {
		t.Fatalf("planFactoryReset (re-run): %v", err)
	}
	if plan2.sessions != 1 || plan2.worktrees != 1 || plan2.branchCount() != 1 {
		t.Fatalf("re-run planned sessions=%d worktrees=%d branches=%d, want exactly the leftover 1/1/1",
			plan2.sessions, plan2.worktrees, plan2.branchCount())
	}

	summary2, err := executeFactoryReset(plan2)
	if err != nil {
		t.Fatalf("executeFactoryReset (re-run) after unlock: %v", err)
	}
	if summary2.blocked != 0 {
		t.Errorf("re-run summary.blocked = %d, want 0", summary2.blocked)
	}
	if _, statErr := os.Stat(liveWT); !os.IsNotExist(statErr) {
		t.Errorf("worktree should be gone after unlock + re-run, stat: %v", statErr)
	}
	if branchExists(repo, "af-session-1") {
		t.Error("branch should be deleted after unlock + re-run")
	}
	if _, statErr := os.Stat(filepath.Join(home, "instances")); !os.IsNotExist(statErr) {
		t.Errorf("instances/ should be gone after the recovery re-run, stat: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(home, "archived")); !os.IsNotExist(statErr) {
		t.Errorf("archived/ should be gone after the recovery re-run, stat: %v", statErr)
	}
}

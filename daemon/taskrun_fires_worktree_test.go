package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
)

// #6259: a worktree an interrupted task fire left behind can be torn down
// again — but only when the marker bound its EXACT path before the worktree
// could exist, and the tree provably carries no work. Every gap in that
// evidence leaves the leftover quarantined, as #6258 made mandatory.

// seedLeftoverWorktree registers a real worktree on the derived branch at
// wtPath — the residue a crash between `git worktree add` and the record
// commit leaves behind.
func seedLeftoverWorktree(t *testing.T, repoPath, branch, wtPath string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput(); err != nil {
		t.Fatalf("seed leftover worktree: %v: %s", err, out)
	}
}

func worktreeBranchFor(t *testing.T, title string) string {
	t.Helper()
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return git.BranchForTitle(cfg.BranchPrefix, title)
}

// TestTaskFireLeftover_BoundCleanWorktreeTombstones is the property #6259
// restores: the marker bound this exact path before `git worktree add` ran, so
// a clean worktree found there is provably this attempt's and is recorded for
// teardown — never removed by reconcile itself.
func TestTaskFireLeftover_BoundCleanWorktreeTombstones(t *testing.T) {
	title := "bound-worktree-leftover"
	repoPath, repo, sanitized, _ := fakeReapFixture(t, title)
	branch := worktreeBranchFor(t, title)
	wtPath := filepath.Join(resolvedTempDir(t), "bound-wt")
	seedLeftoverWorktree(t, repoPath, branch, wtPath)
	installFakeReapTmux(t, sanitized, fakeReapTmux{absent: true})

	fire := &taskFireMarker{
		TaskID: "ab0r7201", IntendedAt: time.Now().Add(-time.Minute), Title: title,
		InstanceID: reapInstanceID, WorktreePath: wtPath,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7201", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row == nil || !row.UserKilled || row.StartupStateUnknown {
		t.Fatalf("a bound clean worktree must tombstone for teardown (UserKilled, not startup-state-unknown), got %+v", row)
	}
	if row.Worktree.WorktreePath != wtPath {
		t.Fatalf("the tombstone must keep the exact worktree handle %q, got %q", wtPath, row.Worktree.WorktreePath)
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Fatalf("reconcile only marks for teardown — the worktree itself must still exist: %v", statErr)
	}
}

// TestTaskFireLeftover_UnboundAndMismatchedWorktreesStayQuarantined: without an
// exact pre-creation binding the worktree has no per-attempt evidence — a
// missing binding and a path that differs from the binding both quarantine.
func TestTaskFireLeftover_UnboundAndMismatchedWorktreesStayQuarantined(t *testing.T) {
	for _, tc := range []struct {
		name        string
		boundPath   string
		writeMarker bool
	}{
		{name: "marker binds a different path", boundPath: filepath.Join(resolvedTempDir(t), "elsewhere"), writeMarker: true},
		{name: "marker binds nothing", writeMarker: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			title := "unbound-worktree-leftover"
			repoPath, repo, sanitized, _ := fakeReapFixture(t, title)
			branch := worktreeBranchFor(t, title)
			wtPath := filepath.Join(resolvedTempDir(t), "actual-wt")
			seedLeftoverWorktree(t, repoPath, branch, wtPath)
			installFakeReapTmux(t, sanitized, fakeReapTmux{absent: true})

			fire := &taskFireMarker{
				TaskID: "ab0r7202", IntendedAt: time.Now().Add(-time.Minute), Title: title,
				InstanceID: reapInstanceID,
			}
			if tc.writeMarker {
				fire.WorktreePath = tc.boundPath
			}
			row, err := taskFireLeftoverRecord(
				&task.Task{ID: "ab0r7202", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
				fire, repo, nil)
			if err != nil {
				t.Fatalf("taskFireLeftoverRecord: %v", err)
			}
			if row == nil || !row.StartupStateUnknown || row.UserKilled {
				t.Fatalf("a worktree without exact path evidence must quarantine, got %+v", row)
			}
			if _, statErr := os.Stat(wtPath); statErr != nil {
				t.Fatalf("the unproven worktree must be left in place: %v", statErr)
			}
		})
	}
}

// TestTaskFireLeftover_BoundWorktreeWithWorkStaysQuarantined: path equality
// proves ownership, not disposability. A bound worktree carrying uncommitted
// files or commits beyond the mainline holds the only copy of that work and
// stays quarantined.
func TestTaskFireLeftover_BoundWorktreeWithWorkStaysQuarantined(t *testing.T) {
	for _, tc := range []struct {
		name string
		soil func(t *testing.T, wtPath string)
	}{
		{
			name: "dirty tree",
			soil: func(t *testing.T, wtPath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(wtPath, "uncommitted.txt"), []byte("work"), 0o644); err != nil {
					t.Fatalf("dirty the worktree: %v", err)
				}
			},
		},
		{
			name: "commit beyond the mainline",
			soil: func(t *testing.T, wtPath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(wtPath, "committed.txt"), []byte("work"), 0o644); err != nil {
					t.Fatalf("write file: %v", err)
				}
				for _, args := range [][]string{
					{"-C", wtPath, "add", "committed.txt"},
					{"-C", wtPath, "commit", "-m", "agent work"},
				} {
					if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
						t.Fatalf("git %v: %v: %s", args, err, out)
					}
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			title := "bound-worktree-with-work"
			repoPath, repo, sanitized, _ := fakeReapFixture(t, title)
			branch := worktreeBranchFor(t, title)
			wtPath := filepath.Join(resolvedTempDir(t), "worked-wt")
			seedLeftoverWorktree(t, repoPath, branch, wtPath)
			installFakeReapTmux(t, sanitized, fakeReapTmux{absent: true})
			tc.soil(t, wtPath)

			fire := &taskFireMarker{
				TaskID: "ab0r7203", IntendedAt: time.Now().Add(-time.Minute), Title: title,
				InstanceID: reapInstanceID, WorktreePath: wtPath,
			}
			row, err := taskFireLeftoverRecord(
				&task.Task{ID: "ab0r7203", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
				fire, repo, nil)
			if err != nil {
				t.Fatalf("taskFireLeftoverRecord: %v", err)
			}
			if row == nil || !row.StartupStateUnknown || row.UserKilled {
				t.Fatalf("a bound worktree carrying work must quarantine, got %+v", row)
			}
			if _, statErr := os.Stat(wtPath); statErr != nil {
				t.Fatalf("a worktree holding work must be left in place: %v", statErr)
			}
		})
	}
}

// TestTaskFireLeftover_BoundWorktreeWithProvenSessionStopsBoth: a bound
// worktree beside a session that proves the fire's instance id is the complete
// crash leftover — the runtime dies by its incarnation id first, then the clean
// bound worktree tombstones.
func TestTaskFireLeftover_BoundWorktreeWithProvenSessionStopsBoth(t *testing.T) {
	title := "bound-worktree-plus-session"
	repoPath, repo, sanitized, home := fakeReapFixture(t, title)
	branch := worktreeBranchFor(t, title)
	wtPath := filepath.Join(resolvedTempDir(t), "paired-wt")
	seedLeftoverWorktree(t, repoPath, branch, wtPath)
	logPath := installFakeReapTmux(t, sanitized, fakeReapTmux{instance: reapInstanceID, home: home})

	fire := &taskFireMarker{
		TaskID: "ab0r7204", IntendedAt: time.Now().Add(-time.Minute), Title: title,
		InstanceID: reapInstanceID, WorktreePath: wtPath,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7204", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	kills := killsIn(fakeTmuxLog(t, logPath))
	if len(kills) != 1 || kills[0] != "kill-session -t $7" {
		t.Fatalf("the proven session must die by its incarnation id before the tree is judged clean; got %q", kills)
	}
	if row == nil || !row.UserKilled || row.StartupStateUnknown {
		t.Fatalf("the proven pair must tombstone for teardown, got %+v", row)
	}
}

// TestTaskFireLeftover_BoundWorktreeWithUnprovenSessionKeepsBoth: even with the
// worktree bound, a session that cannot prove the fire's instance id keeps the
// whole leftover quarantined — the kill never runs, the worktree is never
// tombstoned.
func TestTaskFireLeftover_BoundWorktreeWithUnprovenSessionKeepsBoth(t *testing.T) {
	title := "bound-worktree-foreign-session"
	repoPath, repo, sanitized, home := fakeReapFixture(t, title)
	branch := worktreeBranchFor(t, title)
	wtPath := filepath.Join(resolvedTempDir(t), "foreign-wt")
	seedLeftoverWorktree(t, repoPath, branch, wtPath)
	logPath := installFakeReapTmux(t, sanitized, fakeReapTmux{instance: "a-concurrent-attempt", home: home})

	fire := &taskFireMarker{
		TaskID: "ab0r7205", IntendedAt: time.Now().Add(-time.Minute), Title: title,
		InstanceID: reapInstanceID, WorktreePath: wtPath,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7205", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if kills := killsIn(fakeTmuxLog(t, logPath)); len(kills) != 0 {
		t.Fatalf("an unproven session must not be killed; tmux was sent %q", kills)
	}
	if row == nil || !row.StartupStateUnknown || row.UserKilled {
		t.Fatalf("an unproven-session leftover must quarantine, got %+v", row)
	}
}

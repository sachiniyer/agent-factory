package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
)

// #6259 as narrowed on #6546: a worktree an interrupted task fire left behind
// is never deleted automatically — it stays quarantined exactly as #6258 made
// mandatory. What the path binding restores is ATTRIBUTION: the marker bound
// the exact path before `git worktree add` ran, so the report can name a
// leftover as this fire's — with the path, the fire and task, what it holds,
// and the command that removes it once a user has looked.

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

// TestTaskFireLeftover_BoundWorktreeReportedAndQuarantined is the narrowed
// contract: the marker bound this exact path before `git worktree add` ran,
// so the leftover is named as this attempt's worktree in the WARNING log —
// path, fire, task, residue, and the `af sessions kill` removal command —
// while the row itself quarantines startup-state-unknown, never UserKilled.
func TestTaskFireLeftover_BoundWorktreeReportedAndQuarantined(t *testing.T) {
	title := "bound-worktree-leftover"
	repoPath, repo, sanitized, _ := fakeReapFixture(t, title)
	branch := worktreeBranchFor(t, title)
	wtPath := filepath.Join(resolvedTempDir(t), "bound-wt")
	seedLeftoverWorktree(t, repoPath, branch, wtPath)
	installFakeReapTmux(t, sanitized, fakeReapTmux{absent: true})
	// Give the report something to describe: an untracked file and an
	// unpushed commit are work af must never weigh on the user's behalf.
	if err := os.WriteFile(filepath.Join(wtPath, "uncommitted.txt"), []byte("work"), 0o644); err != nil {
		t.Fatalf("dirty the worktree: %v", err)
	}
	for _, args := range [][]string{
		{"-C", wtPath, "add", "uncommitted.txt"},
		{"-C", wtPath, "commit", "-m", "agent work"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	warnings := captureWarnings(t)

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
	if row == nil || !row.StartupStateUnknown || row.UserKilled {
		t.Fatalf("a leftover worktree is never deleted automatically — it must quarantine, got %+v", row)
	}
	if row.Worktree.WorktreePath != wtPath {
		t.Fatalf("the row must keep the exact worktree handle %q, got %q", wtPath, row.Worktree.WorktreePath)
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Fatalf("the worktree must be left in place: %v", statErr)
	}
	logged := warnings.String()
	for _, want := range []string{
		"ab0r7201", reapInstanceID, title, wtPath,
		"it is this attempt's",
		"af sessions kill",
		"unpushed commits",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the report must name %q; logged: %s", want, logged)
		}
	}
}

// TestTaskFireLeftover_UnboundWorktreeReportedWithoutAttribution: without an
// exact pre-creation binding the worktree cannot be called this fire's — a
// missing binding and a differing binding both report unattributed and
// quarantine.
func TestTaskFireLeftover_UnboundWorktreeReportedWithoutAttribution(t *testing.T) {
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
			warnings := captureWarnings(t)

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
			logged := warnings.String()
			if !strings.Contains(logged, wtPath) || !strings.Contains(logged, "cannot be attributed") {
				t.Fatalf("an unbound leftover must still be reported, without attribution; logged: %s", logged)
			}
			if strings.Contains(logged, "it is this attempt's") {
				t.Fatalf("an unbound leftover must not be named this fire's; logged: %s", logged)
			}
		})
	}
}

// TestTaskFireLeftover_SymlinkSpelledBoundPathStillAttributes: the marker
// keeps the spelling worktree placement derived, which through a symlinked
// home can differ from git's canonical registration of the same directory.
// The canonicalized comparison still attributes it to this fire.
func TestTaskFireLeftover_SymlinkSpelledBoundPathStillAttributes(t *testing.T) {
	title := "symlink-bound-leftover"
	repoPath, repo, sanitized, _ := fakeReapFixture(t, title)
	branch := worktreeBranchFor(t, title)
	wtPath := filepath.Join(resolvedTempDir(t), "symlink-real-wt")
	seedLeftoverWorktree(t, repoPath, branch, wtPath)
	installFakeReapTmux(t, sanitized, fakeReapTmux{absent: true})
	linkPath := filepath.Join(resolvedTempDir(t), "symlink-spelled-wt")
	if err := os.Symlink(wtPath, linkPath); err != nil {
		t.Fatalf("symlink the bound path: %v", err)
	}
	warnings := captureWarnings(t)

	fire := &taskFireMarker{
		TaskID: "ab0r7210", IntendedAt: time.Now().Add(-time.Minute), Title: title,
		InstanceID: reapInstanceID, WorktreePath: linkPath,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7210", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row == nil || !row.StartupStateUnknown {
		t.Fatalf("a symlink spelling of the same bound directory must still attribute, got %+v", row)
	}
	if !strings.Contains(warnings.String(), "it is this attempt's") {
		t.Fatalf("the canonicalized path must still attribute the leftover; logged: %s", warnings.String())
	}
}

// TestTaskFireLeftover_BoundWorktreeWithUnprovenSessionKeepsBoth: even with
// the worktree bound, a session that cannot prove the fire's instance id is
// never touched — the kill never runs and the whole leftover quarantines.
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

// TestTaskFireLeftover_ReFiredMarkerCannotTouchPriorIncarnation: a re-fire
// mints a new marker for the same title — same deterministic worktree path,
// different instance id. That marker must never touch the prior
// incarnation's runtime: the live session's environment carries the earlier
// attempt's id, so nothing is proven and the whole leftover quarantines.
func TestTaskFireLeftover_ReFiredMarkerCannotTouchPriorIncarnation(t *testing.T) {
	title := "refired-leftover"
	repoPath, repo, sanitized, home := fakeReapFixture(t, title)
	branch := worktreeBranchFor(t, title)
	wtPath := filepath.Join(resolvedTempDir(t), "refired-wt")
	seedLeftoverWorktree(t, repoPath, branch, wtPath)
	// The prior attempt's session is still live: its environment stamps the
	// instance id attempt A was minted, not the re-fire's.
	logPath := installFakeReapTmux(t, sanitized, fakeReapTmux{instance: "attempt-a", home: home})

	// The re-fire's marker bound the same deterministic worktree path before
	// its own create ran — then that create crashed too.
	fire := &taskFireMarker{
		TaskID: "ab0r7206", IntendedAt: time.Now().Add(-time.Minute), Title: title,
		InstanceID: "attempt-b", WorktreePath: wtPath,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7206", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if kills := killsIn(fakeTmuxLog(t, logPath)); len(kills) != 0 {
		t.Fatalf("the re-fire's marker must not kill the prior incarnation's session; tmux was sent %q", kills)
	}
	if row == nil || !row.StartupStateUnknown || row.UserKilled {
		t.Fatalf("a prior-incarnation leftover must quarantine, got %+v", row)
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Fatalf("the prior incarnation's worktree must be left in place: %v", statErr)
	}
}

// TestTaskFireWorktreeResidue: the report describes what the leftover holds —
// modified/staged/untracked/ignored entries from `git status --ignored`, plus
// commits the kept branch does not have. It never fails: a probe that cannot
// answer degrades to "unknown", because the residue line reports, it never
// gates.
func TestTaskFireWorktreeResidue(t *testing.T) {
	repoPath := resolvedTempDir(t)
	for _, args := range [][]string{
		{"-C", repoPath, "init", "-b", "main"},
		{"-C", repoPath, "config", "user.email", "t@t"},
		{"-C", repoPath, "config", "user.name", "t"},
		{"-C", repoPath, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	wtPath := filepath.Join(resolvedTempDir(t), "residue-wt")
	seedLeftoverWorktree(t, repoPath, "residue-branch", wtPath)

	got := taskFireWorktreeResidue(wtPath, repoPath)
	if !strings.Contains(got, "no modified") {
		t.Fatalf("a clean tree must report as much, got %q", got)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "untracked.txt"), []byte("work"), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}
	for _, args := range [][]string{
		{"-C", wtPath, "add", "untracked.txt"},
		{"-C", wtPath, "commit", "-m", "agent work"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(wtPath, "scratch.txt"), []byte("work"), 0o644); err != nil {
		t.Fatalf("write scratch file: %v", err)
	}
	got = taskFireWorktreeResidue(wtPath, repoPath)
	if !strings.Contains(got, "entries") || !strings.Contains(got, "1 unpushed commits") {
		t.Fatalf("the residue must describe held work and unpushed commits, got %q", got)
	}

	got = taskFireWorktreeResidue(filepath.Join(resolvedTempDir(t), "does-not-exist"), repoPath)
	if !strings.Contains(got, "unknown") {
		t.Fatalf("a probe that cannot answer must degrade to unknown, got %q", got)
	}
}

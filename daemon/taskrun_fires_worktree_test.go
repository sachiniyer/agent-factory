package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/systemdunit"
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
	// The row's own id is fresh, so its Worktree must carry the durable
	// hook-scope handle the fire's original identity minted — the teardown's
	// survivor pass names scopes from it.
	if want := systemdunit.HookScopeUnitPrefix(reapInstanceID); row.Worktree.HookScopeUnitPrefix != want {
		t.Fatalf("the tombstone must carry the fire's hook-scope handle %q, got %q", want, row.Worktree.HookScopeUnitPrefix)
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
// proves ownership, not disposability. "Clean" is defined by the three probes
// the reconcile gate runs — WorktreeDirtyFiles, WorktreeCommitsBeyondRepo,
// WorktreePrivateRefs — and every kind of residue they can name is exercised
// here: modified or staged files, untracked files, ignored files (a gitignored
// artifact exists nowhere else either), commits beyond the mainline, and the
// worktree's own stash or refs/worktree/* entries. Each holds the only copy of
// that work, so each bound leftover stays quarantined.
func TestTaskFireLeftover_BoundWorktreeWithWorkStaysQuarantined(t *testing.T) {
	for _, tc := range []struct {
		name string
		// prep runs inside the repository before the worktree is created —
		// the stash and ignored cases need tracked content in the tree.
		prep func(t *testing.T, repoPath string)
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
			name: "ignored file",
			prep: func(t *testing.T, repoPath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repoPath, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
					t.Fatalf("write .gitignore: %v", err)
				}
				for _, args := range [][]string{
					{"-C", repoPath, "add", ".gitignore"},
					{"-C", repoPath, "commit", "-m", "ignore logs"},
				} {
					if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
						t.Fatalf("git %v: %v: %s", args, err, out)
					}
				}
			},
			soil: func(t *testing.T, wtPath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(wtPath, "agent.log"), []byte("work"), 0o644); err != nil {
					t.Fatalf("write ignored file: %v", err)
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
		{
			name: "an index-flag-hidden modification",
			prep: func(t *testing.T, repoPath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repoPath, "flagged.txt"), []byte("base"), 0o644); err != nil {
					t.Fatalf("write tracked file: %v", err)
				}
				for _, args := range [][]string{
					{"-C", repoPath, "add", "flagged.txt"},
					{"-C", repoPath, "commit", "-m", "track a file"},
				} {
					if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
						t.Fatalf("git %v: %v: %s", args, err, out)
					}
				}
			},
			// --assume-unchanged hides the edit from git status entirely: the
			// tree reads clean while holding the only copy of the change.
			soil: func(t *testing.T, wtPath string) {
				t.Helper()
				if out, err := exec.Command("git", "-C", wtPath, "update-index",
					"--assume-unchanged", "flagged.txt").CombinedOutput(); err != nil {
					t.Fatalf("git update-index: %v: %s", err, out)
				}
				if err := os.WriteFile(filepath.Join(wtPath, "flagged.txt"), []byte("edited"), 0o644); err != nil {
					t.Fatalf("edit flagged file: %v", err)
				}
			},
		},
		{
			// A stash made inside the leftover lands in the SHARED refs/stash
			// the main checkout also lists — it survives worktree removal, so
			// it is not the tree's own to lose and must not quarantine. The
			// worktree's own refs are what the private-refs gate counts.
			name: "a bisect of its own",
			soil: func(t *testing.T, wtPath string) {
				t.Helper()
				if out, err := exec.Command("git", "-C", wtPath, "update-ref",
					"refs/bisect/agent-bisect", "HEAD").CombinedOutput(); err != nil {
					t.Fatalf("git update-ref: %v: %s", err, out)
				}
			},
		},
		{
			name: "a refs/worktree ref of its own",
			soil: func(t *testing.T, wtPath string) {
				t.Helper()
				if out, err := exec.Command("git", "-C", wtPath, "update-ref",
					"refs/worktree/agent-proof", "HEAD").CombinedOutput(); err != nil {
					t.Fatalf("git update-ref: %v: %s", err, out)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			title := "bound-worktree-with-work"
			repoPath, repo, sanitized, _ := fakeReapFixture(t, title)
			branch := worktreeBranchFor(t, title)
			wtPath := filepath.Join(resolvedTempDir(t), "worked-wt")
			if tc.prep != nil {
				tc.prep(t, repoPath)
			}
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

// TestTaskFireLeftover_SymlinkSpelledBoundPathTombstones: the marker keeps
// the spelling worktree placement derived, which through a symlinked home can
// differ from git's canonical registration of the same directory. Binding is
// still exact — the canonicalized comparison must reach the same tombstone.
func TestTaskFireLeftover_SymlinkSpelledBoundPathTombstones(t *testing.T) {
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
	if row == nil || !row.UserKilled || row.StartupStateUnknown {
		t.Fatalf("a symlink spelling of the same bound directory must still prove exact binding, got %+v", row)
	}
}

// TestTaskFireLeftover_ReFiredMarkerCannotTouchPriorIncarnation: a re-fire
// mints a new marker for the same title — same deterministic worktree path,
// different instance id. That marker must never authorize touching the prior
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

// TestTaskFireLeftover_SharedRepoStashDoesNotQuarantine: `git stash` inside a
// linked worktree writes the SHARED refs/stash the main checkout also lists,
// so a stash — even one made inside this leftover before the crash — survives
// the worktree's removal in the repository. It is not the tree's own to lose:
// the bound clean leftover still tombstones (Codex on #6546).
func TestTaskFireLeftover_SharedRepoStashDoesNotQuarantine(t *testing.T) {
	title := "shared-stash-leftover"
	repoPath, repo, sanitized, _ := fakeReapFixture(t, title)
	// Give the repository a tracked file and stash a change to it — the stash
	// entry lands in the common refs/stash, visible from every worktree.
	if err := os.WriteFile(filepath.Join(repoPath, "tracked.txt"), []byte("base"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	for _, args := range [][]string{
		{"-C", repoPath, "add", "tracked.txt"},
		{"-C", repoPath, "commit", "-m", "track a file"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	branch := worktreeBranchFor(t, title)
	wtPath := filepath.Join(resolvedTempDir(t), "shared-stash-wt")
	seedLeftoverWorktree(t, repoPath, branch, wtPath)
	if err := os.WriteFile(filepath.Join(wtPath, "tracked.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatalf("edit tracked file: %v", err)
	}
	if out, err := exec.Command("git", "-C", wtPath, "stash").CombinedOutput(); err != nil {
		t.Fatalf("git stash in leftover: %v: %s", err, out)
	}
	installFakeReapTmux(t, sanitized, fakeReapTmux{absent: true})

	fire := &taskFireMarker{
		TaskID: "ab0r7207", IntendedAt: time.Now().Add(-time.Minute), Title: title,
		InstanceID: reapInstanceID, WorktreePath: wtPath,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7207", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row == nil || !row.UserKilled || row.StartupStateUnknown {
		t.Fatalf("a shared stash is not the worktree's own state — the bound clean leftover must tombstone, got %+v", row)
	}
}

// TestTaskFireLeftover_IncompleteWriterReapKeepsBoth: a proven session whose
// process capture cannot be verified is Stopped but not WritersReaped — the
// kill fired, yet a survivor could still be writing into the tree, so the
// bound worktree quarantines rather than tombstoning on an unreadable answer.
func TestTaskFireLeftover_IncompleteWriterReapKeepsBoth(t *testing.T) {
	title := "incomplete-reap-leftover"
	repoPath, repo, sanitized, home := fakeReapFixture(t, title)
	branch := worktreeBranchFor(t, title)
	wtPath := filepath.Join(resolvedTempDir(t), "incomplete-wt")
	seedLeftoverWorktree(t, repoPath, branch, wtPath)
	logPath := installFakeReapTmux(t, sanitized, fakeReapTmux{instance: reapInstanceID, home: home, badPanes: true})

	fire := &taskFireMarker{
		TaskID: "ab0r7208", IntendedAt: time.Now().Add(-time.Minute), Title: title,
		InstanceID: reapInstanceID, WorktreePath: wtPath,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7208", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row == nil || !row.StartupStateUnknown || row.UserKilled {
		t.Fatalf("an incomplete writer reap must quarantine the worktree, got %+v", row)
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Fatalf("the unproven-clean worktree must be left in place: %v", statErr)
	}
	if kills := killsIn(fakeTmuxLog(t, logPath)); len(kills) != 1 {
		t.Fatalf("the proven session itself is still stopped — only its writers are unconfirmed; tmux was sent %q", kills)
	}
}

// TestTaskFireLeftover_HookScopesJoinedBeforeCleanProbe: the fire's original
// instance identity names every transient scope its hooks could still own —
// the gate must join them before it judges the tree (a scope survivor can
// still be writing into it), and with the fire's id, not the synthesized
// row's fresh one.
func TestTaskFireLeftover_HookScopesJoinedBeforeCleanProbe(t *testing.T) {
	title := "hook-join-leftover"
	repoPath, repo, sanitized, _ := fakeReapFixture(t, title)
	branch := worktreeBranchFor(t, title)
	wtPath := filepath.Join(resolvedTempDir(t), "hooked-wt")
	seedLeftoverWorktree(t, repoPath, branch, wtPath)
	installFakeReapTmux(t, sanitized, fakeReapTmux{absent: true})

	var gotSession, gotPath string
	calls := 0
	prev := stopOrphanedHookScopes
	stopOrphanedHookScopes = func(sessionID, worktreePath string) error {
		calls++
		gotSession, gotPath = sessionID, worktreePath
		return nil
	}
	t.Cleanup(func() { stopOrphanedHookScopes = prev })

	fire := &taskFireMarker{
		TaskID: "ab0r7209", IntendedAt: time.Now().Add(-time.Minute), Title: title,
		InstanceID: reapInstanceID, WorktreePath: wtPath,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7209", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the clean gate must join orphaned hook scopes exactly once before probing, got %d calls", calls)
	}
	if gotSession != reapInstanceID || gotPath != wtPath {
		t.Fatalf("the join must name the fire's identities, got (%q, %q), want (%q, %q)", gotSession, gotPath, reapInstanceID, wtPath)
	}
	if row == nil || !row.UserKilled || row.StartupStateUnknown {
		t.Fatalf("with the hook join answering, the bound clean worktree must tombstone, got %+v", row)
	}
}

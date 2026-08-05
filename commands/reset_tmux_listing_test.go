package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	cmdutil "github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// failingListTmuxOnPath puts a `tmux` earlier on PATH whose `ls` exits 1 with an
// AMBIGUOUS diagnostic — a reachable socket we are not permitted to talk to, so
// sessions may well be running behind it. Every other subcommand exits non-zero,
// so any attempt to continue the sweep fails loudly instead of silently
// succeeding against a fake.
func failingListTmuxOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
ls)
  echo "error connecting to /tmp/tmux-1000/default (Permission denied)" >&2
  exit 1
  ;;
*)
  exit 97
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestFactoryReset_RefusesWhenTmuxListingFails is the #2870 end-to-end lock, and
// the reason it drives the REAL tmux.CleanupSessions instead of a stub: the
// defect was that a failed read returned nil, so any test that stubs the cleanup
// out cannot see it. Every other destructive boundary is still faked.
//
// `af reset` deletes worktrees, prunes branches, and erases records. It lists
// tmux sessions first precisely so it knows what is live before it destroys
// anything — so a listing that FAILED, read as "there are no sessions", hands
// reset a licence to destroy exactly the work that check exists to protect. The
// user sees a clean run, not an error.
func TestFactoryReset_RefusesWhenTmuxListingFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Chdir(t.TempDir())

	repo, liveWT, reusedWT := seedMockRepo(t, home)
	seedAFState(t, home, repo, liveWT, reusedWT)
	repoID := config.RepoIDFromRoot(repo)
	instancesPath := filepath.Join(home, "instances", repoID, "instances.json")
	recordsBefore, err := os.ReadFile(instancesPath)
	if err != nil {
		t.Fatalf("read seeded instances: %v", err)
	}

	fakeDaemonSeams(t)
	failingListTmuxOnPath(t)
	// The real sweep, against a tmux that cannot answer. This is the seam the
	// bug lived behind.
	cleanupTmuxSessionsFn = func() error { return tmux.CleanupSessions(cmdutil.MakeExecutor()) }

	out, err := runResetCapture(t)
	if err == nil {
		t.Fatalf("runReset returned nil after tmux refused to list sessions — reset proceeded to "+
			"destroy worktrees, branches, and records with the live session set unknown (#2870).\noutput:\n%s", out)
	}
	for _, want := range []string{"could not list tmux sessions", "Permission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q so the user can see what was unreadable", err, want)
		}
	}
	if !strings.Contains(out, "Nothing was removed") {
		t.Errorf("reset output does not tell the user nothing was removed:\n%s", out)
	}
	if strings.Contains(out, "Tmux sessions have been cleaned up") {
		t.Errorf("reset claimed the tmux sweep succeeded:\n%s", out)
	}
	if strings.Contains(out, "Factory reset complete") {
		t.Errorf("reset reported completion after refusing:\n%s", out)
	}

	// --- Nothing was destroyed ---
	if _, err := os.Stat(liveWT); err != nil {
		t.Errorf("live AF worktree %s was removed despite the refusal: %v", liveWT, err)
	}
	if _, err := os.Stat(reusedWT); err != nil {
		t.Errorf("AF worktree %s was removed despite the refusal: %v", reusedWT, err)
	}
	for _, branch := range []string{"af-session-1", "af-session-2", "my-feature", "reused-linked", "master"} {
		if !branchExists(repo, branch) {
			t.Errorf("branch %s was pruned despite the refusal", branch)
		}
	}
	recordsAfter, err := os.ReadFile(instancesPath)
	if err != nil {
		t.Fatalf("session records were erased despite the refusal: %v", err)
	}
	if string(recordsAfter) != string(recordsBefore) {
		t.Errorf("session records changed despite the refusal:\n got %s\nwant %s", recordsAfter, recordsBefore)
	}
	tasks, err := task.LoadTasks()
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("tasks after the refusal = %d, want 2 (untouched)", len(tasks))
	}
	for _, name := range []string{"archived", "events", "logs", "locks", config.StateFileName} {
		if _, err := os.Stat(filepath.Join(home, name)); err != nil {
			t.Errorf("%s was wiped despite the refusal: %v", name, err)
		}
	}

	// --- And the refusal is recoverable: with tmux answering, the same home
	//     still resets cleanly. A fail-closed guard that leaves the user stuck
	//     would be its own bug.
	cleanupTmuxSessionsFn = func() error { return nil }
	if _, err := runResetCapture(t); err != nil {
		t.Fatalf("re-running reset after the tmux failure cleared: %v", err)
	}
	assertGone(t, filepath.Join(home, "instances"))
	assertGone(t, liveWT)
}

// TestPlanFactoryReset_UnreadableRecordDirIsNotAnEmptyOne is the sibling read in
// the same command. planFactoryReset cross-checks <AF_HOME>/instances against
// the repos GetAllInstances surfaced, because LoadAllRepoInstances SKIPS a repo
// file it cannot read — the cross-check is what turns a skipped repo into a
// preserved one, so its branches are not orphaned by a wholesale delete.
//
// A failed directory read must therefore not answer "no additional repos": that
// silently disables the compensating control and lets the wipe take the whole
// instances/ tree, including records nobody could read.
//
// It tests the guard directly rather than through planFactoryReset, and that is
// deliberate: LoadAllRepoInstances reads the SAME directory a few lines earlier
// and already fails closed on it, so a whole-command test would abort there and
// pass with or without this fix — asserting the claim while checking something
// weaker. Only the window between the two reads reaches this branch, and only
// this test can prove what happens in it.
func TestPlanFactoryReset_UnreadableRecordDirIsNotAnEmptyOne(t *testing.T) {
	seen := map[string]struct{}{"known-repo": {}}

	t.Run("missing directory is a determinate empty", func(t *testing.T) {
		ids, err := unreadableRepoIDs(filepath.Join(t.TempDir(), "instances"), seen)
		if err != nil {
			t.Fatalf("unreadableRepoIDs on a missing dir = %v, want nil: a home with no records "+
				"has nothing to preserve and must still be resettable", err)
		}
		if len(ids) != 0 {
			t.Errorf("ids = %v, want none", ids)
		}
	})

	t.Run("unsurfaced repo is reported", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "instances")
		mkdir(t, filepath.Join(dir, "known-repo"))
		mkdir(t, filepath.Join(dir, "skipped-repo"))
		writeFile(t, filepath.Join(dir, "stray-file"), "not a repo dir")
		ids, err := unreadableRepoIDs(dir, seen)
		if err != nil {
			t.Fatalf("unreadableRepoIDs = %v, want nil", err)
		}
		if len(ids) != 1 || ids[0] != "skipped-repo" {
			t.Errorf("ids = %v, want [skipped-repo]", ids)
		}
	})

	t.Run("unreadable directory aborts", func(t *testing.T) {
		// A regular file where the directory should be: ReadDir fails with
		// ENOTDIR for every user, root included, so this pins the guard without
		// depending on file modes.
		notADir := filepath.Join(t.TempDir(), "instances")
		writeFile(t, notADir, "")
		ids, err := unreadableRepoIDs(notADir, seen)
		if err == nil {
			t.Fatalf("unreadableRepoIDs on an unreadable dir = (%v, nil) — a failed read was reported "+
				"as 'no additional repos', which lets the wipe delete records it could not read", ids)
		}
		if !strings.Contains(err.Error(), notADir) {
			t.Errorf("error = %q, want it to name the path that could not be read", err)
		}
	})
}

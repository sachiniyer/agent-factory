package commands

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

func boolPtr(b bool) *bool { return &b }

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func branchExists(dir, branch string) bool {
	cmd := exec.Command("git", "-C", dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
}

// seedMockRepo builds a real git repo with master, an AF live-session worktree
// (branch af-session-1), an AF-created archived-session branch (af-session-2),
// a user branch (my-feature) that an AF --here session reused, and a user
// branch (reused-linked) checked out in a LINKED worktree by a session that
// reused it (BranchCreatedByUs=false). It returns the repo root and the two
// linked worktree paths.
func seedMockRepo(t *testing.T, home string) (repo, liveWT, reusedWT string) {
	t.Helper()
	repo = t.TempDir()
	runGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "init")
	runGit(t, repo, "branch", "-M", "master")
	runGit(t, repo, "branch", "my-feature")    // user branch (reused by a --here session)
	runGit(t, repo, "branch", "reused-linked") // user branch (reused in a linked worktree)
	runGit(t, repo, "branch", "af-session-2")  // AF-created, archived (no live worktree)

	liveWT = filepath.Join(home, "worktrees", "live")
	runGit(t, repo, "worktree", "add", "-q", "-b", "af-session-1", liveWT)

	// A linked worktree that CHECKS OUT an existing user branch (no -b): the
	// session reused it, so BranchCreatedByUs=false. The old bulk-cleanup path
	// would have `git branch -D`'d this — the critical #1 regression to guard.
	reusedWT = filepath.Join(home, "worktrees", "reused")
	runGit(t, repo, "worktree", "add", "-q", reusedWT, "reused-linked")
	return repo, liveWT, reusedWT
}

// seedAFState writes instance records, tasks, archived dirs, state files, and a
// preserved config into the throwaway AF home. Returns the config.toml bytes so
// callers can assert they are untouched.
func seedAFState(t *testing.T, home, repo, liveWT, reusedWT string) []byte {
	t.Helper()
	repoID := config.RepoIDFromRoot(repo)

	recs := []session.InstanceData{
		{ // live AF session — AF created af-session-1, live worktree present
			Title:    "live",
			Path:     repo,
			Liveness: session.LiveReady,
			Worktree: session.GitWorktreeData{
				RepoPath: repo, WorktreePath: liveWT,
				BranchName: "af-session-1", BranchCreatedByUs: boolPtr(true),
			},
		},
		{ // archived AF session — AF created af-session-2, worktree relocated
			Title:    "arch",
			Path:     repo,
			Liveness: session.LiveArchived,
			Worktree: session.GitWorktreeData{
				RepoPath: repo, WorktreePath: filepath.Join(home, "archived", repoID, "arch"),
				BranchName: "af-session-2", BranchCreatedByUs: boolPtr(true),
			},
		},
		{ // --here session that reused the user's branch — must NOT be pruned
			Title:    "here",
			Path:     repo,
			Liveness: session.LiveReady,
			Worktree: session.GitWorktreeData{
				RepoPath: repo, WorktreePath: repo, ExternalWorktree: true,
				BranchName: "my-feature", BranchCreatedByUs: boolPtr(false),
			},
		},
		{ // session that reused a user branch in a LINKED worktree — the worktree
			// is removed but the branch must NOT be pruned via ANY path (#1 fix).
			Title:    "reused",
			Path:     repo,
			Liveness: session.LiveReady,
			Worktree: session.GitWorktreeData{
				RepoPath: repo, WorktreePath: reusedWT, ExternalWorktree: false,
				BranchName: "reused-linked", BranchCreatedByUs: boolPtr(false),
			},
		},
	}
	raw, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRepoInstances(repoID, raw); err != nil {
		t.Fatal(err)
	}

	// Archived worktree dir on disk (its record is the only pointer to it).
	archDir := filepath.Join(home, "archived", repoID, "arch")
	if err := os.MkdirAll(archDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archDir, "keep"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	// Two scheduled tasks.
	for _, id := range []string{"task-one", "task-two"} {
		if err := task.AddTask(task.Task{
			ID: id, CronExpr: "* * * * *", Prompt: "do", Enabled: true, ProjectPath: repo,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// State files and daemon-runtime dirs that must be wiped.
	writeFile(t, filepath.Join(home, config.StateFileName), `{"schema_version":1,"help_screens_seen":7}`)
	writeFile(t, filepath.Join(home, config.TUIStateFileName), `{}`)
	mkdir(t, filepath.Join(home, "events"))
	mkdir(t, filepath.Join(home, "logs"))
	mkdir(t, filepath.Join(home, "locks"))

	// Config that must be PRESERVED.
	cfgBytes := []byte("listen_addr = \"127.0.0.1:9876\"\n[defaults]\nprogram = \"codex\"\n")
	writeFileBytes(t, filepath.Join(home, "config.toml"), cfgBytes)
	// Per-repo config (remote_hooks etc.) is user config, also preserved.
	mkdir(t, filepath.Join(home, "repos", repoID))
	writeFile(t, filepath.Join(home, "repos", repoID, "config.json"), `{"remote_hooks":{}}`)

	return cfgBytes
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	writeFileBytes(t, path, []byte(content))
}

func writeFileBytes(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
}

func TestFactoryReset_WipesEverythingKeepsRepoAndConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	// Neutralize the cwd-repo injection in planFactoryReset: chdir to a non-git
	// dir so ResolveMainRepoRoot adds nothing, keeping the wipe scoped to our
	// seeded mock repo (never the repo the test binary runs from).
	t.Chdir(t.TempDir())

	repo, liveWT, reusedWT := seedMockRepo(t, home)
	cfgBytes := seedAFState(t, home, repo, liveWT, reusedWT)
	repoID := config.RepoIDFromRoot(repo)

	// --- Plan reflects the real scope ---
	plan, err := planFactoryReset()
	if err != nil {
		t.Fatalf("planFactoryReset: %v", err)
	}
	if plan.sessions != 3 {
		t.Errorf("sessions = %d, want 3 (live, here, reused)", plan.sessions)
	}
	if plan.archived != 1 {
		t.Errorf("archived = %d, want 1", plan.archived)
	}
	if plan.tasks != 2 {
		t.Errorf("tasks = %d, want 2", plan.tasks)
	}
	if plan.worktrees != 3 {
		t.Errorf("worktrees = %d, want 3 (live, arch, reused; external --here excluded)", plan.worktrees)
	}
	if plan.branchCount() != 2 {
		t.Errorf("branchCount = %d, want 2 (my-feature + reused-linked excluded)", plan.branchCount())
	}
	if len(plan.corruptRepoIDs) != 0 {
		t.Errorf("corruptRepoIDs = %v, want none", plan.corruptRepoIDs)
	}
	if _, ok := plan.repoRoots[repo]; !ok || len(plan.repoRoots) != 1 {
		t.Errorf("repoRoots = %v, want exactly {%s}", plan.repoRoots, repo)
	}

	// --- Execute ---
	summary, err := executeFactoryReset(plan)
	if err != nil {
		t.Fatalf("executeFactoryReset: %v", err)
	}
	if summary.sessions != 3 || summary.archived != 1 || summary.tasks != 2 ||
		summary.worktrees != 3 || summary.branches != 2 || summary.corrupt != 0 {
		t.Errorf("summary = %+v, want {3 1 2 3 2 0}", *summary)
	}

	// --- Everything AF is gone ---
	assertGone(t, filepath.Join(home, "instances"))
	assertGone(t, filepath.Join(home, "archived"))
	assertGone(t, filepath.Join(home, "events"))
	assertGone(t, filepath.Join(home, "logs"))
	assertGone(t, filepath.Join(home, "locks"))
	assertGone(t, filepath.Join(home, config.StateFileName))
	assertGone(t, filepath.Join(home, config.TUIStateFileName))
	assertGone(t, filepath.Join(home, "tasks.json"))
	assertGone(t, liveWT)
	assertGone(t, reusedWT)

	tasks, err := task.LoadTasks()
	if err != nil {
		t.Fatalf("LoadTasks after reset: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("tasks after reset = %d, want 0", len(tasks))
	}

	// --- AF branches pruned, user branches untouched ---
	if branchExists(repo, "af-session-1") {
		t.Error("af-session-1 (AF live) should be deleted")
	}
	if branchExists(repo, "af-session-2") {
		t.Error("af-session-2 (AF archived) should be deleted")
	}
	if !branchExists(repo, "my-feature") {
		t.Error("my-feature (user branch, reused by --here) must be preserved")
	}
	if !branchExists(repo, "reused-linked") {
		t.Error("reused-linked (user branch reused in a LINKED worktree) must be preserved — the #1 fix")
	}
	if !branchExists(repo, "master") {
		t.Error("master must be preserved")
	}

	// --- Real repo + .git intact ---
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Errorf("repo .git missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "README.md")); err != nil {
		t.Errorf("repo working tree file missing: %v", err)
	}

	// --- Config preserved byte-for-byte; per-repo config preserved ---
	got, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("config.toml missing after reset: %v", err)
	}
	if string(got) != string(cfgBytes) {
		t.Errorf("config.toml changed:\n got %q\nwant %q", got, cfgBytes)
	}
	if _, err := os.Stat(filepath.Join(home, "repos", repoID, "config.json")); err != nil {
		t.Errorf("per-repo config removed: %v", err)
	}

	// --- Idempotent: a second reset is a clean no-op ---
	plan2, err := planFactoryReset()
	if err != nil {
		t.Fatalf("second planFactoryReset: %v", err)
	}
	if plan2.sessions != 0 || plan2.archived != 0 || plan2.tasks != 0 || plan2.branchCount() != 0 {
		t.Errorf("second plan not empty: %+v", *plan2)
	}
	summary2, err := executeFactoryReset(plan2)
	if err != nil {
		t.Fatalf("second executeFactoryReset: %v", err)
	}
	if summary2.sessions != 0 || summary2.archived != 0 || summary2.tasks != 0 ||
		summary2.worktrees != 0 || summary2.branches != 0 {
		t.Errorf("second summary not empty: %+v", *summary2)
	}
	// Config still intact after the second run.
	got2, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if string(got2) != string(cfgBytes) {
		t.Errorf("config.toml changed on second reset")
	}
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, stat err = %v", path, err)
	}
}

// TestFactoryReset_CorruptRecordLeftIntact proves that when a repo's
// instances.json is unreadable (so BranchCreatedByUs is unknown), reset leaves
// BOTH the record and its branch intact and reports it — rather than erasing
// the record while orphaning the branch (Greptile #2).
func TestFactoryReset_CorruptRecordLeftIntact(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Chdir(t.TempDir())

	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "init")
	runGit(t, repo, "branch", "-M", "master")
	runGit(t, repo, "branch", "orphan-candidate") // referenced by the unreadable record

	repoID := config.RepoIDFromRoot(repo)
	instPath, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, instPath, "{ this is not valid json for instances")

	// The corrupt repo also has archived worktrees on disk: its preserved
	// record points at them, so they must survive too (no dangling reference).
	corruptArchive := filepath.Join(home, "archived", repoID, "sess")
	writeFile(t, filepath.Join(corruptArchive, "keep"), "x")

	// A DELETED repo's archived worktrees (an orphaned archive with no readable
	// record) must still be removed.
	deletedRepoID := config.RepoIDFromRoot(t.TempDir())
	deletedArchive := filepath.Join(home, "archived", deletedRepoID, "sess")
	writeFile(t, filepath.Join(deletedArchive, "gone"), "x")

	cfgBytes := []byte("listen_addr = \"127.0.0.1:9000\"\n")
	writeFileBytes(t, filepath.Join(home, "config.toml"), cfgBytes)

	plan, err := planFactoryReset()
	if err != nil {
		t.Fatalf("planFactoryReset: %v", err)
	}
	if len(plan.corruptRepoIDs) != 1 || plan.corruptRepoIDs[0] != repoID {
		t.Fatalf("corruptRepoIDs = %v, want [%s]", plan.corruptRepoIDs, repoID)
	}
	if plan.branchCount() != 0 {
		t.Errorf("branchCount = %d, want 0 (no branches pruned from unreadable records)", plan.branchCount())
	}

	summary, err := executeFactoryReset(plan)
	if err != nil {
		t.Fatalf("executeFactoryReset: %v", err)
	}
	if summary.corrupt != 1 {
		t.Errorf("summary.corrupt = %d, want 1", summary.corrupt)
	}

	// The unreadable record file is NOT erased.
	if _, err := os.Stat(instPath); err != nil {
		t.Errorf("corrupt instances.json was erased: %v", err)
	}
	// The preserved record's archived worktree is KEPT (no dangling reference).
	if _, err := os.Stat(corruptArchive); err != nil {
		t.Errorf("preserved record's archived worktree was deleted (dangling reference): %v", err)
	}
	// A deleted/orphaned repo's archive IS removed.
	assertGone(t, filepath.Join(home, "archived", deletedRepoID))
	// Its branch is NOT orphaned/deleted.
	if !branchExists(repo, "orphan-candidate") {
		t.Error("branch of an unreadable record must be preserved (not orphaned or deleted)")
	}
	if !branchExists(repo, "master") {
		t.Error("master must be preserved")
	}
	// Config still intact.
	if got, _ := os.ReadFile(filepath.Join(home, "config.toml")); string(got) != string(cfgBytes) {
		t.Error("config.toml changed")
	}
}

// TestFactoryReset_ResilientPartialFailure proves that a worktree-cleanup error
// on one repo does NOT abort the reset: the other repo's worktree is still
// removed, state/tasks are still cleared, and the error is surfaced (Greptile
// #3). A follow-up run with the bad root removed completes cleanly.
func TestFactoryReset_ResilientPartialFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Chdir(t.TempDir())

	repo, liveWT, reusedWT := seedMockRepo(t, home)
	seedAFState(t, home, repo, liveWT, reusedWT)

	state := config.LoadState()
	storage, err := session.NewStorage(state, "")
	if err != nil {
		t.Fatal(err)
	}

	// Inject a failing repo root ("" errors in RemoveWorktreesForRepo) alongside
	// the real one, to force the collect-and-continue path deterministically.
	plan := &resetPlan{
		configDir: home,
		storage:   storage,
		sessions:  3, archived: 1, tasks: 2, worktrees: 3,
		repoRoots: map[string]struct{}{"": {}, repo: {}},
		branches:  map[string][]string{repo: {"af-session-1", "af-session-2"}},
	}

	summary, err := executeFactoryReset(plan)
	if err == nil {
		t.Fatal("expected a joined error from the failing repo root, got nil")
	}
	if !strings.Contains(err.Error(), "cleanup worktrees") {
		t.Errorf("error should name the worktree-cleanup failure: %v", err)
	}

	// Despite the failure, the reset reached a consistent end state.
	assertGone(t, liveWT)
	assertGone(t, reusedWT)
	assertGone(t, filepath.Join(home, "instances"))
	assertGone(t, filepath.Join(home, config.StateFileName))
	assertGone(t, filepath.Join(home, "tasks.json"))
	if tasks, _ := task.LoadTasks(); len(tasks) != 0 {
		t.Errorf("tasks not cleared despite partial failure: %d", len(tasks))
	}
	// The good repo's AF branches were still pruned; user branches preserved.
	if branchExists(repo, "af-session-1") || branchExists(repo, "af-session-2") {
		t.Error("AF branches should be pruned even when another repo failed")
	}
	if !branchExists(repo, "reused-linked") || !branchExists(repo, "my-feature") {
		t.Error("user branches must be preserved")
	}
	if summary.branches != 2 {
		t.Errorf("branches = %d, want 2", summary.branches)
	}

	// A clean re-run (no bad root) is a no-op that succeeds.
	plan2, err := planFactoryReset()
	if err != nil {
		t.Fatalf("re-run planFactoryReset: %v", err)
	}
	if _, err := executeFactoryReset(plan2); err != nil {
		t.Fatalf("re-run should complete cleanly, got: %v", err)
	}
}

func TestResetConfirmed(t *testing.T) {
	tests := []struct {
		name     string
		force    bool
		isTTY    bool
		input    string
		want     bool
		wantNote bool // prints the non-TTY notice
	}{
		{name: "force bypasses prompt", force: true, isTTY: true, want: true},
		{name: "force bypasses even non-tty", force: true, isTTY: false, want: true},
		{name: "non-tty skips prompt but proceeds", isTTY: false, want: true, wantNote: true},
		{name: "tty exact WIPE proceeds", isTTY: true, input: "WIPE\n", want: true},
		{name: "tty WIPE with spaces proceeds", isTTY: true, input: "  WIPE  \n", want: true},
		{name: "tty lowercase aborts", isTTY: true, input: "wipe\n", want: false},
		{name: "tty wrong word aborts", isTTY: true, input: "yes\n", want: false},
		{name: "tty empty aborts", isTTY: true, input: "\n", want: false},
		{name: "tty EOF aborts", isTTY: true, input: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			got, err := resetConfirmed(tc.force, tc.isTTY, strings.NewReader(tc.input), &out)
			if err != nil {
				t.Fatalf("resetConfirmed: %v", err)
			}
			if got != tc.want {
				t.Errorf("proceed = %v, want %v", got, tc.want)
			}
			hasNote := strings.Contains(out.String(), "stdin is not a terminal")
			if hasNote != tc.wantNote {
				t.Errorf("non-tty notice printed = %v, want %v (out=%q)", hasNote, tc.wantNote, out.String())
			}
		})
	}
}

package commands

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/spf13/cobra"
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
	registered, err := config.RegisterProject(repo)
	if err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, config.ProjectRegistryDirName, registered.ID, "project.json")); err != nil {
		t.Fatalf("registered sessionless project is not durable before reset: %v", err)
	}
	checkoutMarkers, err := filepath.Glob(filepath.Join(repo, ".git", "agent-factory", "checkout-id-????????????????????????????????"))
	if err != nil || len(checkoutMarkers) != 1 {
		t.Fatalf("registered checkout markers = %v, err = %v, want one", checkoutMarkers, err)
	}
	checkoutMarker := checkoutMarkers[0]
	if _, err := os.Stat(checkoutMarker); err != nil {
		t.Fatalf("registered checkout marker is not durable before reset: %v", err)
	}

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
	var planOutput strings.Builder
	printResetPlan(&planOutput, plan)
	if !strings.Contains(planOutput.String(), "1 registered project record(s)") ||
		!strings.Contains(planOutput.String(), "checkout identity marker(s)") {
		t.Errorf("reset plan omitted the sessionless project registration it will remove:\n%s", planOutput.String())
	}

	// --- Execute ---
	summary, err := executeFactoryReset(plan)
	if err != nil {
		t.Fatalf("executeFactoryReset: %v", err)
	}
	if summary.sessions != 3 || summary.archived != 1 || summary.tasks != 2 || summary.projects != 1 ||
		summary.worktrees != 3 || summary.branches != 2 || summary.corrupt != 0 {
		t.Errorf("summary = %+v, want 3 sessions, 1 archived, 2 tasks, 1 project, 3 worktrees, 2 branches, no corruption", *summary)
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
	assertGone(t, filepath.Join(home, config.ProjectRegistryDirName))
	assertGone(t, checkoutMarker)
	assertGone(t, checkoutMarker+".lock")
	assertGone(t, liveWT)
	assertGone(t, reusedWT)
	projects, err := config.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects after reset: %v", err)
	}
	if len(projects) != 0 {
		t.Errorf("projects after reset = %d, want 0", len(projects))
	}

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
	if plan2.sessions != 0 || plan2.archived != 0 || plan2.tasks != 0 || plan2.projects != 0 || plan2.branchCount() != 0 {
		t.Errorf("second plan not empty: %+v", *plan2)
	}
	summary2, err := executeFactoryReset(plan2)
	if err != nil {
		t.Fatalf("second executeFactoryReset: %v", err)
	}
	if summary2.sessions != 0 || summary2.archived != 0 || summary2.tasks != 0 || summary2.projects != 0 ||
		summary2.worktrees != 0 || summary2.branches != 0 {
		t.Errorf("second summary not empty: %+v", *summary2)
	}
	// Config still intact after the second run.
	got2, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if string(got2) != string(cfgBytes) {
		t.Errorf("config.toml changed on second reset")
	}

	reregistered, err := config.RegisterProject(repo)
	if err != nil {
		t.Fatalf("RegisterProject after reset: %v", err)
	}
	if reregistered.CheckoutID == registered.CheckoutID {
		t.Errorf("checkout id after reset = %s, want a newly minted identity", reregistered.CheckoutID)
	}
}

func TestFactoryReset_PreservesUnownedProjectsDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Chdir(t.TempDir())

	userFile := filepath.Join(home, "projects", "personal-repo", "README.md")
	writeFile(t, userFile, "keep me")

	plan, err := planFactoryReset()
	if err != nil {
		t.Fatalf("planFactoryReset: %v", err)
	}
	if _, err := executeFactoryReset(plan); err != nil {
		t.Fatalf("executeFactoryReset: %v", err)
	}

	got, err := os.ReadFile(userFile)
	if err != nil {
		t.Fatalf("factory reset removed caller-owned projects directory: %v", err)
	}
	if string(got) != "keep me" {
		t.Fatalf("caller-owned project changed to %q", got)
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

// TestFactoryReset_PreservesUserWorktreesWithNoAFRecords proves the reset never
// removes the user's own manually-created linked worktrees: a repo with a
// user-created worktree and NO AF records is left entirely alone, even when it
// is the current directory (the pre-#1736 cwd fallback would have bulk-removed
// it). AF-created worktrees are still removed (covered by the end-to-end test).
func TestFactoryReset_PreservesUserWorktreesWithNoAFRecords(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "init")
	runGit(t, repo, "branch", "-M", "master")

	// A worktree the USER created by hand, OUTSIDE the AF home, with no AF record.
	userWT := filepath.Join(t.TempDir(), "user-wt")
	runGit(t, repo, "worktree", "add", "-q", "-b", "user-branch", userWT)

	writeFileBytes(t, filepath.Join(home, "config.toml"), []byte("listen_addr = \"127.0.0.1:9000\"\n"))

	// chdir INTO the repo: the removed cwd fallback would have grabbed it here.
	t.Chdir(repo)

	plan, err := planFactoryReset()
	if err != nil {
		t.Fatalf("planFactoryReset: %v", err)
	}
	if len(plan.worktreeTargets) != 0 {
		t.Errorf("worktreeTargets = %v, want none (no AF records)", plan.worktreeTargets)
	}
	if _, ok := plan.repoRoots[repo]; ok {
		t.Error("a repo with no AF records must not be pulled in (cwd fallback dropped)")
	}

	if _, err := executeFactoryReset(plan); err != nil {
		t.Fatalf("executeFactoryReset: %v", err)
	}

	// The user's worktree, its contents, and its branch are untouched.
	if _, err := os.Stat(filepath.Join(userWT, "README.md")); err != nil {
		t.Errorf("user-created worktree was removed: %v", err)
	}
	if !branchExists(repo, "user-branch") {
		t.Error("user-branch must be preserved")
	}
	if !branchExists(repo, "master") {
		t.Error("master must be preserved")
	}
	// The worktree is still registered and functional.
	if out := runGit(t, repo, "worktree", "list", "--porcelain"); !strings.Contains(out, userWT) {
		t.Errorf("user worktree no longer registered:\n%s", out)
	}
}

// TestFactoryReset_ResilientPartialFailure proves a mid-reset step failure does
// NOT abort the wipe: worktrees are still removed, AF branches still pruned,
// state still cleared, and the error is surfaced (Greptile #3). A follow-up run
// after the failure is cleared completes cleanly.
func TestFactoryReset_ResilientPartialFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Chdir(t.TempDir())

	repo, liveWT, reusedWT := seedMockRepo(t, home)
	seedAFState(t, home, repo, liveWT, reusedWT)

	// Plan while everything is well-formed.
	plan, err := planFactoryReset()
	if err != nil {
		t.Fatalf("planFactoryReset: %v", err)
	}

	// Inject a deterministic, uid-independent failure into a mid-reset step:
	// replace tasks.json with a NON-EMPTY directory, so DeleteAllTasks's
	// os.Remove fails with ENOTEMPTY (which not even root can rmdir).
	tasksPath := filepath.Join(home, "tasks.json")
	if err := os.Remove(tasksPath); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(tasksPath, "x"))

	summary, err := executeFactoryReset(plan)
	if err == nil {
		t.Fatal("expected a joined error from the failing task-delete step, got nil")
	}
	if !strings.Contains(err.Error(), "reset tasks") {
		t.Errorf("error should name the failing step: %v", err)
	}

	// Despite the failure, everything else reached a consistent end state.
	assertGone(t, liveWT)
	assertGone(t, reusedWT)
	assertGone(t, filepath.Join(home, "instances"))
	assertGone(t, filepath.Join(home, config.StateFileName))
	if branchExists(repo, "af-session-1") || branchExists(repo, "af-session-2") {
		t.Error("AF branches should be pruned even when a later step failed")
	}
	if !branchExists(repo, "reused-linked") || !branchExists(repo, "my-feature") {
		t.Error("user branches must be preserved")
	}
	if summary.branches != 2 {
		t.Errorf("branches = %d, want 2", summary.branches)
	}

	// Clear the injected failure and re-run: a clean run completes.
	if err := os.RemoveAll(tasksPath); err != nil {
		t.Fatal(err)
	}
	plan2, err := planFactoryReset()
	if err != nil {
		t.Fatalf("re-run planFactoryReset: %v", err)
	}
	if _, err := executeFactoryReset(plan2); err != nil {
		t.Fatalf("re-run should complete cleanly, got: %v", err)
	}
}

// stubResetDaemonHandling replaces runReset's daemon/tmux/autostart touchpoints
// with recorders, restoring the production values on cleanup. It returns the
// recorded event list. The resume recorder also captures whether the wipe had
// already removed the seeded state file, so tests can pin the ordering
// "resume runs only after the wipe" — the whole point of pausing.
func stubResetDaemonHandling(t *testing.T, home string, installed bool, pauseErr error) *[]string {
	t.Helper()
	// fakeDaemonSeams neutralises EVERY daemon/autostart/tmux boundary (and
	// restores them), so the seams this helper does not care about can never
	// fall through to the real thing — notably stopOrphanDaemonsFn, which
	// otherwise runs a host-wide process scan from a unit test.
	fakeDaemonSeams(t)

	var events []string
	stateFile := filepath.Join(home, config.StateFileName)
	wiped := func() string {
		if _, err := os.Stat(stateFile); os.IsNotExist(err) {
			return "wiped"
		}
		return "unwiped"
	}
	autostartInstalledFn = func() bool { return installed }
	// The unit under test is the one for THIS home; the cross-home case has its
	// own test (TestFactoryReset_LeavesOtherHomesAutostartUnitAlone).
	autostartUnitServesHomeFn = func(string) (bool, bool, error) { return installed, installed, nil }
	pauseAutostartUnitFn = func() error {
		events = append(events, "pause:"+wiped())
		return pauseErr
	}
	resumeAutostartUnitFn = func() error {
		events = append(events, "resume:"+wiped())
		return nil
	}
	stopDaemonFn = func() (bool, error) {
		events = append(events, "stop:"+wiped())
		return false, nil
	}
	cleanupTmuxSessionsFn = func() error {
		events = append(events, "tmux:"+wiped())
		return nil
	}
	return &events
}

// runResetForTest runs runReset with the WIPE prompt force-bypassed against a
// throwaway cobra command, returning its combined output.
func runResetForTest(t *testing.T) string {
	t.Helper()
	prevForce := resetForceFlag
	t.Cleanup(func() { resetForceFlag = prevForce })
	resetForceFlag = true

	var out strings.Builder
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runReset(cmd, nil); err != nil {
		t.Fatalf("runReset: %v\noutput:\n%s", err, out.String())
	}
	return out.String()
}

// TestFactoryReset_PausesAutostartAroundWipe pins the fix for the reset/
// autostart race: the service manager relaunches a daemon that exits uncleanly,
// and a daemon relaunched mid-wipe restores sessions from the very records the
// wipe is deleting, ending up with ghost in-memory instances that have no
// storage backing. The unit must be paused BEFORE the daemon stop and resumed
// only AFTER the wipe has finished.
func TestFactoryReset_PausesAutostartAroundWipe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Chdir(t.TempDir())
	writeFile(t, filepath.Join(home, config.StateFileName), `{"schema_version":1}`)

	events := stubResetDaemonHandling(t, home, true, nil)
	out := runResetForTest(t)

	want := []string{"pause:unwiped", "stop:unwiped", "tmux:unwiped", "resume:wiped"}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("daemon handling order = %v, want %v", *events, want)
	}
	// The unit stop already took the supervised daemon down, so the "no managed
	// daemon" hint (with its pkill suggestion) would be noise here.
	if strings.Contains(out, "No managed daemon was stopped") {
		t.Errorf("paused-unit reset must not print the no-managed-daemon hint, got:\n%s", out)
	}
}

func TestFactoryReset_NoAutostartUnitInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Chdir(t.TempDir())
	writeFile(t, filepath.Join(home, config.StateFileName), `{"schema_version":1}`)

	events := stubResetDaemonHandling(t, home, false, nil)
	out := runResetForTest(t)

	want := []string{"stop:unwiped", "tmux:unwiped"}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("daemon handling order = %v, want %v", *events, want)
	}
	if !strings.Contains(out, "No managed daemon was stopped") {
		t.Errorf("without a unit, a no-op stop must keep the no-managed-daemon hint, got:\n%s", out)
	}
}

// A pause failure must not abort the reset (the wipe is the user's explicit,
// confirmed intent), must warn, and must not attempt a resume of a unit that
// was never paused.
func TestFactoryReset_PauseFailureWarnsAndWipes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Chdir(t.TempDir())
	stateFile := filepath.Join(home, config.StateFileName)
	writeFile(t, stateFile, `{"schema_version":1}`)

	events := stubResetDaemonHandling(t, home, true, errors.New("launchctl went sideways"))
	out := runResetForTest(t)

	want := []string{"pause:unwiped", "stop:unwiped", "tmux:unwiped"}
	if !reflect.DeepEqual(*events, want) {
		t.Errorf("daemon handling order = %v, want %v", *events, want)
	}
	if !strings.Contains(out, "launchctl went sideways") {
		t.Errorf("pause failure must be surfaced as a warning, got:\n%s", out)
	}
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Errorf("wipe must proceed despite a pause failure; %s still exists", stateFile)
	}
}

// TestFactoryReset_LegacyNilProvenance_PreservesUserBranches is the #1953
// headline regression: a PRE-EXISTING USER BRANCH SURVIVES RESET.
//
// It drives the real reset path (planFactoryReset → executeFactoryReset →
// git.DeleteLocalBranch) against a real throwaway AGENT_FACTORY_HOME and a real
// git repo with real branches — not branchCreatedByAF in isolation, which skips
// the ExternalWorktree/BranchName gates that decide whether the branch ever
// reaches the deletion plan.
//
// Both legacy shapes that predate branch_created_by_us (2026-04-17) are covered:
//
//   - external_worktree=true + nil — the issue's shape: a `--here`/attach
//     session IS the user's live tree, so AF never owned its branch.
//   - external_worktree=false + nil — the shape the issue does NOT mention: a
//     normal AF linked worktree built by setupFromExistingBranch on a branch the
//     user already had. No external flag protects this one anywhere.
//
// Neither branch may be touched. The AF-created branches of
// TestFactoryReset_WipesEverythingKeepsRepoAndConfig (explicit true) are still
// pruned, which is what keeps this fix from over-correcting into a no-op.
func TestFactoryReset_LegacyNilProvenance_PreservesUserBranches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Chdir(t.TempDir())
	stubResetDaemonHandling(t, home, false, nil)

	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "init")
	runGit(t, repo, "branch", "-M", "master")
	runGit(t, repo, "branch", "legacy-here")   // user branch an attach/--here session reused
	runGit(t, repo, "branch", "legacy-reused") // user branch a linked worktree reused

	legacyReusedWT := filepath.Join(home, "worktrees", "legacy-reused")
	runGit(t, repo, "worktree", "add", "-q", legacyReusedWT, "legacy-reused")

	repoID := config.RepoIDFromRoot(repo)
	recs := []session.InstanceData{
		{ // #1953: attach-to-existing-worktree record, Mar 3 – Apr 17 2026.
			Title:    "legacy-here",
			Path:     repo,
			Liveness: session.LiveReady,
			Worktree: session.GitWorktreeData{
				RepoPath: repo, WorktreePath: repo, ExternalWorktree: true,
				BranchName: "legacy-here", BranchCreatedByUs: nil,
			},
		},
		{ // The unmentioned sibling: setupFromExistingBranch record, pre Apr 17 2026.
			Title:    "legacy-reused",
			Path:     repo,
			Liveness: session.LiveReady,
			Worktree: session.GitWorktreeData{
				RepoPath: repo, WorktreePath: legacyReusedWT, ExternalWorktree: false,
				BranchName: "legacy-reused", BranchCreatedByUs: nil,
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

	plan, err := planFactoryReset()
	if err != nil {
		t.Fatalf("planFactoryReset: %v", err)
	}
	if plan.branchCount() != 0 {
		t.Errorf("branchCount = %d, want 0: unknown provenance must never plan a branch deletion (planned: %v)",
			plan.branchCount(), plan.branches)
	}
	if _, err := executeFactoryReset(plan); err != nil {
		t.Fatalf("executeFactoryReset: %v", err)
	}

	for _, branch := range []string{"legacy-here", "legacy-reused", "master"} {
		if !branchExists(repo, branch) {
			t.Errorf("#1953: reset deleted the user-owned branch %q", branch)
		}
	}
	// The external session's "worktree" IS the user's repo — it must survive.
	if _, err := os.Stat(filepath.Join(repo, "README.md")); err != nil {
		t.Errorf("reset removed the external session's live repo tree: %v", err)
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

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// setupBareCloneWorktree3358 builds the issue's exact repository shape: the
// linked worktree has a bare common directory and no main working tree.
func setupBareCloneWorktree3358(t *testing.T) (parent, bare, worktree string) {
	t.Helper()
	parent = testguard.CanonicalTempDir(t)
	source := filepath.Join(parent, "source")
	bare = filepath.Join(parent, "bare.git")
	worktree = filepath.Join(parent, "worktree")

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
	}
	run(parent, "init", source)
	run(source, "config", "user.email", "test@test.com")
	run(source, "config", "user.name", "Test")
	run(source, "commit", "--allow-empty", "-m", "init")
	run(parent, "clone", "--bare", source, bare)
	run(bare, "worktree", "add", worktree)
	return parent, bare, worktree
}

// TestCreateSessionPreservesAndNamesLegacyBareCloneRows pins the compatibility
// rule for rows written before #3358. They stay under the old parent identity —
// old records discarded the requesting worktree, so moving them could claim an
// unrelated repository — and the create names that retained state explicitly.
func TestCreateSessionPreservesAndNamesLegacyBareCloneRows(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent, _, worktree := setupBareCloneWorktree3358(t)
	legacyID := config.RepoIDFromRoot(parent)
	branchOwned := false
	legacy := session.InstanceData{
		ID:     "legacy-bare-row",
		Title:  "legacy-parent",
		Path:   parent,
		Status: session.Archived,
		Worktree: session.GitWorktreeData{
			RepoPath:          parent,
			WorktreePath:      parent,
			SessionName:       "legacy-parent",
			ExternalWorktree:  true,
			BranchCreatedByUs: &branchOwned,
		},
	}
	if err := appendInstanceData(legacyID, legacy); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	warnings := captureWarnings(t)
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: "new-identity", RepoPath: worktree, Program: "claude", InPlace: true,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rows, err := loadRepoInstanceData(legacyID)
	if err != nil {
		t.Fatalf("reload legacy rows: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != legacy.ID {
		t.Fatalf("legacy rows were moved or overwritten: %+v", rows)
	}
	if warning := warnings.String(); !strings.Contains(warning, "pre-#3358") || !strings.Contains(warning, legacyID) {
		t.Fatalf("compatibility warning did not name retained identity %s: %q", legacyID, warning)
	}
}

// TestCreateSessionAtBareCloneWorktreeUsesBareIdentityAndWorkspace drives the
// real daemon create path. Identity belongs to the bare common directory, but
// --here must provision at the linked worktree the caller requested.
func TestCreateSessionAtBareCloneWorktreeUsesBareIdentityAndWorkspace(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	parent, bare, worktree := setupBareCloneWorktree3358(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	data, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "bare-here",
		RepoPath: worktree,
		Program:  "claude",
		InPlace:  true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected 1 NewInstance call, got %d", len(*seen))
	}
	if got := (*seen)[0].Path; got != worktree {
		t.Fatalf("create workspace = %q, want linked worktree %q", got, worktree)
	}
	if data.Path != worktree {
		t.Fatalf("persisted workspace = %q, want linked worktree %q", data.Path, worktree)
	}

	identityID := config.RepoIDFromRoot(bare)
	rows, err := loadRepoInstanceData(identityID)
	if err != nil {
		t.Fatalf("load identity-keyed rows: %v", err)
	}
	if len(rows) != 1 || rows[0].Title != "bare-here" {
		t.Fatalf("bare identity %s rows = %+v, want bare-here", identityID, rows)
	}
	legacyID := config.RepoIDFromRoot(parent)
	legacyRows, err := loadRepoInstanceData(legacyID)
	if err != nil {
		t.Fatalf("load legacy parent-keyed rows: %v", err)
	}
	if len(legacyRows) != 0 {
		t.Fatalf("new create leaked under legacy parent identity %s: %+v", legacyID, legacyRows)
	}
}

func TestCreateSessionCheckpointKeepsFirstWriteBareIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	_, bare, worktree := setupBareCloneWorktree3358(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	data, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: "bare-worktreeless", RepoPath: worktree, Program: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if data.Worktree.RepoPath != "" {
		t.Fatalf("fixture must create a worktree-less row, got repo path %q", data.Worktree.RepoPath)
	}

	identityID := config.RepoIDFromRoot(bare)
	cmd := exec.Command("git", "-C", bare, "worktree", "remove", "--force", worktree)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remove linked worktree: %v: %s", err, out)
	}
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("recreate former workspace path: %v", err)
	}
	wrongID := config.RepoIDForPath(worktree)
	if wrongID == identityID {
		t.Fatalf("fixture did not change identity after removing linked worktree: %s", identityID)
	}

	if err := manager.SaveInstances(); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}
	identityRows, err := loadRepoInstanceData(identityID)
	if err != nil {
		t.Fatalf("load identity-keyed rows: %v", err)
	}
	if len(identityRows) != 1 || identityRows[0].Title != "bare-worktreeless" {
		t.Fatalf("first-write identity %s rows = %+v", identityID, identityRows)
	}
	wrongRows, err := loadRepoInstanceData(wrongID)
	if err != nil {
		t.Fatalf("load path-derived rows: %v", err)
	}
	if len(wrongRows) != 0 {
		t.Fatalf("checkpoint duplicated fresh row under re-resolved identity %s: %+v", wrongID, wrongRows)
	}
}

// TestEnsureRootAgentsCreatesRootAtBareCloneWorktree is the BOOT-time half of
// the #3361 identity rule: the registered checkout remains the root agent's
// in-place workspace, while its row and daemon key use the bare repository's
// identity. TestEnsureRootAgentsReattributesBareCloneWorktreeCreate
// (rootagent_reattribution_test.go) is its ensure-cadence twin; the two must
// agree on both halves, which is the parity #3299 owns.
func TestEnsureRootAgentsCreatesRootAtBareCloneWorktree(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	_, bare, worktree := setupBareCloneWorktree3358(t)

	project := registerTestProject(t, worktree)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/bare-root\"")
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.ensureRootAgentsAndWait()

	if len(*seen) != 1 {
		t.Fatalf("enabled bare-clone worktree project got %d creates, want 1", len(*seen))
	}
	if got := (*seen)[0].Path; got != worktree {
		t.Fatalf("root workspace = %q, want registered worktree %q", got, worktree)
	}
	if got := (*seen)[0].Program; got != "/opt/bare-root" {
		t.Fatalf("root program = %q, want personal program", got)
	}
	manager.mu.Lock()
	root := manager.instances[daemonInstanceKey(config.RepoIDFromRoot(bare), session.RootSessionTitle)]
	manager.mu.Unlock()
	if root == nil {
		t.Fatalf("root not keyed under bare repository identity %s", config.RepoIDFromRoot(bare))
	}
}

// TestWarnLegacyBareCloneTasksNamesStrandedAutomation pins the #3358 review
// finding on the AUTOMATION half of the identity transition. A task created
// from a bare clone's linked worktree before the fix retained the unrelated
// parent in both ProjectPath and RepoID. After the fix the corrected bare
// project no longer lists it, yet an enabled cron/watch task keeps firing under
// the old identity — and when that parent is itself a repository, every
// delivery keeps creating sessions there, invisible from the project the user
// now works in. Preserved sessions are inert; running automation is not, so the
// transition has to NAME these rather than leave them silently active.
func TestWarnLegacyBareCloneTasksNamesStrandedAutomation(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	parent, _, worktree := setupBareCloneWorktree3358(t)
	legacyID := config.RepoIDFromRoot(parent)

	// Seeded exactly as the pre-#3358 writer produced them: the task binds to the
	// PARENT, which is what the old resolver returned for this worktree.
	if err := task.AddTask(task.Task{
		ID: "legacy-cron", Name: "nightly", Prompt: "go",
		CronExpr: "0 3 * * *", Program: "claude",
		ProjectPath: parent, Enabled: true,
	}); err != nil {
		t.Fatalf("seed enabled legacy task: %v", err)
	}
	if err := task.AddTask(task.Task{
		ID: "legacy-watch", Name: "watcher", Prompt: "go",
		WatchCmd: "true", Program: "claude",
		ProjectPath: parent, Enabled: false,
	}); err != nil {
		t.Fatalf("seed disabled legacy task: %v", err)
	}
	// AddTask re-derives RepoID from ProjectPath, and TODAY that parent resolves
	// to no repository at all — so it cannot mint the pre-#3358 binding. The old
	// resolver returned the parent PATH for this worktree regardless, which is
	// what made RepoIDFromRoot(parent) the stored identity. Restore that binding
	// on the stored row, which is the state a pre-#3358 daemon actually left on
	// disk. legacy-watch keeps the empty RepoID a still-older row has, so the two
	// seeds cover BOTH match paths: the retained id and the ProjectPath fallback.
	bindStoredTaskToLegacyIdentity(t, "legacy-cron", legacyID)

	seeded, err := task.LoadTasks()
	if err != nil {
		t.Fatalf("reload seeded tasks: %v", err)
	}
	byID := map[string]task.Task{}
	for _, seededTask := range seeded {
		byID[seededTask.ID] = seededTask
	}
	if got := byID["legacy-cron"].RepoID; got != legacyID {
		t.Fatalf("fixture: legacy-cron bound to %q, want the legacy parent identity %s", got, legacyID)
	}
	if got := byID["legacy-watch"].RepoID; got != "" {
		t.Fatalf("fixture: legacy-watch must keep an empty RepoID to cover the ProjectPath fallback, got %q", got)
	}
	if got := byID["legacy-watch"].ProjectPath; got != parent {
		t.Fatalf("fixture: legacy-watch ProjectPath = %q, want the legacy parent %s", got, parent)
	}

	repo, err := config.RepoFromPath(worktree)
	if err != nil {
		t.Fatalf("RepoFromPath(%s): %v", worktree, err)
	}
	if repo.ID == legacyID {
		t.Fatalf("fixture: the corrected identity must differ from the legacy parent identity %s", legacyID)
	}

	warnings := captureWarnings(t)

	warnLegacyBareCloneTasks(repo)

	warning := warnings.String()
	for _, want := range []string{
		"legacy-cron", "legacy-watch", // every stranded task is named
		"nightly",                         // and named recognizably, not only by id
		legacyID,                          // the identity they are still bound to
		parent,                            // and its spelling, so the user can find them
		"2 pre-#3358 task(s) (1 enabled)", // enabled ones are the live hazard
		"af tasks list --all",             // an actionable next step
	} {
		if !strings.Contains(warning, want) {
			t.Fatalf("legacy task warning missing %q: %q", want, warning)
		}
	}
}

// TestWarnLegacyBareCloneTasksSilentWithoutStrandedTasks keeps the warning from
// becoming noise every create: a bare clone whose tasks all bind to the
// corrected identity has nothing stranded to report.
func TestWarnLegacyBareCloneTasksSilentWithoutStrandedTasks(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	_, _, worktree := setupBareCloneWorktree3358(t)

	repo, err := config.RepoFromPath(worktree)
	if err != nil {
		t.Fatalf("RepoFromPath(%s): %v", worktree, err)
	}
	if err := task.AddTask(task.Task{
		ID: "current", Name: "current", Prompt: "go",
		CronExpr: "0 3 * * *", Program: "claude",
		ProjectPath: worktree, Enabled: true,
	}); err != nil {
		t.Fatalf("seed current task: %v", err)
	}

	warnings := captureWarnings(t)

	warnLegacyBareCloneTasks(repo)

	if warning := warnings.String(); strings.Contains(warning, "pre-#3358 task(s)") {
		t.Fatalf("no task is stranded, but the transition still warned: %q", warning)
	}
}

// bindStoredTaskToLegacyIdentity rewrites one stored task's repo_id in place.
// AddTask derives that field from ProjectPath, so it is the only way to produce
// the pre-#3358 binding on today's resolver — see its call site.
func bindStoredTaskToLegacyIdentity(t *testing.T, taskID, repoID string) {
	t.Helper()
	path, err := task.MigrateOnLoadPath()
	if err != nil {
		t.Fatalf("tasks path: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tasks file: %v", err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse tasks file: %v", err)
	}
	tasks, ok := file["tasks"].([]any)
	if !ok {
		t.Fatalf("tasks file has no tasks array: %s", raw)
	}
	found := false
	for _, entry := range tasks {
		row, ok := entry.(map[string]any)
		if !ok || row["id"] != taskID {
			continue
		}
		row["repo_id"] = repoID
		found = true
	}
	if !found {
		t.Fatalf("task %s not present in the stored file: %s", taskID, raw)
	}
	updated, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("re-encode tasks file: %v", err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatalf("write tasks file: %v", err)
	}
}

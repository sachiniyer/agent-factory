package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

func seedDisabledTask(id string) error {
	return task.AddTask(task.Task{
		ID:        id,
		CronExpr:  "0 3 * * *",
		Enabled:   false,
		CreatedAt: time.Now(),
	})
}

// TestRunTask_PathTraversalCreatesLockOutsideLocksDir is the regression test
// for issue #575: a user-supplied task ID containing path-traversal sequences
// must not cause a lock file to be created outside ~/.agent-factory/locks/.
// Before the fix RunTask called filepath.Join(lockDir, "task-"+taskID+".lock")
// without validating taskID, so an ID like "foo/../../rogue/pwned" produced a
// lock file in an arbitrary writable directory.
func TestRunTask_PathTraversalCreatesLockOutsideLocksDir(t *testing.T) {
	tmp := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	// Pre-create the rogue parent so an unchecked OpenFile would succeed:
	// without the directory the file open errors out for the wrong reason
	// and the test would pass against the unpatched code too. We want to
	// prove that even when the rogue path is writable, the call refuses.
	rogueDir := filepath.Join(tmp, "rogue")
	if err := os.MkdirAll(rogueDir, 0755); err != nil {
		t.Fatalf("setup rogue dir: %v", err)
	}

	// Same payload as the issue report.
	payload := "foo/../../rogue/pwned"

	err := RunTask(payload)
	if err == nil {
		t.Fatalf("expected error when triggering task with path-traversal ID")
	}

	roguePath := filepath.Join(rogueDir, "pwned.lock")
	if _, statErr := os.Stat(roguePath); statErr == nil {
		t.Fatalf("SECURITY: path traversal allowed lock file creation outside locks directory at %s", roguePath)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error checking rogue lock path: %v", statErr)
	}

	// Also confirm nothing was written into the legitimate locks dir for a
	// task ID that does not correspond to a real task — i.e., the lock is
	// only created after GetTask succeeds.
	locksDir := filepath.Join(tmp, "locks")
	if entries, err := os.ReadDir(locksDir); err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected no lock files for invalid/nonexistent task, found: %v", names)
	}
}

// TestRunTask_RefusesDisabledTask pins that a disabled task can never fire,
// whichever caller (scheduler or `af tasks trigger`) asks.
func TestRunTask_RefusesDisabledTask(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	if err := seedDisabledTask("eeee0001"); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	err := RunTask("eeee0001")
	if err == nil {
		t.Fatalf("expected error running a disabled task")
	}
}

// setupTaskRepo creates a throwaway git repo so config.RepoFromPath inside
// the delivery path succeeds. Returns the repo path.
func setupTaskRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{
		{"init", repo},
		{"-C", repo, "config", "user.email", "test@example.com"},
		{"-C", repo, "config", "user.name", "Test User"},
		{"-C", repo, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo
}

// stubTaskDelivery swaps the daemon RPC indirections used by deliverTaskPrompt
// for in-memory recorders and restores them on cleanup. createSessionForTask
// backs the no-target path (a fresh session per run); deliverPromptForTask
// backs the target_session path (the serialized create-or-send the daemon owns
// since #865). The deliver recorder reports "sent" when the seeded target
// exists and "started" otherwise, mirroring Manager.DeliverPrompt.
func stubTaskDelivery(t *testing.T) (*[]CreateSessionRequest, *[]DeliverPromptRequest) {
	t.Helper()
	var creates []CreateSessionRequest
	var delivers []DeliverPromptRequest
	origCreate := createSessionForTask
	origDeliver := deliverPromptForTask
	createSessionForTask = func(req CreateSessionRequest) (*session.InstanceData, error) {
		creates = append(creates, req)
		title := req.Title
		if title == "" {
			title = req.TitleBase
		}
		return &session.InstanceData{Title: title}, nil
	}
	deliverPromptForTask = func(req DeliverPromptRequest) (string, error) {
		delivers = append(delivers, req)
		repo, err := config.RepoFromPath(req.RepoPath)
		if err != nil {
			return "", err
		}
		exists, err := repoHasSessionTitle(repo.ID, req.Title)
		if err != nil {
			return "", err
		}
		if exists {
			return "sent", nil
		}
		return "started", nil
	}
	t.Cleanup(func() {
		createSessionForTask = origCreate
		deliverPromptForTask = origDeliver
	})
	return &creates, &delivers
}

// seedTargetSession persists a bare instance record so the delivery path sees
// the target session as existing.
func seedTargetSession(t *testing.T, repoPath, title string) {
	t.Helper()
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	raw, err := json.Marshal([]session.InstanceData{{Title: title}})
	if err != nil {
		t.Fatalf("marshal instance data: %v", err)
	}
	if err := config.SaveRepoInstances(repo.ID, raw); err != nil {
		t.Fatalf("SaveRepoInstances: %v", err)
	}
}

// TestRunTask_RefusesWatchTask pins that a watch task can never be fired
// manually: there is no event line to render the prompt with.
func TestRunTask_RefusesWatchTask(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	if err := task.AddTask(task.Task{
		ID:        "ffff0001",
		WatchCmd:  "tail -f log",
		Enabled:   true,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	err := RunTask("ffff0001")
	if err == nil {
		t.Fatalf("expected error running a watch task manually")
	}
	if !strings.Contains(err.Error(), "watch task") {
		t.Fatalf("error should explain the watch-task refusal, got: %v", err)
	}
}

// TestDeliverTaskPrompt_CreatesSessionWithoutTarget pins the historical
// delivery mode: no target_session means a fresh session per run, titled from
// the task name.
func TestDeliverTaskPrompt_CreatesSessionWithoutTarget(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	creates, delivers := stubTaskDelivery(t)

	tsk := &task.Task{ID: "ffff0002", Name: "nightly", Prompt: "do it", CronExpr: "0 3 * * *", ProjectPath: repo, Enabled: true}
	status, err := deliverTaskPrompt(tsk, tsk.Prompt, true)
	if err != nil {
		t.Fatalf("deliverTaskPrompt: %v", err)
	}
	if status != "started" {
		t.Fatalf("status = %q, want started", status)
	}
	if len(*delivers) != 0 {
		t.Fatalf("expected no DeliverPrompt calls, got %d", len(*delivers))
	}
	if len(*creates) != 1 {
		t.Fatalf("expected 1 CreateSession call, got %d", len(*creates))
	}
	got := (*creates)[0]
	if got.TitleBase != "nightly" || got.Title != "" {
		t.Fatalf("create should use TitleBase=nightly (collision-suffixed by the daemon), got Title=%q TitleBase=%q", got.Title, got.TitleBase)
	}
	if got.Prompt != "do it" {
		t.Fatalf("create prompt = %q, want %q", got.Prompt, "do it")
	}
}

// TestDeliverTaskPrompt_SendsIntoExistingTargetSession verifies the new
// delivery mode: with target_session set and the session present, the prompt
// is sent into it (repo-scoped) instead of creating a session.
func TestDeliverTaskPrompt_SendsIntoExistingTargetSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	creates, delivers := stubTaskDelivery(t)
	seedTargetSession(t, repo, "captain")

	tsk := &task.Task{ID: "ffff0003", Name: "gh-issues", Prompt: "Triage: {{line}}", WatchCmd: "watch.sh", TargetSession: "captain", ProjectPath: repo, Enabled: true}
	status, err := deliverTaskPrompt(tsk, "Triage: new issue", true)
	if err != nil {
		t.Fatalf("deliverTaskPrompt: %v", err)
	}
	if status != "sent" {
		t.Fatalf("status = %q, want sent", status)
	}
	if len(*creates) != 0 {
		t.Fatalf("expected no CreateSession calls, got %d", len(*creates))
	}
	if len(*delivers) != 1 {
		t.Fatalf("expected 1 DeliverPrompt call, got %d", len(*delivers))
	}
	got := (*delivers)[0]
	if got.Title != "captain" || got.RepoPath != repo || got.Prompt != "Triage: new issue" {
		t.Fatalf("unexpected DeliverPrompt request: %+v", got)
	}
}

// TestDeliverTaskPrompt_AutoCreatesMissingTargetSession pins the
// Sachin-approved missing-target behavior (#782): route the delivery through
// the daemon's create-or-send path with the task's project_path/program so a
// missing target_session is auto-created and the prompt delivered as its
// initial prompt, mirroring `af sessions send-prompt --create`.
func TestDeliverTaskPrompt_AutoCreatesMissingTargetSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	creates, delivers := stubTaskDelivery(t)

	tsk := &task.Task{ID: "ffff0004", Name: "gh-issues", WatchCmd: "watch.sh", TargetSession: "captain", ProjectPath: repo, Program: "claude", Enabled: true}
	status, err := deliverTaskPrompt(tsk, "new issue #9", true)
	if err != nil {
		t.Fatalf("deliverTaskPrompt: %v", err)
	}
	if status != "started" {
		t.Fatalf("status = %q, want started", status)
	}
	if len(*creates) != 0 {
		t.Fatalf("expected no direct CreateSession calls (delivery is serialized in the daemon), got %d", len(*creates))
	}
	if len(*delivers) != 1 {
		t.Fatalf("expected 1 DeliverPrompt call, got %d", len(*delivers))
	}
	got := (*delivers)[0]
	if got.Title != "captain" {
		t.Fatalf("delivery must use the exact target title, got %q", got.Title)
	}
	if got.RepoPath != repo || got.Program != "claude" || got.Prompt != "new issue #9" {
		t.Fatalf("unexpected DeliverPrompt request: %+v", got)
	}
}

// TestRunTask_CronTaskHonorsTargetSession verifies the scheduled-send mode
// end to end through RunTask: a cron task with target_session sends its
// prompt into the live session and records the "sent" status.
func TestRunTask_CronTaskHonorsTargetSession(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	_, delivers := stubTaskDelivery(t)
	seedTargetSession(t, repo, "captain")

	if err := task.AddTask(task.Task{
		ID:            "ffff0005",
		Name:          "ping",
		Prompt:        "scheduled check-in",
		CronExpr:      "0 3 * * *",
		TargetSession: "captain",
		ProjectPath:   repo,
		Enabled:       true,
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	if err := RunTask("ffff0005"); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if len(*delivers) != 1 || (*delivers)[0].Prompt != "scheduled check-in" {
		t.Fatalf("expected one scheduled delivery, got %+v", *delivers)
	}
	got, err := task.GetTask("ffff0005")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.LastRunStatus != "sent" || got.LastRunAt == nil {
		t.Fatalf("expected LastRunStatus=sent with LastRunAt set, got status=%q at=%v", got.LastRunStatus, got.LastRunAt)
	}
}

// TestRunTask_PersistsFailureStatusOnBadRepo is the #924 regression: a cron
// task whose project path is not a git repo used to return early from RunTask
// before reaching UpdateTaskStatus, so the scheduler only logged the error and
// the TUI showed a stale LastRunStatus forever. RunTask must now record an
// "errored" status on every failure that occurs once it owns the task's run.
func TestRunTask_PersistsFailureStatusOnBadRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	// A real directory that is deliberately NOT a git repo. A prior successful
	// run is simulated by seeding LastRunStatus so we can prove it is replaced,
	// not merely left untouched.
	notARepo := t.TempDir()
	now := time.Now()
	if err := task.AddTask(task.Task{
		ID:            "dddd0001",
		Name:          "broken",
		Prompt:        "do it",
		CronExpr:      "0 3 * * *",
		ProjectPath:   notARepo,
		Enabled:       true,
		CreatedAt:     now,
		LastRunAt:     &now,
		LastRunStatus: "started",
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	err := RunTask("dddd0001")
	if err == nil {
		t.Fatalf("expected RunTask to fail on a non-git project path")
	}
	if !strings.Contains(err.Error(), "not a valid git repository") {
		t.Fatalf("error should explain the git-repo failure, got: %v", err)
	}

	got, gerr := task.GetTask("dddd0001")
	if gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	}
	if !strings.HasPrefix(got.LastRunStatus, "errored") {
		t.Fatalf("LastRunStatus must record the failure, got %q (must not stay stale at \"started\")", got.LastRunStatus)
	}
	if got.LastRunAt == nil || !got.LastRunAt.After(now) {
		t.Fatalf("LastRunAt must advance to the failed run, got %v", got.LastRunAt)
	}
}

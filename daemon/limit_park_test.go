package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The task-runner "park don't fail" tests (#1146 PR4). A task-driven session
// that hits a usage-limit wall during startup must be PARKED (LiveLimitReached,
// prompt stashed for the resume machinery), NOT killed and recorded as a failed
// run. Resuming that parked session re-delivers its OWN prompt so the run
// proceeds to completion — never a bare "continue" that would drop the work.

// limitBannerCreateBackend is a FakeBackend whose pane capture always shows the
// Claude usage-limit banner, so WaitForReady (via the built-in detector) parks
// the create instead of spinning to a readiness timeout. CompleteStart lets
// instance.Start return immediately.
type limitBannerCreateBackend struct {
	*session.FakeBackend
}

func (limitBannerCreateBackend) Preview(*session.Instance) (string, error) {
	return "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)", nil
}

func installLimitBannerBackend(t *testing.T) {
	t.Helper()
	restore := session.SetBackendFactoryForTest(func(_ session.InstanceOptions, _ string) (session.Backend, error) {
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return limitBannerCreateBackend{backend}, nil
	})
	t.Cleanup(restore)
}

// TestCreateSession_ParksOnUsageLimitBanner is the load-bearing PR4 assertion: a
// create whose agent shows a usage-limit banner during startup must return
// SUCCESS with a LiveLimitReached row — the session KEPT (registered + persisted),
// the prompt stashed for resume — not an error that kills the session and records
// the task run as failed.
func TestCreateSession_ParksOnUsageLimitBanner(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installLimitBannerBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	data, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "nightly-task",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "run the nightly report",
	})
	if err != nil {
		t.Fatalf("CreateSession must PARK, not fail, on a usage-limit banner: %v", err)
	}
	if data.Liveness != session.LiveLimitReached {
		t.Fatalf("parked create liveness = %v, want LiveLimitReached", data.Liveness)
	}

	// The session must remain registered (not killed) and limit-blocked.
	key := daemonInstanceKey(repo.ID, "nightly-task")
	manager.mu.Lock()
	inst := manager.instances[key]
	manager.mu.Unlock()
	if inst == nil {
		t.Fatal("parked session must stay registered in the manager, not be killed")
	}
	if !inst.LimitReached() {
		t.Fatalf("registered instance liveness = %v, want LimitReached", inst.GetLiveness())
	}
	// The task prompt must be stashed so resume re-delivers the task's OWN work.
	if inst.Prompt != "run the nightly report" {
		t.Fatalf("stored prompt = %q, want the task prompt so resume re-delivers it", inst.Prompt)
	}

	// Persisted as limit-blocked so the badge + auto-resume scheduler survive a
	// daemon restart.
	raw, err := config.LoadRepoInstances(repo.ID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var stored []session.InstanceData
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	if len(stored) != 1 || stored[0].Liveness != session.LiveLimitReached {
		t.Fatalf("persisted store = %+v, want exactly one LiveLimitReached row", stored)
	}
	if stored[0].Prompt != "run the nightly report" {
		t.Fatalf("persisted InstanceData prompt = %q, want the parked task prompt", stored[0].Prompt)
	}
	var persisted []map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("unmarshal persisted map: %v", err)
	}
	if len(persisted) != 1 || persisted[0]["prompt"] != "run the nightly report" {
		t.Fatalf("persisted prompt = %v, want the parked task prompt to survive daemon restart", persisted)
	}
}

// stubParkedCreate swaps createSessionForTask for one that reports a session
// parked at a usage-limit wall (LiveLimitReached), the shape CreateSession
// returns after PR4's park branch. Restored on cleanup.
func stubParkedCreate(t *testing.T) {
	t.Helper()
	orig := createSessionForTask
	createSessionForTask = func(req CreateSessionRequest) (*session.InstanceData, error) {
		title := req.Title
		if title == "" {
			title = req.TitleBase
		}
		return &session.InstanceData{Title: title, Liveness: session.LiveLimitReached}, nil
	}
	t.Cleanup(func() { createSessionForTask = orig })
}

// TestDeliverTaskPrompt_ParksOnUsageLimit: a no-target task run whose fresh
// session parks must return the distinct parked status — not "started" and never
// an "errored:" value.
func TestDeliverTaskPrompt_ParksOnUsageLimit(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	stubParkedCreate(t)

	tsk := &task.Task{ID: "ffff0010", Name: "nightly", Prompt: "do it", CronExpr: "0 3 * * *", ProjectPath: repo, Enabled: true}
	status, err := deliverTaskPrompt(tsk, tsk.Prompt, true)
	if err != nil {
		t.Fatalf("deliverTaskPrompt must not error on a park: %v", err)
	}
	if status != TaskStatusLimitParked {
		t.Fatalf("status = %q, want %q", status, TaskStatusLimitParked)
	}
	if strings.HasPrefix(status, "errored") {
		t.Fatalf("a parked run must never carry an errored status, got %q", status)
	}
}

// TestRunTask_ParksNotFailedOnUsageLimit is the end-to-end guard: a cron task
// whose run parks records the parked status via the normal success path — the
// deferred failure recorder (which writes "errored: …") never fires, so no
// failure side-effect occurs. LastRunAt is set so the TUI shows the run
// happened and is waiting, not stale and not failed.
func TestRunTask_ParksNotFailedOnUsageLimit(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	stubParkedCreate(t)

	if err := task.AddTask(task.Task{
		ID:          "ffff0011",
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 3 * * *",
		ProjectPath: repo,
		Enabled:     true,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	if err := RunTask("ffff0011"); err != nil {
		t.Fatalf("RunTask must not error on a parked run: %v", err)
	}

	got, err := task.GetTask("ffff0011")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.LastRunStatus != TaskStatusLimitParked {
		t.Fatalf("LastRunStatus = %q, want %q (parked, not failed)", got.LastRunStatus, TaskStatusLimitParked)
	}
	if strings.HasPrefix(got.LastRunStatus, "errored") {
		t.Fatalf("parked run must not record an errored status (no failure side-effect)")
	}
	if got.LastRunAt == nil {
		t.Fatalf("LastRunAt must be set for a parked run")
	}
}

// TestResumeFromLimit_ParkedTaskSessionReDeliversTaskPrompt closes the loop: a
// task session parked at startup (live stall, carrying its task prompt) resumes
// its OWN prompt and clears the limit — parked -> proceeds to completion. A bare
// "continue" here would silently drop the task's work.
func TestResumeFromLimit_ParkedTaskSessionReDeliversTaskPrompt(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	inst := registerStarted(t, manager, repoID, repoPath, "nightly-task", backend, true, session.Running)
	inst.Prompt = "run the nightly report"
	inst.SetLimitReached(time.Now())

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "nightly-task", RepoID: repoID}); err != nil {
		t.Fatalf("resume of a parked task session failed: %v", err)
	}

	_, respawnCalls, prompts := backend.snapshot()
	if respawnCalls != 0 {
		t.Fatalf("a live-stall park needs no re-spawn, got %d", respawnCalls)
	}
	if len(prompts) != 1 || prompts[0] != "run the nightly report" {
		t.Fatalf("parked task must resume its OWN prompt, got %v (a bare \"continue\" would drop the work)", prompts)
	}
	if inst.LimitReached() {
		t.Fatal("resume must clear the limit so the run proceeds to completion")
	}
}

package daemon

import (
	"bytes"
	stdlog "log"
	"sort"
	"strings"
	"testing"
	"time"

	cron "github.com/robfig/cron/v3"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

func sortedScheduledIDs(s *taskScheduler) []string {
	ids := s.scheduledTaskIDs()
	sort.Strings(ids)
	return ids
}

// TestSchedulerReloadRegistersEnabledValidTasks verifies the schedule set
// after a reload: enabled tasks with valid cron expressions are registered,
// disabled tasks are skipped, and an invalid expression (possible via a
// hand-edited tasks.json) is skipped without failing the reload.
func TestSchedulerReloadRegistersEnabledValidTasks(t *testing.T) {
	s := newTaskScheduler()
	s.loadTasks = func() ([]task.Task, error) {
		return []task.Task{
			{ID: "aaaa0001", CronExpr: "0 3 * * *", Enabled: true},
			{ID: "aaaa0002", CronExpr: "*/5 * * * *", Enabled: false},
			{ID: "aaaa0003", CronExpr: "not a cron", Enabled: true},
			{ID: "aaaa0004", CronExpr: "0 0 * * 7", Enabled: true},
			// Watch tasks are event-triggered and belong to the watcher
			// supervisor, never the cron scheduler (#782 phase 2).
			{ID: "aaaa0005", WatchCmd: "tail -f log", Enabled: true},
		}, nil
	}

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	got := sortedScheduledIDs(s)
	want := []string{"aaaa0001", "aaaa0004"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("scheduled IDs = %v, want %v", got, want)
	}
}

// TestSchedulerReloadFollowsTaskCRUD drives the scheduler through the real
// task store: add, disable, and remove operations followed by a Reload must
// be reflected in the schedule set without a daemon restart (#782).
func TestSchedulerReloadFollowsTaskCRUD(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	s := newTaskScheduler()

	mk := func(id string) task.Task {
		return task.Task{ID: id, Prompt: "p", CronExpr: "0 3 * * *", Enabled: true, CreatedAt: time.Now()}
	}

	// Add two tasks.
	if err := task.AddTask(mk("bbbb0001")); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if err := task.AddTask(mk("bbbb0002")); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload after add: %v", err)
	}
	if got := sortedScheduledIDs(s); len(got) != 2 || got[0] != "bbbb0001" || got[1] != "bbbb0002" {
		t.Fatalf("after add: scheduled IDs = %v, want [bbbb0001 bbbb0002]", got)
	}

	// Disable one.
	disabledEnabled := false
	if _, err := task.UpdateTask("bbbb0001", task.TaskUpdate{Enabled: &disabledEnabled}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload after disable: %v", err)
	}
	if got := sortedScheduledIDs(s); len(got) != 1 || got[0] != "bbbb0002" {
		t.Fatalf("after disable: scheduled IDs = %v, want [bbbb0002]", got)
	}

	// Remove the other.
	if err := task.RemoveTask("bbbb0002"); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload after remove: %v", err)
	}
	if got := s.scheduledTaskIDs(); len(got) != 0 {
		t.Fatalf("after remove: scheduled IDs = %v, want []", got)
	}
}

// TestSchedulerFiresDueTask is the firing integration test. robfig/cron has
// no injectable clock, so the test swaps in a seconds-granularity parser and
// an every-second schedule, then waits up to a few real seconds for the
// scheduler to fire the task through its runTask hook.
func TestSchedulerFiresDueTask(t *testing.T) {
	secondsParser := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	fired := make(chan string, 16)
	s := newTaskScheduler()
	s.loadTasks = func() ([]task.Task, error) {
		return []task.Task{{ID: "cccc0001", CronExpr: "* * * * *", Enabled: true}}, nil
	}
	s.parse = func(string) (cron.Schedule, error) {
		return secondsParser.Parse("* * * * * *")
	}
	s.runTask = func(taskID string) { fired <- taskID }

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	s.Start()
	defer s.Stop()

	select {
	case id := <-fired:
		if id != "cccc0001" {
			t.Fatalf("fired task %q, want cccc0001", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("scheduler did not fire a due task within 3s")
	}
}

// TestControlServerReloadTasksRPC verifies the RPC handler path: a reload
// request rebuilds the schedule set from the (stubbed) store, and a server
// without a scheduler reports an error instead of pretending to reload.
func TestControlServerReloadTasksRPC(t *testing.T) {
	s := newTaskScheduler()
	s.loadTasks = func() ([]task.Task, error) {
		return []task.Task{{ID: "dddd0001", CronExpr: "30 2 * * *", Enabled: true}}, nil
	}
	server := &controlServer{scheduler: s}

	var resp ReloadTasksResponse
	if err := server.ReloadTasks(ReloadTasksRequest{}, &resp); err != nil {
		t.Fatalf("ReloadTasks: %v", err)
	}
	if !resp.OK {
		t.Fatalf("expected OK response")
	}
	if got := s.scheduledTaskIDs(); len(got) != 1 || got[0] != "dddd0001" {
		t.Fatalf("scheduled IDs = %v, want [dddd0001]", got)
	}

	none := &controlServer{}
	if err := none.ReloadTasks(ReloadTasksRequest{}, &ReloadTasksResponse{}); err == nil {
		t.Fatalf("expected error from a control server without a scheduler")
	}
}

// TestSchedulerReloadDuplicateTaskIDs pins the #855 fix: a hand-edited
// tasks.json with duplicate task IDs must produce exactly one schedule per ID
// with a logged warning. Pre-fix, the second occurrence overwrote the first
// entry ID in s.entries, leaving an untracked cron entry that kept firing —
// and because cleanup iterates s.entries, no later Reload could remove it, so
// correcting the file did not recover without a daemon restart. The
// s.cron.Entries() counts are the orphan detector: they must match the
// tracked entry set after every reload.
func TestSchedulerReloadDuplicateTaskIDs(t *testing.T) {
	var warnings bytes.Buffer
	prevWarn := aflog.WarningLog
	aflog.WarningLog = stdlog.New(&warnings, "", 0)
	t.Cleanup(func() { aflog.WarningLog = prevWarn })

	s := newTaskScheduler()
	tasks := []task.Task{
		{ID: "ffff0001", CronExpr: "0 3 * * *", Enabled: true},
		{ID: "ffff0001", CronExpr: "*/5 * * * *", Enabled: true},
		{ID: "ffff0002", CronExpr: "0 4 * * *", Enabled: true},
	}
	s.loadTasks = func() ([]task.Task, error) { return tasks, nil }

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload with duplicate IDs: %v", err)
	}
	if got := sortedScheduledIDs(s); len(got) != 2 || got[0] != "ffff0001" || got[1] != "ffff0002" {
		t.Fatalf("scheduled IDs = %v, want [ffff0001 ffff0002]", got)
	}
	if got := len(s.cron.Entries()); got != 2 {
		t.Fatalf("cron entries = %d, want 2 (the duplicate must not schedule a second, untracked entry)", got)
	}
	if !strings.Contains(warnings.String(), `duplicate task ID "ffff0001"`) {
		t.Fatalf("expected a duplicate-ID warning, got log output: %q", warnings.String())
	}

	// Correcting the file must recover fully without a daemon restart.
	tasks = []task.Task{{ID: "ffff0002", CronExpr: "0 4 * * *", Enabled: true}}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload after correction: %v", err)
	}
	if got := sortedScheduledIDs(s); len(got) != 1 || got[0] != "ffff0002" {
		t.Fatalf("after correction: scheduled IDs = %v, want [ffff0002]", got)
	}
	if got := len(s.cron.Entries()); got != 1 {
		t.Fatalf("after correction: cron entries = %d, want 1 (no orphaned entry may survive)", got)
	}

	// And an empty file leaves nothing behind to keep firing.
	tasks = nil
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload to empty: %v", err)
	}
	if got := len(s.cron.Entries()); got != 0 {
		t.Fatalf("after removing all tasks: cron entries = %d, want 0", got)
	}
}

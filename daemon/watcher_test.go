package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
)

// watchRecorder collects deliveries and lifecycle statuses from a supervisor
// under test.
type watchRecorder struct {
	mu       sync.Mutex
	events   []string // "<taskID>:<line>"
	statuses []string // "<taskID>:<status>"
}

func (r *watchRecorder) deliver(taskID, line string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, taskID+":"+line)
	return nil
}

func (r *watchRecorder) setStatus(taskID, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, taskID+":"+status)
}

func (r *watchRecorder) eventsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *watchRecorder) statusesSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.statuses...)
}

// newTestSupervisor builds a supervisor with fast timings, an injected task
// list, and recorder-backed hooks, so no test touches the real task store or
// home directory.
func newTestSupervisor(t *testing.T, tasks func() ([]task.Task, error)) (*watcherSupervisor, *watchRecorder) {
	t.Helper()
	rec := &watchRecorder{}
	logDir := t.TempDir()
	s := newWatcherSupervisor()
	s.loadTasks = tasks
	s.deliver = rec.deliver
	s.setStatus = rec.setStatus
	s.logPath = func(taskID string) (string, error) {
		return filepath.Join(logDir, "task-"+taskID+".log"), nil
	}
	s.shell = "sh"
	s.baseBackoff = 40 * time.Millisecond
	s.maxBackoff = time.Second
	s.stopGrace = 250 * time.Millisecond
	t.Cleanup(s.Stop)
	return s, rec
}

func staticTasks(tasks ...task.Task) func() ([]task.Task, error) {
	return func() ([]task.Task, error) { return tasks, nil }
}

func watchTask(id, cmd, dir string) task.Task {
	return task.Task{ID: id, Name: "watch-" + id, WatchCmd: cmd, ProjectPath: dir, Enabled: true}
}

// waitUntil polls cond until it returns true or the timeout elapses.
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestWatcherDeliversLinesInOrderAndStopsOnExitZero covers the core event
// contract: each stdout line is one event, deliveries arrive in emission
// order with the script env applied, and exit 0 parks the watcher as
// "stopped" without a restart. The trailing unterminated chunk must not
// become an event.
func TestWatcherDeliversLinesInOrderAndStopsOnExitZero(t *testing.T) {
	dir := t.TempDir()
	script := `echo one; echo two; echo "$AF_TASK_ID|$AF_TASK_NAME|$AF_PROJECT_PATH"; pwd; printf incomplete; exit 0`
	s, rec := newTestSupervisor(t, staticTasks(watchTask("aaaa0001", script, dir)))

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 5*time.Second, "watcher to stop", func() bool {
		return len(rec.statusesSnapshot()) > 0
	})

	want := []string{
		"aaaa0001:one",
		"aaaa0001:two",
		"aaaa0001:aaaa0001|watch-aaaa0001|" + dir,
		"aaaa0001:" + dir,
	}
	got := rec.eventsSnapshot()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		// pwd may resolve through symlinks (e.g. /tmp on macOS), so compare
		// the trailing path component for the cwd line.
		if i == 3 {
			if filepath.Base(got[i]) != filepath.Base(want[i]) {
				t.Fatalf("event %d = %q, want cwd %q", i, got[i], want[i])
			}
			continue
		}
		if got[i] != want[i] {
			t.Fatalf("event %d = %q, want %q", i, got[i], want[i])
		}
	}

	if statuses := rec.statusesSnapshot(); len(statuses) != 1 || statuses[0] != "aaaa0001:stopped" {
		t.Fatalf("statuses = %v, want [aaaa0001:stopped]", statuses)
	}

	// Exit 0 is an intentional stop: no respawn, no further deliveries.
	time.Sleep(200 * time.Millisecond)
	if got := rec.eventsSnapshot(); len(got) != len(want) {
		t.Fatalf("watcher restarted after a clean exit: events = %v", got)
	}
	if ids := s.watchingTaskIDs(); len(ids) != 0 {
		t.Fatalf("stopped watcher still reported live: %v", ids)
	}
}

// TestWatcherBackoffAndCrashLoopBreaker pins the failure contract: non-zero
// exits restart with exponential backoff, and the fifth failure inside the
// crash window marks the task "errored" and stops restarting.
func TestWatcherBackoffAndCrashLoopBreaker(t *testing.T) {
	dir := t.TempDir()
	s, rec := newTestSupervisor(t, staticTasks(watchTask("bbbb0001", "echo run; exit 3", dir)))

	start := time.Now()
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 10*time.Second, "crash-loop breaker to trip", func() bool {
		statuses := rec.statusesSnapshot()
		return len(statuses) > 0 && statuses[len(statuses)-1] == "bbbb0001:errored"
	})
	elapsed := time.Since(start)

	events := rec.eventsSnapshot()
	if len(events) != s.crashMaxExits {
		t.Fatalf("expected exactly %d runs before the breaker tripped, got %d (%v)", s.crashMaxExits, len(events), events)
	}
	// Four backoff sleeps separate the five runs: 40+80+160+320ms. A lower
	// bound proves the delays grew without flaking on scheduler jitter.
	if minimum := 600 * time.Millisecond; elapsed < minimum {
		t.Fatalf("five failing runs finished in %s; exponential backoff should make this take at least %s", elapsed, minimum)
	}

	// Errored watchers stay down until the next reload.
	time.Sleep(150 * time.Millisecond)
	if got := rec.eventsSnapshot(); len(got) != s.crashMaxExits {
		t.Fatalf("watcher restarted after the breaker tripped: %v", got)
	}

	// A reload is the re-arm path for errored watchers.
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload after errored: %v", err)
	}
	waitUntil(t, 5*time.Second, "errored watcher to re-arm on reload", func() bool {
		return len(rec.eventsSnapshot()) > s.crashMaxExits
	})
}

// TestWatcherRateLimitDropsExcess pins the flood contract: events over the
// per-minute budget are dropped (not queued, not delivered) and counted.
func TestWatcherRateLimitDropsExcess(t *testing.T) {
	dir := t.TempDir()
	script := `i=1; while [ $i -le 15 ]; do echo "line $i"; i=$((i+1)); done; sleep 60`
	s, rec := newTestSupervisor(t, staticTasks(watchTask("cccc0001", script, dir)))

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 5*time.Second, "rate limit to engage", func() bool {
		return s.droppedEvents("cccc0001") == 5
	})

	events := rec.eventsSnapshot()
	if len(events) != s.eventsPerMinute {
		t.Fatalf("delivered %d events, want the %d/min budget (%v)", len(events), s.eventsPerMinute, events)
	}
	for i, e := range events {
		if want := fmt.Sprintf("cccc0001:line %d", i+1); e != want {
			t.Fatalf("event %d = %q, want %q (in-order delivery)", i, e, want)
		}
	}
}

// TestWatcherTruncatesLongLines pins the 64KB line cap: the first
// maxWatchLineBytes become the event, the rest of the line is discarded, and
// the following line still arrives intact.
func TestWatcherTruncatesLongLines(t *testing.T) {
	dir := t.TempDir()
	script := `head -c 100000 /dev/zero | tr '\0' 'x'; echo; echo next; exit 0`
	s, rec := newTestSupervisor(t, staticTasks(watchTask("dddd0001", script, dir)))

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 5*time.Second, "watcher to finish", func() bool {
		return len(rec.statusesSnapshot()) > 0
	})

	events := rec.eventsSnapshot()
	if len(events) != 2 {
		t.Fatalf("expected 2 events (truncated + next), got %d", len(events))
	}
	long := strings.TrimPrefix(events[0], "dddd0001:")
	if len(long) != maxWatchLineBytes {
		t.Fatalf("truncated event length = %d, want %d", len(long), maxWatchLineBytes)
	}
	if strings.Trim(long, "x") != "" {
		t.Fatalf("truncated event corrupted: %.80q...", long)
	}
	if events[1] != "dddd0001:next" {
		t.Fatalf("event after the long line = %q, want %q", events[1], "dddd0001:next")
	}
}

// TestWatcherGroupKillsGrandchildren is the #769 regression guard for watch
// scripts: a process the script backgrounded with `&` must not outlive the
// watcher — the supervisor SIGKILLs the whole process group when the shell
// exits.
func TestWatcherGroupKillsGrandchildren(t *testing.T) {
	dir := t.TempDir()
	script := `sleep 300 & echo $!; exit 0`
	s, rec := newTestSupervisor(t, staticTasks(watchTask("eeee0001", script, dir)))

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 5*time.Second, "watcher to stop", func() bool {
		return len(rec.statusesSnapshot()) > 0
	})

	events := rec.eventsSnapshot()
	if len(events) != 1 {
		t.Fatalf("expected the grandchild PID as the only event, got %v", events)
	}
	pid, err := strconv.Atoi(strings.TrimPrefix(events[0], "eeee0001:"))
	if err != nil {
		t.Fatalf("event is not a PID: %v", err)
	}
	waitUntil(t, 3*time.Second, "backgrounded grandchild to be killed", func() bool {
		return syscall.Kill(pid, 0) != nil
	})
}

// TestWatcherStopEscalatesToGroupKill pins the shutdown contract: Stop sends
// SIGTERM, and a script that ignores it is SIGKILLed (whole group) after the
// grace instead of blocking daemon shutdown forever.
func TestWatcherStopEscalatesToGroupKill(t *testing.T) {
	dir := t.TempDir()
	script := `trap '' TERM; echo $$; while true; do sleep 0.1; done`
	s, rec := newTestSupervisor(t, staticTasks(watchTask("ffff0001", script, dir)))

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 5*time.Second, "script to report its PID", func() bool {
		return len(rec.eventsSnapshot()) == 1
	})
	pid, err := strconv.Atoi(strings.TrimPrefix(rec.eventsSnapshot()[0], "ffff0001:"))
	if err != nil {
		t.Fatalf("event is not a PID: %v", err)
	}

	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Stop did not return; SIGKILL escalation failed")
	}
	waitUntil(t, 2*time.Second, "TERM-ignoring script to be killed", func() bool {
		return syscall.Kill(pid, 0) != nil
	})
}

// TestWatcherStderrGoesToTaskLog verifies the script contract's logging leg:
// stderr appends to the per-task log file.
func TestWatcherStderrGoesToTaskLog(t *testing.T) {
	dir := t.TempDir()
	script := `echo "diagnostic detail" >&2; exit 0`
	s, rec := newTestSupervisor(t, staticTasks(watchTask("abab0001", script, dir)))
	logDir := t.TempDir()
	s.logPath = func(taskID string) (string, error) {
		return filepath.Join(logDir, "task-"+taskID+".log"), nil
	}

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 5*time.Second, "watcher to stop", func() bool {
		return len(rec.statusesSnapshot()) > 0
	})

	data, err := os.ReadFile(filepath.Join(logDir, "task-abab0001.log"))
	if err != nil {
		t.Fatalf("read task log: %v", err)
	}
	if !strings.Contains(string(data), "diagnostic detail") {
		t.Fatalf("task log missing stderr output, got: %q", data)
	}
}

// TestWatcherReloadReconciles drives the supervisor through config changes:
// disabled tasks stop their watcher, removed tasks stop it, and a changed
// watch command restarts the script with the new config while an unchanged
// live watcher is left alone.
func TestWatcherReloadReconciles(t *testing.T) {
	dir := t.TempDir()
	current := struct {
		mu    sync.Mutex
		tasks []task.Task
	}{}
	setTasks := func(ts ...task.Task) {
		current.mu.Lock()
		current.tasks = ts
		current.mu.Unlock()
	}
	s, rec := newTestSupervisor(t, func() ([]task.Task, error) {
		current.mu.Lock()
		defer current.mu.Unlock()
		return append([]task.Task(nil), current.tasks...), nil
	})

	longRunning := func(marker string) string {
		return fmt.Sprintf(`echo %s; sleep 60`, marker)
	}

	// Two enabled watch tasks plus a cron task and a disabled watch task that
	// must be ignored.
	setTasks(
		watchTask("a1a1a1a1", longRunning("first"), dir),
		watchTask("b2b2b2b2", longRunning("second"), dir),
		task.Task{ID: "c3c3c3c3", CronExpr: "0 3 * * *", Prompt: "p", ProjectPath: dir, Enabled: true},
		task.Task{ID: "d4d4d4d4", WatchCmd: longRunning("never"), ProjectPath: dir, Enabled: false},
	)
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 5*time.Second, "both watchers to start", func() bool {
		return len(rec.eventsSnapshot()) == 2
	})
	if ids := s.watchingTaskIDs(); len(ids) != 2 {
		t.Fatalf("watching IDs = %v, want the two enabled watch tasks", ids)
	}

	// Disable one, change the other's command: the disabled watcher stops,
	// the changed one restarts and emits its new marker.
	disabled := watchTask("a1a1a1a1", longRunning("first"), dir)
	disabled.Enabled = false
	setTasks(disabled, watchTask("b2b2b2b2", longRunning("second-v2"), dir))
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	waitUntil(t, 5*time.Second, "changed watcher to restart", func() bool {
		events := rec.eventsSnapshot()
		return len(events) == 3 && events[2] == "b2b2b2b2:second-v2"
	})
	if ids := s.watchingTaskIDs(); len(ids) != 1 || ids[0] != "b2b2b2b2" {
		t.Fatalf("watching IDs = %v, want [b2b2b2b2]", ids)
	}

	// Remove everything: the supervisor winds down to zero watchers.
	setTasks()
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if ids := s.watchingTaskIDs(); len(ids) != 0 {
		t.Fatalf("watching IDs = %v, want none after removal", ids)
	}
}

// TestDeliverWatchEvent_RendersTemplateAndRecordsStatus exercises the
// production delivery hook against the real task store: {{line}} renders into
// the prompt, the rendered prompt is sent to the target session, and the
// run status lands on the task via the #664 path.
func TestDeliverWatchEvent_RendersTemplateAndRecordsStatus(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupTaskRepo(t)
	_, sends := stubTaskDelivery(t)
	seedTargetSession(t, repo, "captain")

	if err := task.AddTask(task.Task{
		ID:            "cafe0001",
		Name:          "gh-issues",
		Prompt:        "Triage: {{line}}",
		WatchCmd:      "watch.sh",
		TargetSession: "captain",
		ProjectPath:   repo,
		Enabled:       true,
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	if err := deliverWatchEvent("cafe0001", "new issue #9"); err != nil {
		t.Fatalf("deliverWatchEvent: %v", err)
	}
	if len(*sends) != 1 || (*sends)[0].Prompt != "Triage: new issue #9" {
		t.Fatalf("expected one rendered send, got %+v", *sends)
	}
	got, err := task.GetTask("cafe0001")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.LastRunStatus != "sent" || got.LastRunAt == nil {
		t.Fatalf("expected LastRunStatus=sent with LastRunAt set, got %q at %v", got.LastRunStatus, got.LastRunAt)
	}

	// An event for a task that was disabled after the watcher spawned must
	// not deliver.
	got.Enabled = false
	if err := task.UpdateTask(*got); err != nil {
		t.Fatalf("disable task: %v", err)
	}
	if err := deliverWatchEvent("cafe0001", "late event"); err == nil {
		t.Fatalf("expected delivery to a disabled task to error")
	}
	if len(*sends) != 1 {
		t.Fatalf("disabled task still received an event: %+v", *sends)
	}
}

// TestControlServerReloadTasksRPC_ReloadsWatchers verifies the RPC handler
// re-arms both trigger hosts: a watch task added to the store starts its
// watcher on the same poke that refreshes cron entries.
func TestControlServerReloadTasksRPC_ReloadsWatchers(t *testing.T) {
	dir := t.TempDir()
	sched := newTaskScheduler()
	sched.loadTasks = staticTasks(task.Task{ID: "feed0001", CronExpr: "30 2 * * *", Prompt: "p", Enabled: true})
	watchers, rec := newTestSupervisor(t, staticTasks(watchTask("feed0002", "echo hi; sleep 60", dir)))
	server := &controlServer{scheduler: sched, watchers: watchers}

	var resp ReloadTasksResponse
	if err := server.ReloadTasks(ReloadTasksRequest{}, &resp); err != nil {
		t.Fatalf("ReloadTasks: %v", err)
	}
	if !resp.OK {
		t.Fatalf("expected OK response")
	}
	if got := sched.scheduledTaskIDs(); len(got) != 1 || got[0] != "feed0001" {
		t.Fatalf("scheduled IDs = %v, want [feed0001]", got)
	}
	waitUntil(t, 5*time.Second, "watcher to start via RPC reload", func() bool {
		events := rec.eventsSnapshot()
		return len(events) == 1 && events[0] == "feed0002:hi"
	})
}

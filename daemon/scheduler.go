package daemon

import (
	"sync"

	cron "github.com/robfig/cron/v3"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

// taskScheduler evaluates task cron expressions in-process and fires due
// tasks through RunTask. It replaces the per-task systemd/launchd timer units
// that previous versions installed (#782): the daemon is the single always-on
// scheduler host. CLI task CRUD uses daemon RPCs which persist and re-arm
// schedules atomically; TUI CRUD still writes tasks.json directly and pokes
// ReloadTasks (tracked follow-up).
type taskScheduler struct {
	mu      sync.Mutex
	cron    *cron.Cron
	entries map[string]cron.EntryID // task ID → scheduled entry

	// Injection points for tests: loadTasks substitutes fixture task lists,
	// parse allows a seconds-granularity parser so firing tests don't wait a
	// full minute, and runTask observes fires without spawning sessions.
	loadTasks func() ([]task.Task, error)
	parse     func(expr string) (cron.Schedule, error)
	runTask   func(taskID string)
}

func newTaskScheduler() *taskScheduler {
	return &taskScheduler{
		cron:      cron.New(),
		entries:   make(map[string]cron.EntryID),
		loadTasks: task.LoadTasks,
		parse:     task.ParseCron,
		runTask: func(taskID string) {
			if err := RunTask(taskID); err != nil {
				log.WarningLog.Printf("scheduled task %s failed to run: %v", taskID, err)
			}
		},
	}
}

// Start begins evaluating schedules. Each due task runs in its own goroutine
// (robfig/cron's job dispatch), so a slow session start cannot delay other
// schedules; overlapping fires of the same task are serialized by RunTask's
// per-task lock file.
func (s *taskScheduler) Start() {
	s.cron.Start()
}

// Stop halts schedule evaluation. Already-running task fires are left to
// finish on their own goroutines.
func (s *taskScheduler) Stop() {
	s.cron.Stop()
}

// Reload re-reads tasks.json and replaces the scheduled entry set so it
// reflects exactly the currently enabled tasks. A task whose cron expression
// fails to parse, or whose ID duplicates one already scheduled in this pass,
// is skipped with a warning rather than failing the whole reload — the
// user-facing CRUD paths validate before saving, so this only guards
// hand-edited files.
func (s *taskScheduler) Reload() error {
	tasks, err := s.loadTasks()
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for id, entryID := range s.entries {
		s.cron.Remove(entryID)
		delete(s.entries, id)
	}

	// s.entries is keyed by task ID, so a duplicate ID in a hand-edited
	// tasks.json would overwrite the first entry ID and orphan its cron entry:
	// untracked, it keeps firing and no later Reload can remove it until the
	// daemon restarts (#855). Schedule only the first occurrence.
	seen := make(map[string]struct{}, len(tasks))
	for _, t := range tasks {
		if !t.Enabled {
			continue
		}
		// Watch tasks are event-triggered: the watcher supervisor hosts them,
		// not the cron scheduler (#782 phase 2).
		if t.IsWatch() {
			continue
		}
		if _, dup := seen[t.ID]; dup {
			log.WarningLog.Printf("duplicate task ID %q in tasks.json, scheduling only its first occurrence", t.ID)
			continue
		}
		seen[t.ID] = struct{}{}
		schedule, err := s.parse(t.CronExpr)
		if err != nil {
			log.WarningLog.Printf("task %s has an invalid cron expression %q, not scheduling it: %v", t.ID, t.CronExpr, err)
			continue
		}
		taskID := t.ID
		s.entries[taskID] = s.cron.Schedule(schedule, cron.FuncJob(func() {
			s.runTask(taskID)
		}))
	}
	return nil
}

// scheduledTaskIDs returns the IDs of the tasks currently registered with the
// scheduler, for tests and status reporting.
func (s *taskScheduler) scheduledTaskIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	return ids
}

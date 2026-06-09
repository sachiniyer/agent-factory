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
// scheduler host, and task CRUD paths just rewrite tasks.json and ask the
// daemon to Reload via the ReloadTasks RPC.
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
// fails to parse is skipped with a warning rather than failing the whole
// reload — the user-facing CRUD paths validate before saving, so this only
// guards hand-edited files.
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

	for _, t := range tasks {
		if !t.Enabled {
			continue
		}
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

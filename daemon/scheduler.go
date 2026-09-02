package daemon

import (
	"sync"
	"time"

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
	// controlMu serializes task-file mutations and cron/watcher reconciliation
	// across every daemon transport. The gob and HTTP control servers are
	// distinct objects but share this scheduler, so the lock must live here
	// rather than on either transport wrapper.
	controlMu sync.Mutex
	mu        sync.Mutex
	cron      *cron.Cron
	entries   map[string]cron.EntryID // task ID → scheduled entry
	// armed latches on the first completed reload. Before it, an empty entry set
	// means "arming has not run yet", not "nothing is armed" — the daemon accepts
	// control RPCs while it is still warming up, and reporting every task as
	// unarmed in that window would be the fabricated negative this whole feature
	// exists to remove (#3623).
	armed bool

	// Injection points for tests: loadTasks substitutes fixture task lists,
	// parse allows a seconds-granularity parser so firing tests don't wait a
	// full minute, and runTask observes fires without spawning sessions.
	loadTasks func() ([]task.Task, error)
	// applyTasks is a post-load test seam. Production leaves it nil and applies
	// the supplied validated snapshot below; control tests use it to prove that
	// a failure after durable task commit still returns the committed outcome.
	applyTasks func([]task.Task) error
	parse      func(expr string) (cron.Schedule, error)
	runTask    func(taskID string)
}

func newTaskScheduler() *taskScheduler {
	return &taskScheduler{
		cron:      cron.New(),
		entries:   make(map[string]cron.EntryID),
		loadTasks: task.LoadTasks,
		parse:     task.ParseCron,
		runTask: func(taskID string) {
			if err := RunTask(taskID, task.ProjectExpectation{}); err != nil {
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
	return s.reloadTasks(tasks)
}

// reloadTasks applies an already-loaded, validated snapshot. Startup and task
// control use this so cron and watch consume the exact same lifecycle decision
// instead of independently re-reading a legacy binding between the two.
func (s *taskScheduler) reloadTasks(tasks []task.Task) error {
	if s.applyTasks != nil {
		return s.applyTasks(tasks)
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
	s.armed = true
	return nil
}

// armingFor reports the LIVE arming state of one cron task and, when it is
// armed, the instant the scheduler will actually fire it next.
//
// The next-run time is READ OFF the armed entry rather than recomputed from the
// expression, which is the whole point of #3623: the two places that used to
// show a next-fire time recomputed it for display, so a task that was not armed
// at all still rendered a confident "next Sep 02 04:20". A number read from the
// scheduler cannot say that about a task the scheduler does not hold.
//
// The entry lookup happens OUTSIDE s.mu: cron.Entry round-trips through the run
// loop while the cron is running, and no daemon lock should be held across
// another goroutine's turn.
func (s *taskScheduler) armingFor(taskID string) (string, time.Time) {
	s.mu.Lock()
	armed := s.armed
	entryID, scheduled := s.entries[taskID]
	s.mu.Unlock()
	if !armed {
		return task.ArmingUnknown, time.Time{}
	}
	if !scheduled {
		return task.ArmingNotArmed, time.Time{}
	}
	entry := s.cron.Entry(entryID)
	if entry.ID != entryID {
		// Registered here but gone from the cron itself — treat the cron as
		// authoritative; it is the thing that would have to fire.
		return task.ArmingNotArmed, time.Time{}
	}
	return task.ArmingArmed, entry.Next
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

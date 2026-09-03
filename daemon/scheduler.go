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
	entries   map[string]scheduledEntry // task ID → what the cron is holding for it
	// armed latches on the first completed reload. Before it, an empty entry set
	// means "arming has not run yet", not "nothing is armed" — the daemon accepts
	// control RPCs while it is still warming up, and reporting every task as
	// unarmed in that window would be the fabricated negative this whole feature
	// exists to remove (#3623).
	armed bool
	// started latches when the cron's run loop is going. Before it, every entry
	// carries a zero next-fire time simply because nothing has computed one yet —
	// which must not be read as the very different "this entry will never fire"
	// (see armingSnapshot).
	started bool

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

// scheduledEntry pairs a live cron entry with the DEFINITION it was built from.
//
// The expression is what makes an ID-keyed lookup honest. A task write commits
// durably and then reloads the scheduler as a separate, non-transactional step
// (see reloadTaskSchedulesLocked and the committed-outcome error every task RPC
// can return), so the two can disagree: the record on disk carries a new cron
// expression while the cron still holds an entry built from the old one. Keyed
// on the ID alone, that reads as "armed" with the previous schedule's next-fire
// time — a confident, wrong answer about the exact thing this feature exists to
// report, and one doctor would call healthy while the task fired at its former
// times (#3623 review).
type scheduledEntry struct {
	id   cron.EntryID
	expr string
}

func newTaskScheduler() *taskScheduler {
	return &taskScheduler{
		cron:      cron.New(),
		entries:   make(map[string]scheduledEntry),
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
	// Starting the cron and publishing the latch are ONE step, under the lock
	// armingSnapshot takes. Split, they leave a window where started is true and
	// the cron is not: Entries() then takes its not-running path and hands back
	// every entry with a zero Next, the zero-time filter drops all of them, and
	// every enabled cron task on the box reports not-armed during startup — the
	// fabricated negative that filter exists to avoid, reintroduced by the fix
	// for it (#3623 review).
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cron.Start()
	s.started = true
}

// Stop halts schedule evaluation. Already-running task fires are left to
// finish on their own goroutines.
func (s *taskScheduler) Stop() {
	// Symmetric with Start, and for the same reason: a stopped cron's Entries()
	// takes the not-running path, so leaving started latched would make every
	// entry's zero Next read as "will not fire" during shutdown.
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cron.Stop()
	s.started = false
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

	for id, entry := range s.entries {
		s.cron.Remove(entry.id)
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
		s.entries[taskID] = scheduledEntry{
			id: s.cron.Schedule(schedule, cron.FuncJob(func() {
				s.runTask(taskID)
			})),
			expr: t.CronExpr,
		}
	}
	s.armed = true
	return nil
}

// armingSnapshot returns ONE consistent view of what this scheduler is holding:
// task ID → the instant its armed entry will next fire, plus whether arming has
// been observed at all.
//
// Each next-run time is READ OFF the armed entry rather than recomputed from the
// expression, which is the whole point of #3623: the two places that used to
// show a next-fire time recomputed it for display, so a task the scheduler was
// not holding at all still rendered a confident "next Sep 02 04:20". A number
// read from the scheduler cannot say that about a task the scheduler does not
// hold.
//
// It is a SNAPSHOT rather than a per-task lookup for the reason doctor memoizes
// its tmux listing: one response must not describe two different worlds. A
// per-task query would round-trip through the cron's run loop once per task, and
// a reload landing mid-list would leave some rows describing the schedule before
// it and some after.
//
// The second return is false when no reload has completed yet. The control
// socket answers reads during warm-up, and an empty entry map in that window
// means "arming has not run", never "nothing is armed" — see taskScheduler.armed.
//
// Both halves are read under s.mu, and that is load-bearing rather than
// incidental: reloadTasks replaces EVERY entry on each task write, so reading
// s.entries and then the cron without the lock lets a reload land between them —
// the copied IDs are gone from the cron, and a task that is armed under a fresh
// ID is reported not-armed. That false alarm would reach `af doctor` and the
// task list on nothing worse than a concurrent `af tasks update`.
//
// Holding s.mu across cron.Entries means holding it across a round-trip through
// the cron's run loop, which is exactly what reloadTasks already does with
// cron.Remove and cron.Schedule. The run loop never takes s.mu — its job closure
// calls RunTask, which touches no scheduler state — so there is no cycle.
func (s *taskScheduler) armingSnapshot() (map[string]armedEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.armed {
		return nil, false
	}
	// The cron itself is authoritative: it is the thing that would have to fire.
	// An ID registered here but absent there is reported as not armed.
	live := make(map[cron.EntryID]time.Time, len(s.entries))
	for _, entry := range s.cron.Entries() {
		live[entry.ID] = entry.Next
	}
	next := make(map[string]armedEntry, len(s.entries))
	for id, entry := range s.entries {
		at, ok := live[entry.id]
		if !ok {
			continue
		}
		// An entry the running cron holds with a ZERO next-fire time is one it will
		// not fire, and robfig never revisits it: its run loop sorts zero entries
		// last and breaks on the first one it reaches, so that entry is not
		// recomputed for the life of the process (cron.go byTime/run). It gets there
		// when Next() found no match inside its five-year horizon at the moment the
		// entry was created — a long-gap expression such as a leap day in the run-up
		// to a skipped leap year — and it STAYS there even once the horizon has
		// moved on, so this daemon will not run that task until something reloads
		// it. Omitting it from the map reports it as not armed, which is what it
		// functionally is, and points at the daemon restart that actually fixes it
		// (#3623 review).
		//
		// Only once the cron is RUNNING. Before Start every entry's Next is zero
		// because nothing has computed one yet, and reading that as "will not fire"
		// would report every cron task on a starting daemon as broken.
		if s.started && at.IsZero() {
			continue
		}
		next[id] = armedEntry{next: at, expr: entry.expr}
	}
	return next, true
}

// armedEntry is one task's live scheduling, as the caller sees it: when the cron
// will fire it, and the expression that entry was built from so a stale one can
// be told from a current one.
type armedEntry struct {
	next time.Time
	expr string
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

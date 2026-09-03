package daemon

import (
	"context"

	"github.com/sachiniyer/agent-factory/task"
)

// The live half of task schedule health (#3623).
//
// task.WithScheduleHealth derives everything computable from the record alone —
// overdue, missed occurrences — and every read path gets it for free. What it
// cannot know is whether the running daemon is ACTUALLY holding this task: that
// is observable only from inside the process that owns the scheduler and the
// watcher supervisor, which is here.
//
// The distinction matters because "enabled" and "armed" are different claims,
// and the gap between them is where #2929 lived: a task refused at arming stays
// enabled on disk, keeps whatever status its last successful run wrote, and is
// absent from both subsystems. That refusal now has a general answer rather than
// a per-cause one — whatever the reason a task is not armed, the record says so.

// armingFor reports the LIVE arming state of one watch task: whether this
// supervisor is running a watcher for THIS definition of it.
//
// A watch task has no schedule, so it has no next-run time and can never be
// overdue — a command that emits nothing for a month may be working perfectly.
// Its arming state is the whole signal, and it is a real one: a watcher whose
// process crashed past its restart budget has FINISHED, so an enabled watch task
// with a dead watcher reports not-armed instead of looking healthy forever.
//
// It takes the task rather than the id because a live watcher is not
// automatically a watcher for the record on disk. The write commits and the
// supervisor reload is a separate, non-transactional step (the committed-outcome
// error every task RPC can return), so an edited watch_cmd, project_path or name
// can leave the OLD process running under the same id — executing the previous
// command, in the previous directory. `watcherSignature` is what the supervisor
// itself compares on reload to decide whether a watcher is stale, so comparing
// it here makes this answer agree with the reload's own definition of current.
// The cron path makes the same check against its entry's expression (#3623
// review).
func (s *watcherSupervisor) armingFor(t task.Task) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.armed || s.stopped {
		// stopped is the shutdown twin of the scheduler resetting its started
		// latch in Stop, and it matters for the same reason: Stop EMPTIES this map
		// while the control socket deliberately stays open to drain in-flight
		// deliveries, so a read landing there would find no watcher for any task
		// and report every one of them as not armed. A daemon on its way out has
		// observed nothing about steady state (#3623 review).
		return task.ArmingUnknown
	}
	w, ok := s.watchers[t.ID]
	if !ok || w.finished() || w.sig != watcherSignature(t) {
		return task.ArmingNotArmed
	}
	return task.ArmingArmed
}

// withLiveArming layers this daemon's arming observation onto records that
// already carry the pure derivation, and returns them.
//
// It reports the observation for every task, including disabled ones, because
// "disabled and therefore not armed" is a true and unsurprising reading of the
// same field — the surfaces that raise an alarm (af doctor, af tasks list) are
// the ones that decide an unarmed ENABLED task is the interesting case.
//
// A server with no scheduler (the synthetic ones some tests build) leaves every
// task at ArmingUnknown, which is the honest answer: nothing observed it.
func (s *controlServer) withLiveArming(tasks []task.Task) []task.Task {
	// One snapshot for the whole response, so no two rows can describe the
	// schedule on different sides of a reload.
	var scheduled map[string]armedEntry
	observed := false
	if s.scheduler != nil {
		scheduled, observed = s.scheduler.armingSnapshot()
	}
	// A hand-edited store can hold two rows with the same ID, and both subsystems
	// arm only the FIRST (see taskScheduler.reloadTasks and
	// watcherSupervisor.reloadSnapshot). An ID-keyed lookup alone would hand the
	// surviving entry to every matching row, so a skipped duplicate — a different
	// expression that will never execute — would report armed, with the other
	// row's next_run_at. At most one row per ID can be armed, and it is the first.
	seen := make(map[string]bool, len(tasks))
	for i := range tasks {
		duplicate := seen[tasks[i].ID]
		seen[tasks[i].ID] = true
		if tasks[i].IsWatch() {
			if s.watchers == nil {
				continue
			}
			arming := s.watchers.armingFor(tasks[i])
			if duplicate && arming == task.ArmingArmed {
				arming = task.ArmingNotArmed
			}
			tasks[i].Arming = arming
			continue
		}
		if !observed {
			continue
		}
		if duplicate {
			tasks[i].Arming = task.ArmingNotArmed
			continue
		}
		entry, armed := scheduled[tasks[i].ID]
		// The entry has to be for THIS definition. A task write commits durably and
		// reloads the scheduler as a separate step, so a post-commit reload failure
		// leaves the cron holding an entry built from the PREVIOUS expression while
		// the record carries the new one — armed by id, stale in fact, and reported
		// with the old schedule's next-fire time (#3623 review). A mismatch is
		// not-armed, which is true of the definition on disk and points at the
		// reload that fixes it.
		if !armed || entry.expr != tasks[i].CronExpr {
			tasks[i].Arming = task.ArmingNotArmed
			continue
		}
		tasks[i].Arming = task.ArmingArmed
		if next := entry.next; !next.IsZero() {
			// Zero reaches here only before the cron has started, which computes
			// every entry's first fire; the task is armed either way, and inventing
			// a time would be the recomputation this feature exists to remove. A
			// RUNNING cron's zero entry never reaches here at all — armingSnapshot
			// reports it as not armed, because that is what it is.
			at := next
			tasks[i].NextRunAt = &at
		}
	}
	return tasks
}

// taskActor resolves which surface a task mutation came from, for the audit
// trail.
//
// A client that names itself is believed. This is a label, not an
// authentication claim: every caller that can reach these RPCs can already write
// tasks.json, so there is nothing for a lie to buy — and a transport-only answer
// would be WRONG for the surface that matters most here, since the TUI reaches
// the daemon over the same HTTP routes the web UI does.
//
// A client that names nothing is resolved from the transport, which is the best
// available evidence: over HTTP it is some API client (the web UI, a script), and
// over the owner-only control socket it is unknown rather than assumed to be the
// CLI — an older af that predates this field must not be recorded as something
// it never claimed.
func taskActor(ctx context.Context, declared string) task.Actor {
	if actor := task.ParseActor(declared); actor != task.ActorUnknown {
		return actor
	}
	if rpcRequesterIsHTTP(ctx) {
		return task.ActorAPI
	}
	return task.ActorUnknown
}

// liveTaskRecord re-reads one task and annotates it with both halves of the
// derivation, for a mutation response.
//
// A write response that omitted the derived fields would report `overdue`
// absent — indistinguishable from false to every JSON reader — for a task the
// caller may have just disabled or re-pointed. Re-reading after the reload also
// makes `af tasks update --enabled false` return arming "not-armed" as proof the
// disarm actually took effect, rather than as an assertion that it should have.
func (s *controlServer) liveTaskRecord(fallback task.Task) task.Task {
	tasks, err := task.LoadTasks()
	if err != nil {
		return fallback
	}
	for i := range tasks {
		if tasks[i].ID == fallback.ID {
			return s.withLiveArming(tasks[i : i+1])[0]
		}
	}
	return fallback
}

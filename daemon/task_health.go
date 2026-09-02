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
// supervisor holds a watcher for it that has not finished.
//
// A watch task has no schedule, so it has no next-run time and can never be
// overdue — a command that emits nothing for a month may be working perfectly.
// Its arming state is the whole signal, and it is a real one: a watcher whose
// process crashed past its restart budget has FINISHED, so an enabled watch task
// with a dead watcher reports not-armed instead of looking healthy forever.
func (s *watcherSupervisor) armingFor(taskID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.armed {
		return task.ArmingUnknown
	}
	w, ok := s.watchers[taskID]
	if !ok || w.finished() {
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
	for i := range tasks {
		if tasks[i].IsWatch() {
			if s.watchers != nil {
				tasks[i].Arming = s.watchers.armingFor(tasks[i].ID)
			}
			continue
		}
		if s.scheduler == nil {
			continue
		}
		arming, next := s.scheduler.armingFor(tasks[i].ID)
		tasks[i].Arming = arming
		if arming == task.ArmingArmed && !next.IsZero() {
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

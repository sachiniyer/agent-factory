package task

// Carrying a daemon's live arming observation onto records read from disk
// (#3626).
//
// WithScheduleHealth derives everything computable from a record and then
// deliberately CLEARS Arming/NextRunAt, because a disk read has observed
// nothing about the running scheduler. That is right, and it is also why the
// TUI's automations rail — which polls tasks.json every 750ms and never asks
// the daemon — could not report what was actually armed. It fell back to
// evaluating the cron expression, which is the reading that made two tasks dark
// for 18 days look perfectly healthy (#3623).
//
// The daemon's ListTasks response carries the observation. This file is how a
// disk-backed reader adopts it WITHOUT adopting the daemon's copy of the record
// itself: the definition stays the one on disk (repo-scoped, and freshly read),
// and only the two live fields cross over.

// ApplyLiveArming copies the live arming observation from observed onto tasks,
// matched by ID, and returns tasks. Records with no observation are left
// exactly as WithScheduleHealth left them — arming unknown — which is the
// honest answer for "no daemon has reported on this one".
//
// The two lists come from two separate reads of the same file, so they can
// disagree, and an observation adopted across a disagreement is worse than no
// observation at all: it reports a fire time for a schedule that is no longer
// the schedule. sameTrigger is the guard, and it is the client-side twin of the
// checks the daemon already makes internally against its own live state —
// controlServer.withLiveArming compares the armed entry's expression, and
// watcherSupervisor.armingFor compares the watcher's signature. All three ask
// the same question: is this observation ABOUT the definition in front of me?
//
// Duplicate IDs resolve first-wins in both directions, which is the rule the
// scheduler, the watch supervisor and `af tasks list` already agree on (#855):
// only the first row for an ID can be armed, so a later row must not inherit
// the observation the first one earned.
func ApplyLiveArming(tasks, observed []Task) []Task {
	if len(tasks) == 0 || len(observed) == 0 {
		return tasks
	}
	live := make(map[string]Task, len(observed))
	for _, o := range observed {
		if _, dup := live[o.ID]; dup {
			continue
		}
		live[o.ID] = o
	}
	seen := make(map[string]bool, len(tasks))
	for i := range tasks {
		if seen[tasks[i].ID] {
			continue
		}
		seen[tasks[i].ID] = true
		o, ok := live[tasks[i].ID]
		if !ok || !sameTrigger(o, tasks[i]) {
			continue
		}
		tasks[i].Arming = o.Arming
		// Copied together with Arming, never independently: NextRunAt is the
		// armed entry's own next fire, so it is meaningful only alongside the
		// observation that produced it. The daemon leaves it nil for anything it
		// did not observe as armed, so this carries nil in exactly those cases.
		tasks[i].NextRunAt = o.NextRunAt
	}
	return tasks
}

// sameTrigger reports whether an observation about o describes the same trigger
// as t — that is, whether what the daemon armed is what this record configures.
//
// The fields are the ones each subsystem itself keys on, and they differ by kind
// because the subsystems differ. A cron entry holds an expression and fires by
// id, so a renamed cron task is still the same armed schedule. A watcher RUNS a
// command in a directory, so its identity is that command, that directory and
// the name it reports under — watcherSignature's three fields, exactly.
//
// Enabled is compared for both kinds. A task disabled on disk a moment ago is
// not armed however recently the daemon saw it armed, and reporting "next
// Mar 04 09:00" for a task the user just switched off is the same lie in the
// opposite direction.
//
// ProjectPath is compared for both kinds too, and for the cron branch it does a
// job the daemon's own expression check cannot. The daemon skips a duplicated id
// GLOBALLY, first row wins; the caller here has already been filtered to ONE
// repo, so a row the daemon skipped can be the only one this list holds, and it
// is then locally first. Two stores sharing an id and an expression across
// repos would otherwise hand the globally-first row's "armed" and its next-fire
// time to a row the daemon explicitly refused to arm — a rail reporting a task
// as scheduled precisely because it is the duplicate that will never run
// (#3626 review). Local order cannot see that; the definition can.
func sameTrigger(o, t Task) bool {
	if o.IsWatch() != t.IsWatch() || o.Enabled != t.Enabled {
		return false
	}
	if t.IsWatch() {
		return o.WatchCmd == t.WatchCmd && o.ProjectPath == t.ProjectPath && o.Name == t.Name
	}
	return o.CronExpr == t.CronExpr && o.ProjectPath == t.ProjectPath
}

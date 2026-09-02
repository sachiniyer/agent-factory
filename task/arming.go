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
// paired by the ROW each record occupies in tasks.json, and returns tasks.
// Records with no observation are left exactly as WithScheduleHealth left them —
// arming unknown — which is the honest answer for "no daemon has reported on
// this one".
//
// An observation is ABOUT A ROW, so the row is what it is paired on. Both lists
// come from the same file through the same loader — the rail's from
// LoadTasksForKnownRepo, the daemon's from ListTasks — and that loader numbers
// every record with its position (see Task.Ordinal). Repo filtering drops rows
// but changes no survivor's number, so a repo-scoped list still says exactly
// which rows of the store it is holding. Duplicate IDs then resolve exactly:
// there is no first-wins rule to get right, no candidate list to consume, and no
// binding to compare, because the question those answered — WHICH ROW IS THIS —
// is no longer being reconstructed from content (#3680).
//
// It replaces a match that had grown to seven fields and a refusal. Every one of
// those comparisons was added by a real review finding, and every one was right;
// the shape was the problem. ID is the natural key and a hand-edited store can
// duplicate it, so a content match is reconstructing an identity the record does
// not have — and content is not unique, so each fix added a field and the next
// case found a pair that agreed on all of them. The seventh fix was a refusal,
// which is the model reporting that it has run out of information rather than
// that it needs an eighth field.
//
// sameTrigger stays, as the ONE guard, for the case the two reads straddle a
// write that added or removed a row: ordinals then refer to different rows, and
// an observation adopted across that reports a fire time for a schedule that is
// no longer the schedule. It is the client-side twin of the checks the daemon
// already makes internally against its own live state — controlServer's
// withLiveArming compares the armed entry's expression, watcherSupervisor's
// armingFor compares the watcher's signature. All three ask the same question:
// is this observation ABOUT the definition in front of me? Where it cannot tell,
// the answer is unknown, which is the honest one.
func ApplyLiveArming(tasks, observed []Task) []Task {
	if len(tasks) == 0 || len(observed) == 0 {
		return tasks
	}
	live := make(map[int]Task, len(observed))
	// An ordinal claimed by two observations means the list is not one read of
	// one file — the shape a future remote read would produce by merging two
	// stores, which both have a row 3. Neither claimant is trustworthy then, so
	// the row is left unknown rather than resolved by arrival order: a fabricated
	// "armed" is the false clean bill this whole feature exists to remove, and it
	// would be worse coming from a store on another machine. Documented on
	// Task.Ordinal; enforced here so the hazard cannot be reintroduced quietly.
	contested := make(map[int]bool)
	for _, o := range observed {
		// Unnumbered records identify no row. That is the version-skew case — an
		// older daemon's response carries no ordinal field at all — and skipping
		// them leaves every record unknown, which is exactly right for an answer
		// that cannot be attributed.
		if o.Ordinal == unstampedOrdinal {
			continue
		}
		if _, taken := live[o.Ordinal]; taken {
			contested[o.Ordinal] = true
			continue
		}
		live[o.Ordinal] = o
	}
	for i := range tasks {
		if tasks[i].Ordinal == unstampedOrdinal || contested[tasks[i].Ordinal] {
			continue
		}
		o, ok := live[tasks[i].Ordinal]
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
// This is the staleness guard, and the only comparison left. Row identity says
// which row an observation is about; this says whether the row still holds the
// definition it held when the daemon looked. The case it exists for is two reads
// straddling a write that ADDED or REMOVED a row, since that is what moves the
// rows below it and makes one list's row N a different task from the other's.
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
// ProjectPath is compared for both kinds too. For watch tasks it is part of the
// signature above; for cron it is kept as extra evidence that the row still
// holds the same task, which costs nothing and catches a straddling write that
// happened to preserve the expression. What it is NOT any more is a stand-in for
// row identity — that was its job while the match was made on content, and it is
// the ordinal's job now.
func sameTrigger(o, t Task) bool {
	if o.IsWatch() != t.IsWatch() || o.Enabled != t.Enabled {
		return false
	}
	if t.IsWatch() {
		return o.WatchCmd == t.WatchCmd && o.ProjectPath == t.ProjectPath && o.Name == t.Name
	}
	return o.CronExpr == t.CronExpr && o.ProjectPath == t.ProjectPath
}

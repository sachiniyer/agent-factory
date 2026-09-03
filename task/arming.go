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
// The row is qualified by the STORE GENERATION it was read in, and that is what
// answers the straddling write. A row number is only meaningful inside one
// version of a mutable file: a removal shifts every row below it up, and when
// the rows involved are duplicates — same id, same project, same expression,
// which is the store this feature exists for — the two records are identical, so
// no comparison of their content can tell that the reads came from different
// generations. Review found exactly that (#3684). Lists whose generations differ
// pair nothing and every answer stays unknown, which after a task edit costs a
// poll or two of the labelled cron fallback and is the honest reading meanwhile.
//
// sameTrigger is kept behind it. With generations equal the two reads decoded the
// same bytes, so in production it can no longer fire — it is the assertion that
// the pairing really is about the definition in hand, so that a future weakening
// of the generation (a cheaper digest, a partial hash, a generation carried per
// response rather than per record) fails loudly instead of quietly restoring the
// mispairing class. It is also the client-side twin of the checks the daemon
// makes against its own live state: controlServer's withLiveArming compares the
// armed entry's expression, watcherSupervisor's armingFor compares the watcher's
// signature, and all three ask whether this observation is ABOUT the definition
// in front of them.
func ApplyLiveArming(tasks, observed []Task) []Task {
	if len(tasks) == 0 || len(observed) == 0 {
		return tasks
	}
	live := make(map[rowIdentity]Task, len(observed))
	// One identity claimed by two observations means the list is not one read of
	// one store. Neither claimant is trustworthy then, so the row is left unknown
	// rather than resolved by arrival order — a fabricated "armed" is the false
	// clean bill this whole feature exists to remove.
	contested := make(map[rowIdentity]bool)
	for _, o := range observed {
		key, ok := identifyRow(o)
		if !ok {
			continue
		}
		if _, taken := live[key]; taken {
			contested[key] = true
			continue
		}
		live[key] = o
	}
	for i := range tasks {
		key, ok := identifyRow(tasks[i])
		if !ok || contested[key] {
			continue
		}
		o, ok := live[key]
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

// rowIdentity is which row of which version of tasks.json a record came from.
//
// The two halves are one key, never two comparisons, because a row number is
// only meaningful inside the generation it was read in — and a guard that can be
// half-applied is one a later change will half-apply. Keyed on the pair, a
// lookup CANNOT ask "which row" without also asking "of which file" (#3684).
type rowIdentity struct {
	generation string
	row        int
}

// identifyRow reports a record's row identity, and false when it has none.
//
// Either half missing means the record came from no read this can reason
// about — a record built in memory, or one decoded from an older daemon whose
// response carries neither field, which JSON gives back as the zero values. It
// pairs with nothing, which leaves arming UNKNOWN: the answer the rail already
// renders honestly, and the one direction in which being wrong is safe.
func identifyRow(t Task) (rowIdentity, bool) {
	if t.StoreGeneration == "" || t.Ordinal == unstampedOrdinal {
		return rowIdentity{}, false
	}
	return rowIdentity{generation: t.StoreGeneration, row: t.Ordinal}, true
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
//
// ID is compared first, and it has to be STATED here rather than assumed. It is
// not an identity — a hand-edited store can duplicate it, which is the whole
// reason the pairing moved to rows — but it is immutable for a row, since no
// update verb can change it (see TaskUpdate), so an observation about a different
// id is never about this record however well everything else matches. While the
// lookup was keyed on the id that held implicitly; keyed on the row it does not.
//
// The case is a straddling REMOVAL, the twin of the insert above: deleting a row
// shifts every row below it UP, so the rail's row N becomes the daemon's row
// N-1. A replacement task in the same project with the same expression — or a
// watch task with the same signature — agrees on every other field here, and
// adopting its answer would report the deleted task as armed, carrying the
// replacement's next fire. That is a fabricated armed for a task the daemon never
// reported on, which is the exact false clean bill this feature exists to remove
// (#3684 review).
func sameTrigger(o, t Task) bool {
	if o.ID != t.ID {
		return false
	}
	if o.IsWatch() != t.IsWatch() || o.Enabled != t.Enabled {
		return false
	}
	if t.IsWatch() {
		return o.WatchCmd == t.WatchCmd && o.ProjectPath == t.ProjectPath && o.Name == t.Name
	}
	return o.CronExpr == t.CronExpr && o.ProjectPath == t.ProjectPath
}

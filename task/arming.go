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
	// EVERY observation for an id is kept, in order, not just the first. The
	// daemon answers about all repos while this list is one repo's, so a
	// duplicated id can hold the other repo's row AND this one's — and it is
	// precisely this one's, the row the daemon refused to arm, that carries the
	// answer worth having. Keeping only the first discarded it and reported the
	// task as merely unobserved, which suppresses the not-armed warning and falls
	// back to a computed fire time: a task that will never run, rendered as if it
	// were fine (#3626 review).
	live := make(map[string][]Task, len(observed))
	for _, o := range observed {
		live[o.ID] = append(live[o.ID], o)
	}
	// An observation describes ONE row, so it is adopted by at most one — each is
	// consumed when it is taken. That is what lets a duplicated id be reported
	// row by row: the daemon returns an observation per row and marks every one
	// after the first not-armed, so the later rows have an authoritative negative
	// waiting for them. Skipping them wholesale left them UNKNOWN, which hides the
	// "[!] not armed" mark behind a computed fire time for a task the scheduler
	// will never run — the same false clean bill as adopting the wrong row, in the
	// other direction (#3626 review).
	used := make(map[string][]bool, len(live))
	for i := range tasks {
		o, ok := takeMatching(live, used, tasks[i])
		if !ok {
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

// takeMatching claims the earliest unclaimed observation that is about t, and
// marks it claimed. Reports false when none is.
//
// Earliest-first is the daemon's first-wins rule for a duplicated id, resolved
// where it belongs: among the observations that are CANDIDATES for this record,
// not among every row that happens to share its id. Keyed on the id alone it
// threw away the only observation describing the record in hand as soon as
// another repo held the same id.
//
// Claiming is what makes the rule hold for a run of local duplicates. Two rows
// in one repo with the same trigger are both candidates for the same
// observation; without consuming it, the second would take the first's "armed"
// and report a row the scheduler skipped as scheduled. With it, the second falls
// to the daemon's own observation of that second row — which is not-armed, and
// is the answer worth having (#855, and #3626 review).
func takeMatching(live map[string][]Task, used map[string][]bool, t Task) (Task, bool) {
	observed := live[t.ID]
	claimed, ok := used[t.ID]
	if !ok {
		claimed = make([]bool, len(observed))
		used[t.ID] = claimed
	}
	// An unbound record must not GUESS between candidates that disagree about
	// which project they belong to. sameBinding lets a missing RepoID match
	// anything, which is right while the field is being backfilled — but if the
	// disk read landed before the backfill and ListTasks after it, an older row
	// retained to a different repo can still share the reused path, the id and the
	// expression, and "match anything" would take its armed answer over this
	// record's own not-armed one. Tolerate the missing identity only where it
	// cannot pick the wrong row (#3626 review).
	if t.RepoID == "" && ambiguousBinding(observed, claimed, t) {
		return Task{}, false
	}
	for j, o := range observed {
		if claimed[j] || !sameTrigger(o, t) {
			continue
		}
		claimed[j] = true
		return o, true
	}
	return Task{}, false
}

// ambiguousBinding reports whether the unclaimed candidates for t disagree about
// which repo they belong to. Two candidates carrying DIFFERENT non-empty RepoIDs
// are two different tasks that a record with no id of its own cannot choose
// between, and choosing wrong reports a row the daemon skipped as armed.
//
// A single candidate is never ambiguous however it is bound, and neither is a
// set that agrees — those are the ordinary cases the backfill fallback exists
// for, and they keep working.
func ambiguousBinding(observed []Task, claimed []bool, t Task) bool {
	seen := ""
	for j, o := range observed {
		if claimed[j] || o.RepoID == "" || !sameTrigger(o, t) {
			continue
		}
		if seen != "" && seen != o.RepoID {
			return true
		}
		seen = o.RepoID
	}
	return false
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
	if !sameBinding(o, t) {
		return false
	}
	if t.IsWatch() {
		return o.WatchCmd == t.WatchCmd && o.ProjectPath == t.ProjectPath && o.Name == t.Name
	}
	return o.CronExpr == t.CronExpr && o.ProjectPath == t.ProjectPath
}

// sameBinding reports whether two records are bound to the same project, by the
// identity the display scope itself keys on.
//
// ProjectPath alone is not that identity. repoScope.matches treats a RETAINED
// RepoID as authoritative and only falls back to the path, precisely so a task
// survives its project being moved — which means two rows can share a path and
// still belong to different repos, after a path is deleted and reused. The rail
// then displays the new repo's row while an id-and-path match would hand it the
// old repo's armed entry and its fire time (#3626 review).
//
// Compared only when BOTH sides carry one, which is the fallback legacy rows
// need and is not merely a concession to them. RepoID is daemon-backfilled, so
// two reads of the same file can straddle a backfill and disagree about a record
// nobody changed; demanding equality there would spend real observations on
// spurious unknowns. An empty id on either side means "not bound yet", and the
// path comparison the callers already make is then the best available answer.
func sameBinding(o, t Task) bool {
	if o.RepoID == "" || t.RepoID == "" {
		return true
	}
	return o.RepoID == t.RepoID
}

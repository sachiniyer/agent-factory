package task

import (
	"slices"
	"time"

	cron "github.com/robfig/cron/v3"
)

// Schedule health: whether a task is actually firing on its own schedule
// (#3623).
//
// The bug this closes: two enabled hourly cron tasks stopped firing for 18 days
// on a healthy, running daemon, and no surface anywhere could say so. `enabled`
// describes only the current instant — a task toggled off and back on reads
// true and carries no trace. `last_run_status` still said "started", because the
// last run really did start, 18 days ago. And the two places that computed a
// next-fire time recomputed it from the cron expression FOR DISPLAY, so the
// Automations pane cheerfully rendered "next Sep 02 04:20 · last Aug 14 14:20"
// and left the subtraction to the reader. Nothing anywhere compared a task's
// last run against its own schedule.
//
// So the machine does the subtraction now. It is pure derivation from state that
// was already on disk, which is exactly what makes it catch every cause — an
// operator toggle nobody remembers, a daemon that came back without the task, an
// expression that stopped parsing, a cause nobody has thought of yet. Nothing new
// is persisted: a stored "overdue" would be a claim about an instant that has
// already passed by the time anything reads it back.
//
// The premise that makes this load-bearing is #2212's: a box whose owner never
// opens the TUI. A cron task's whole promise is that you stop thinking about it,
// so the machine has to be the one that notices.

// MinSlack is the floor of the lateness window. A schedule shorter than this
// (a per-minute watchdog) would otherwise report overdue on any hiccup in
// firing, delivery, or clock, so the floor buys every schedule at least five
// minutes of grace before it is called late.
const MinSlack = 5 * time.Minute

// MaxMissedOccurrences caps MissedOccurrences, and with it the work one
// derivation may do: the count is produced by stepping the schedule forward one
// occurrence at a time, so a per-minute task dark for a year would otherwise
// walk half a million steps on a read path the TUI hits every 750ms. The count
// SATURATES rather than truncating the verdict — Overdue stays true and
// Saturated says the number is a floor, because "at least 10000 missed" and
// "exactly 526000 missed" call for the same action.
const MaxMissedOccurrences = 10000

// Arming states. These are the LIVE observation — whether the running daemon's
// scheduler or watcher supervisor actually holds an entry for this task — and
// not a re-derivation from the record, which is the whole point: the number a
// user reads must reflect what is armed, not what the cron expression says
// ought to be.
//
// ArmingUnknown is the zero value and means NOT OBSERVED: no daemon answered, so
// nothing is known either way. It must never be read as "not armed" — that is
// the fabricated negative this repo keeps re-learning, and here it would report
// every task on a box with a stopped daemon as broken.
const (
	ArmingUnknown  = ""
	ArmingArmed    = "armed"
	ArmingNotArmed = "not-armed"
)

// ScheduleHealth is the derived answer for one task at one instant. It is
// computed at read time and never stored; Task carries the two fields every
// surface shows (Overdue, MissedOccurrences) and this struct carries the rest
// for callers that report on them, such as `af doctor`.
type ScheduleHealth struct {
	// Overdue reports that the task's last run precedes its most recent
	// scheduled occurrence by more than the slack window.
	Overdue bool
	// MissedOccurrences counts the scheduled fires strictly after the reference
	// point (the last run, or creation for a task that has never run) up to and
	// including now. Zero when the task is not overdue: within the slack window
	// a pending occurrence is not a missed one.
	MissedOccurrences int
	// Saturated reports that MissedOccurrences hit MaxMissedOccurrences and is
	// therefore a floor, not an exact count. It rides the task record too
	// (MissedOccurrencesCapped): a consumer that reads "10000" and cannot tell it
	// from an exact count has been handed a number that is quietly wrong.
	Saturated bool
	// Unschedulable reports an expression that PARSES but matches no date — the
	// February 31st class. Nothing can be derived about lateness for such a task
	// because it has no occurrences at all, and reporting the absence of a verdict
	// as health would be the worst answer available: the task is armed, the
	// scheduler holds an entry with a zero next-fire time, and it will never run.
	Unschedulable bool
	// OldestMissedAt is the first occurrence the task did not run — the instant
	// the silence began. Zero when the task is not overdue.
	OldestMissedAt time.Time
	// Slack is the window that was allowed before calling the task late, kept so
	// a report can explain its own verdict.
	Slack time.Duration
}

// DeriveScheduleHealth answers "is this task firing on its schedule?" at now.
//
// Only an ENABLED CRON task can be overdue:
//
//   - A watch task has no schedule to be late against. It fires when its command
//     emits a line, which may legitimately be never, so lateness is not defined
//     for it at all — its supervision state is the signal, and the watcher
//     supervisor already owns that.
//   - A disabled task is not expected to fire. Whether disabling it was
//     INTENDED is exactly what the audit trail answers (see task/audit.go); it
//     is not something this derivation can know.
//   - An expression that no longer parses derives nothing. The task is still
//     broken, and the arming state says so: the scheduler skips what it cannot
//     parse, so the record reports arming "not-armed" while enabled.
//
// The verdict is one Next() call, not a scan. The task is overdue exactly when
// some scheduled occurrence lands at or before now and more than slack after the
// reference point, and the first such occurrence is Next(ref+slack): if that is
// at or before now the schedule has moved on without the task, and if it is
// after now the task is merely pending. That equivalence is what keeps this
// affordable on a 750ms poll — the walk below runs only for a task already known
// to be overdue.
func DeriveScheduleHealth(t Task, now time.Time) ScheduleHealth {
	if !t.Enabled || t.IsWatch() {
		return ScheduleHealth{}
	}
	sched, err := ParseCron(t.CronExpr)
	if err != nil {
		return ScheduleHealth{}
	}
	ref, ok := scheduleReference(t)
	if !ok {
		return ScheduleHealth{}
	}
	slack, ok := slackFor(sched, ref)
	if !ok {
		// The expression parses but has no occurrences within robfig's five-year
		// horizon: it can never fire, which is a report, not a silence.
		return ScheduleHealth{Unschedulable: true}
	}
	oldest := sched.Next(ref.Add(slack))
	if oldest.IsZero() || oldest.After(now) {
		return ScheduleHealth{Slack: slack}
	}
	// The oldest MISSED occurrence is the first one after the reference point,
	// which is not the same as the first one past the slack window: a task last
	// run at 14:20 and silent since has missed 15:20 even though the window
	// forgave it until 15:20:00. Report the instant the silence began.
	missedFrom := sched.Next(ref)
	count, saturated := countOccurrences(sched, ref, now)
	return ScheduleHealth{
		Overdue:           true,
		MissedOccurrences: count,
		Saturated:         saturated,
		OldestMissedAt:    missedFrom,
		Slack:             slack,
	}
}

// scheduleReference is the instant lateness is measured from: the LATEST of the
// last run, the last time the task was enabled, and — for a task that has never
// run — its creation.
//
// The never-run case is worth getting right on its own: a task created and then
// never armed has no LastRunAt at all, and treating that as "nothing to compare
// against" would make the most broken task the one that reports healthiest.
//
// A RE-ENABLE is a fresh start, exactly as a first run is, and this is where the
// audit trail earns its keep beyond being something a person reads. Without it,
// a task deliberately paused and later switched back on reports every occurrence
// it missed WHILE INTENTIONALLY OFF: #3623's own tasks, disabled 2026-08-14 and
// re-enabled 2026-09-01, would have read "overdue · missed 432" from the moment
// they came back until their next fire. That is a true statement about the past
// and a useless one about the present — the operator knows why it was off, and
// burying the signal under misses they caused on purpose is how a warning gets
// trained into noise.
//
// The trail is BOUNDED (AuditLimit), so an enable old enough to have fallen off
// the window leaves the reference where it was before. That direction is the
// safe one: it can only make the verdict more eager, never hide a task that has
// genuinely stopped. Tasks written before the trail existed carry none, and
// behave exactly as they did.
//
// A record with no timestamp at all (hand-edited, or written before CreatedAt was
// populated) derives nothing rather than measuring from the zero time, which
// would report every such task as millions of occurrences overdue.
func scheduleReference(t Task) (time.Time, bool) {
	var ref time.Time
	if t.LastRunAt != nil && !t.LastRunAt.IsZero() {
		ref = *t.LastRunAt
	} else if !t.CreatedAt.IsZero() {
		ref = t.CreatedAt
	}
	if restarted, ok := lastScheduleRestartAt(t); ok && restarted.After(ref) {
		ref = restarted
	}
	if ref.IsZero() {
		return time.Time{}, false
	}
	return ref, true
}

// lastScheduleRestartAt returns when the schedule the task is on NOW began,
// from its audit trail: the most recent moment it was switched on, or its cron
// expression changed.
//
// Both are the same event in the only sense that matters here — the schedule a
// task is being judged against started then, so occurrences before it are not
// misses the task owes. The enable case is the deliberate-pause one. The
// expression case is its twin and just as sharp: retiming a daily task to run
// every minute at 23:00 would otherwise be measured against this morning's run
// and report a day's worth of per-minute misses that happened before that
// schedule existed.
//
// An ordinary edit — the prompt, the target session, the program — restarts
// nothing: the schedule did not move.
//
// An entry with no timestamp is SKIPPED rather than answered with. A
// hand-edited or truncated row must not shadow a real restart behind it, and
// measuring from the zero time would report the task as millions of occurrences
// overdue.
func lastScheduleRestartAt(t Task) (time.Time, bool) {
	for i := len(t.Audit) - 1; i >= 0; i-- {
		e := t.Audit[i]
		if e.At.IsZero() {
			continue
		}
		if e.Action == AuditEnabled || slices.Contains(e.Fields, "cron_expr") {
			return e.At, true
		}
	}
	return time.Time{}, false
}

// slackFor is how late a fire may be before the task counts as overdue: one full
// period of its own schedule, or MinSlack, whichever is larger.
//
// One period is the right unit because it is the only lateness that is
// indistinguishable from ordinary operation: an hourly task whose last run was
// 59 minutes ago has simply not reached its next occurrence yet, and one whose
// last run was 61 minutes ago has skipped one. Scaling with the schedule is what
// lets the same rule serve a per-minute watchdog and a monthly report.
//
// The period is measured AT THE REFERENCE POINT, and as the WIDER of the two
// gaps around it, because both halves have a false positive waiting behind them:
//
//   - The gap the task must bridge next (Next(ref) - ref) is the one that
//     matters for an irregular schedule. A weekdays-at-9 task whose last run was
//     a Friday is owed the whole weekend; judged by the Monday-to-Tuesday period
//     instead, it would be called overdue the instant Monday 09:00 arrived —
//     before the fire it is waiting for had any chance to happen.
//   - That gap alone is too tight when the reference is not itself an
//     occurrence, which is exactly the never-run case: a task created at 09:00
//     on a ":20 past" schedule would get 20 minutes of slack and be called
//     overdue at 09:25, five minutes into its first ever run.
//
// Taking the larger of the two is right for both, and reduces to the plain
// period for the ordinary case where the last run sits on an occurrence.
func slackFor(sched cron.Schedule, ref time.Time) (time.Duration, bool) {
	first := sched.Next(ref)
	if first.IsZero() {
		return 0, false
	}
	second := sched.Next(first)
	if second.IsZero() {
		return 0, false
	}
	slack := second.Sub(first)
	if gap := first.Sub(ref); gap > slack {
		slack = gap
	}
	if slack < MinSlack {
		slack = MinSlack
	}
	return slack, true
}

// countOccurrences counts the schedule's fires in (from, now], saturating at
// MaxMissedOccurrences. The second return says whether it saturated.
func countOccurrences(sched cron.Schedule, from, now time.Time) (int, bool) {
	count := 0
	for at := sched.Next(from); !at.IsZero() && !at.After(now); at = sched.Next(at) {
		count++
		if count >= MaxMissedOccurrences {
			return count, true
		}
	}
	return count, false
}

// WithScheduleHealth populates the derived read-time fields on every task in the
// slice and returns it, so a read path can wrap its load in one call. It is the
// PURE half of the derivation — everything computable from the record alone.
// The live arming half (Arming/NextRunAt) can only be observed by the process
// that holds the scheduler, so the daemon layers it on top of this.
//
// Callers on a write path must not use it: derived fields never reach disk, and
// saveTasks strips them precisely so a mistake here cannot persist one.
func WithScheduleHealth(tasks []Task, now time.Time) []Task {
	for i := range tasks {
		health := DeriveScheduleHealth(tasks[i], now)
		tasks[i].Overdue = health.Overdue
		tasks[i].MissedOccurrences = health.MissedOccurrences
		tasks[i].MissedOccurrencesCapped = health.Saturated
	}
	return tasks
}

// stripDerived clears every read-time field. saveTasks calls it on the way to
// disk so the "derived, never persisted" rule is enforced by the write path
// itself rather than by the discipline of each caller — a loaded record now
// always carries these fields, so any future path that loads and re-saves one
// would otherwise store them.
func (t *Task) stripDerived() {
	t.Overdue = false
	t.MissedOccurrences = 0
	t.MissedOccurrencesCapped = false
	t.NextRunAt = nil
	t.Arming = ""
}
